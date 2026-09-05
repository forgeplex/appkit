package gen

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRenderContractMatchesGoldenWithoutWrites(t *testing.T) {
	dir := t.TempDir()
	input, err := os.ReadFile("testdata/contract.yaml")
	if err != nil {
		t.Fatal(err)
	}
	inPath := filepath.Join(dir, "contract.yaml")
	if err := os.WriteFile(inPath, input, 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotContractTree(t, dir)
	got, err := RenderContract(inPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(goldenFiles["contract"]) {
		t.Fatalf("generated %d files, want %d", len(got), len(goldenFiles["contract"]))
	}
	for _, golden := range goldenFiles["contract"] {
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got[filepath.Base(golden)], want) {
			t.Errorf("rendered %s differs from checked-in golden", golden)
		}
	}
	if after := snapshotContractTree(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatal("render changed the input tree")
	}
	got["service.gen.go"][0] = '!'
	delete(got, "client.gen.go")
	again, err := RenderContract(inPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 5 || again["service.gen.go"][0] != '/' {
		t.Fatal("caller changes leaked into a later render")
	}
}

func TestRenderContractSourceUsesCapturedBytes(t *testing.T) {
	input, err := os.ReadFile("testdata/contract.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want, err := RenderContract("testdata/contract.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sourceName := filepath.Join(dir, "contract.yaml")
	// The path has changed since the caller captured input. A second read would
	// fail or silently generate content that no longer matches the plan digest.
	if err := os.WriteFile(sourceName, []byte("version: ["), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotContractTree(t, dir)
	captured := bytes.Clone(input)
	got, err := RenderContractSource(sourceName, captured)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("byte renderer differs from rendering the captured source file")
	}
	if !bytes.Equal(captured, input) {
		t.Fatal("byte renderer changed caller input")
	}
	if after := snapshotContractTree(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatal("byte renderer modified files")
	}
	if err := os.Remove(sourceName); err != nil {
		t.Fatal(err)
	}
	again, err := RenderContractSource(sourceName, captured)
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("source name must not require a readable file: %v", err)
	}
	if _, err := os.Stat(sourceName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("byte renderer created a source file: %v", err)
	}
}

func TestRenderContractSourceDiagnostics(t *testing.T) {
	const sourceName = "captured/contracts/example.yaml"
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"version: [", sourceName + ": 解析 yaml"},
		{"version: 2\n", sourceName + ": 不支持的 version 2"},
		{"version: 1\npackage: p\nsystem: s\nmethods:\n  - {name: bad, path: /b, doc: d}\n", sourceName + ":5:"},
	} {
		_, err := RenderContractSource(sourceName, []byte(tc.input))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("diagnostic = %v, want %q", err, tc.want)
		}
	}
}

func TestCheckContractReadOnly(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*testing.T, string)
		want    []ContractDrift
	}{
		{name: "unchanged"},
		{
			name: "missing and modified",
			prepare: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, "client.gen.go")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "service.gen.go"), []byte("// manually changed\n"), 0o640); err != nil {
					t.Fatal(err)
				}
			},
			want: []ContractDrift{{Path: "client.gen.go", Reason: "missing"}, {Path: "service.gen.go", Reason: "stale"}},
		},
		{
			name: "empty generated file",
			prepare: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, "openapi.yaml"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: []ContractDrift{{Path: "openapi.yaml", Reason: "stale"}},
		},
		{
			name: "directory instead of file",
			prepare: func(t *testing.T, dir string) {
				p := filepath.Join(dir, "wrap.gen.go")
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(p, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: []ContractDrift{{Path: "wrap.gen.go", Reason: "not_regular"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := Contract("testdata/contract.yaml", dir); err != nil {
				t.Fatal(err)
			}
			// The checker owns only generated filenames, and need not parse or
			// rewrite handwritten source while deriving the wrapper.
			if err := os.WriteFile(filepath.Join(dir, "handwritten.go"), []byte("unfinished handwritten source\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.prepare != nil {
				tc.prepare(t, dir)
			}
			before := snapshotContractTree(t, dir)
			err := CheckContract("testdata/contract.yaml", dir)
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var drift *ContractDriftError
				if !errors.Is(err, ErrContractDrift) || !errors.As(err, &drift) {
					t.Fatalf("want typed drift error, got %v", err)
				}
				if !reflect.DeepEqual(drift.Files, tc.want) {
					t.Fatalf("drift = %#v, want %#v", drift.Files, tc.want)
				}
				again := CheckContract("testdata/contract.yaml", dir)
				if again == nil || again.Error() != err.Error() {
					t.Fatalf("drift diagnostic is unstable: %v / %v", err, again)
				}
			}
			if after := snapshotContractTree(t, dir); !reflect.DeepEqual(before, after) {
				t.Fatalf("check modified target files: before %#v, after %#v", before, after)
			}
		})
	}
}

func TestCheckContractDoesNotCreateTarget(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "missing", "contract")
	before := snapshotContractTree(t, parent)
	err := CheckContract("testdata/contract.yaml", dir)
	want := "APPKIT_GEN_CONTRACT_DRIFT: missing client.gen.go; missing openapi.yaml; missing server.gen.go; missing service.gen.go; missing wrap.gen.go"
	if err == nil || err.Error() != want {
		t.Fatalf("diagnostic = %v, want %q", err, want)
	}
	if after := snapshotContractTree(t, parent); !reflect.DeepEqual(before, after) {
		t.Fatal("check created missing target directories")
	}
}

func TestCheckContractInvalidInputDoesNotChangeTarget(t *testing.T) {
	for _, input := range []string{"version: [", "version: 2\n", "version: 1\npackage: p\nsystem: s\nmethods: []\n"} {
		t.Run(input, func(t *testing.T) {
			dir := t.TempDir()
			inPath := filepath.Join(dir, "contract.yaml")
			if err := os.WriteFile(inPath, []byte(input), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := Contract("testdata/contract.yaml", dir); err != nil {
				t.Fatal(err)
			}
			before := snapshotContractTree(t, dir)
			err := CheckContract(inPath, dir)
			if err == nil || errors.Is(err, ErrContractDrift) || !strings.Contains(err.Error(), inPath) {
				t.Fatalf("invalid input should report its validation error, got %v", err)
			}
			if after := snapshotContractTree(t, dir); !reflect.DeepEqual(before, after) {
				t.Fatal("invalid input changed target")
			}
		})
	}
}

func TestCheckContractRejectsSymlinkOutput(t *testing.T) {
	dir := t.TempDir()
	if err := Contract("testdata/contract.yaml", dir); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "service.gen.go")
	external := filepath.Join(t.TempDir(), "service.gen.go")
	if err := os.Rename(target, external); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, target); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	before, err := os.ReadFile(external)
	if err != nil {
		t.Fatal(err)
	}
	err = CheckContract("testdata/contract.yaml", dir)
	if err == nil || err.Error() != "APPKIT_GEN_CONTRACT_DRIFT: not_regular service.gen.go" {
		t.Fatalf("symlink diagnostic = %v", err)
	}
	after, err := os.ReadFile(external)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("check changed external symlink target")
	}
}

type contractFileState struct {
	Mode    fs.FileMode
	ModTime time.Time
	Data    string
}

func snapshotContractTree(t *testing.T, dir string) map[string]contractFileState {
	t.Helper()
	state := make(map[string]contractFileState)
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var content []byte
		if info.Mode().IsRegular() {
			content, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		state[path] = contractFileState{Mode: info.Mode(), ModTime: info.ModTime(), Data: string(content)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}
