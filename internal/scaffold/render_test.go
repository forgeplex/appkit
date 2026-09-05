package scaffold

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRenderScaffoldParityAndReadOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opts   Options
		render func(Options) (map[string][]byte, error)
		write  func(Options, io.Writer) error
	}{
		{"domain", Options{Name: "sample"}, RenderDomain, Domain},
		{"tenant", Options{Name: "sample", Tenant: true}, RenderDomain, Domain},
		{"partitioned", Options{Name: "sample", Partitioned: true}, RenderDomain, Domain},
		{"partitioned tenant", Options{Name: "sample", Partitioned: true, Tenant: true}, RenderDomain, Domain},
		{"system", Options{Name: "sample"}, RenderSystem, System},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			opts.Module, opts.AppkitVersion = "example.com/sample", "v0.7.3"
			opts.WorkflowRef = "0123456789abcdef0123456789abcdef01234567"
			parent := t.TempDir()
			opts.Dir = filepath.Join(parent, "no-output")
			before := opts
			files, err := tc.render(opts)
			if err != nil {
				t.Fatal(err)
			}
			if opts != before {
				t.Fatal("render mutated caller options")
			}
			if entries, err := os.ReadDir(parent); err != nil || len(entries) != 0 {
				t.Fatalf("render wrote output directory: %v %v", entries, err)
			}
			// Rendering does not depend on an output directory being fresh, or
			// even on it being a directory at all.
			if err := os.WriteFile(opts.Dir, []byte("existing user file"), 0o600); err != nil {
				t.Fatal(err)
			}
			again, err := tc.render(opts)
			if err != nil || !reflect.DeepEqual(files, again) {
				t.Fatalf("render depends on filesystem or is nondeterministic: %v", err)
			}
			got, err := os.ReadFile(opts.Dir)
			if err != nil || string(got) != "existing user file" {
				t.Fatalf("render changed unrelated user file: %v", err)
			}
			opts.Dir = t.TempDir()
			if err := tc.write(opts, nil); err != nil {
				t.Fatal(err)
			}
			names := listFiles(t, opts.Dir)
			if len(names) != len(files) {
				t.Fatalf("rendered %d files, writer created %d", len(files), len(names))
			}
			for _, name := range names {
				got, err := os.ReadFile(filepath.Join(opts.Dir, filepath.FromSlash(name)))
				if err != nil || !bytes.Equal(got, files[name]) {
					t.Errorf("%s differs between pure renderer and normal scaffold: %v", name, err)
				}
			}
			assertRendered(t, opts.Dir)
			assertGoParses(t, opts.Dir)
			assertGofmt(t, opts.Dir)
			files["go.mod"][0] = '!'
			delete(files, "README.md")
			last, err := tc.render(opts)
			if err != nil || !reflect.DeepEqual(last, again) {
				t.Fatalf("caller mutation leaked into later rendering: %v", err)
			}
		})
	}
}

func TestRenderScaffoldValidationWithoutWrites(t *testing.T) {
	for _, render := range []func(Options) (map[string][]byte, error){RenderDomain, RenderSystem} {
		for _, opts := range []Options{{}, {Name: "Bad-Name"}, {Name: "internal"}, {Name: "sample", Module: "bad path"}} {
			parent := t.TempDir()
			opts.Dir = filepath.Join(parent, "out")
			if _, err := render(opts); err == nil {
				t.Fatalf("invalid options accepted: %+v", opts)
			}
			if entries, err := os.ReadDir(parent); err != nil || len(entries) != 0 {
				t.Fatalf("invalid render wrote files: %v %v", entries, err)
			}
		}
	}
}
