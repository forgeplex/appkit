package gen

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestYAMLSourceRenderersUseCapturedBytes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		input  string
		golden string
		render func(string, []byte) ([]byte, error)
		write  func(string, string) error
	}{
		{"events", "testdata/events.yaml", "genfixture/events.gen.go", RenderEventsSource, Events},
		{"errors", "testdata/codes.yaml", "genfixture/codes.gen.go", RenderErrorsSource, Errors},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, err := os.ReadFile(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(tc.golden)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "source.yaml")
			if err := os.WriteFile(path, []byte("version: ["), 0o644); err != nil {
				t.Fatal(err)
			}
			before := snapshotContractTree(t, dir)
			captured := bytes.Clone(input)
			got, err := tc.render(path, captured)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("captured rendering differs from golden: %v", err)
			}
			if !bytes.Equal(captured, input) {
				t.Fatal("renderer mutated input bytes")
			}
			if after := snapshotContractTree(t, dir); !reflect.DeepEqual(before, after) {
				t.Fatal("renderer wrote to source directory")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			got[0] = '!'
			again, err := tc.render(path, captured)
			if err != nil || !bytes.Equal(again, want) {
				t.Fatalf("renderer depended on source file or previous result: %v", err)
			}
			out := filepath.Join(dir, "out.go")
			if err := tc.write(tc.input, out); err != nil {
				t.Fatal(err)
			}
			written, err := os.ReadFile(out)
			if err != nil || !bytes.Equal(written, again) {
				t.Fatalf("file generator differs from pure renderer: %v", err)
			}
		})
	}
}

func TestYAMLSourceRendererDiagnostics(t *testing.T) {
	const name = "captured/source.yaml"
	for _, tc := range []struct {
		label  string
		render func(string, []byte) ([]byte, error)
		input  string
		want   string
	}{
		{"events syntax", RenderEventsSource, "version: [", name + ": 解析 yaml"},
		{"errors syntax", RenderErrorsSource, "version: [", name + ": 解析 yaml"},
		{"events version", RenderEventsSource, "version: 2", name + ": 不支持的 version 2"},
		{"errors version", RenderErrorsSource, "version: 2", name + ": 不支持的 version 2"},
		{"events name", RenderEventsSource, "version: 1\npackage: p\nevents:\n  - {name: bad, topic: e}\n", name + ":4:"},
		{"errors code", RenderErrorsSource, "version: 1\npackage: p\ncodes:\n  - {code: bad, status: 400, message: bad}\n", name + ":4:"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			_, err := tc.render(name, []byte(tc.input))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("diagnostic = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRenderWrapSourcesMatchesExistingGenerator(t *testing.T) {
	files := make(map[string][]byte)
	entries, err := os.ReadDir("genfixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		files[entry.Name()], err = os.ReadFile(filepath.Join("genfixture", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile("genfixture/wrap.gen.go")
	if err != nil {
		t.Fatal(err)
	}
	got, err := RenderWrapSources(files, "Service", "greet")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("snapshot wrapper differs from golden: %v", err)
	}
	out := filepath.Join(t.TempDir(), "wrap.gen.go")
	if err := Wrap("genfixture", "Service", "greet", out); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(written, got) {
		t.Fatalf("file wrapper differs from snapshot wrapper: %v", err)
	}
}

func TestRenderWrapSourcesOrderingFilteringAndOwnership(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "b.go")
	if err := os.WriteFile(path, []byte("invalid actual source"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		filepath.Join(dir, "a.go"):      []byte("package p\ntype Other interface{}"),
		filepath.Join(dir, "a_test.go"): []byte("invalid ignored test source"),
		filepath.Join(dir, "a.yaml"):    []byte("invalid ignored non-Go source"),
		path:                            []byte("package p\nimport (\"context\"; u \"net/url\")\ntype Service interface { Call(context.Context, u.Values) (u.Values, error) }"),
		filepath.Join(dir, "z.go"):      []byte("invalid unvisited source after interface"),
	}
	snapshot := cloneSources(files)
	before := snapshotContractTree(t, dir)
	want, err := RenderWrapSources(files, "Service", "example")
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"wrappedService) Call", `u "net/url"`, "contract.Call", "u.Values"} {
		if !bytes.Contains(want, []byte(fragment)) {
			t.Errorf("wrapper missing %q", fragment)
		}
	}
	if !reflect.DeepEqual(files, snapshot) {
		t.Fatal("renderer modified source map or buffers")
	}
	if after := snapshotContractTree(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatal("renderer changed source directory")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		// Rebuilding a map makes insertion/iteration order independent of the
		// stable lexical source-selection order used by the renderer.
		got, err := RenderWrapSources(cloneSources(files), "Service", "example")
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("wrapper is nondeterministic or reread source: %v", err)
		}
		got[0] = '!'
	}
	files[filepath.Join(dir, "a.go")] = []byte("invalid first eligible source")
	if _, err := RenderWrapSources(files, "Service", "example"); err == nil || !strings.Contains(err.Error(), "a.go") {
		t.Fatalf("an invalid earlier file must fail before selecting b.go: %v", err)
	}
}

func TestRenderWrapSourcesDoesNotReadNilSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "valid.go")
	if err := os.WriteFile(path, []byte("package p\nimport \"context\"\ntype S interface{ M(context.Context) error }"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := RenderWrapSources(map[string][]byte{path: nil}, "S", "sys")
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("nil captured bytes should fail parsing without reading valid file: %v", err)
	}
}

func TestRenderWrapSourcesValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		iface  string
		system string
		want   string
	}{
		{"missing iface", "", "", "sys", "-iface"},
		{"invalid system", "", "S", "My-Sys", "-system"},
		{"missing interface", "type Other interface{}", "S", "sys", "未找到接口 S"},
		{"not interface", "type S struct{}", "S", "sys", "不是接口类型"},
		{"generic", "type S[T any] interface{ M(context.Context) error }", "S", "sys", "泛型"},
		{"signature", "type S interface{ M() error }", "S", "sys", "首参必须是 context.Context"},
		{"import", "type S interface{ M(context.Context, ghost.Request) error }", "S", "sys", `限定符 "ghost"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string][]byte{"snapshot.go": []byte("package p\nimport \"context\"\n" + tc.source)}
			_, err := RenderWrapSources(files, tc.iface, tc.system)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("diagnostic = %v, want %q", err, tc.want)
			}
		})
	}
}

func cloneSources(files map[string][]byte) map[string][]byte {
	clone := make(map[string][]byte, len(files))
	for name, data := range files {
		clone[name] = bytes.Clone(data)
	}
	return clone
}
