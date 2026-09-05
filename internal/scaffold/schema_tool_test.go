package scaffold

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSchemaToolInstallation(t *testing.T) {
	dir := t.TempDir()
	files, err := SchemaToolFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Fatalf("unexpected toolkit file count: %d", len(files))
	}
	if err := InstallSchemaTool(dir); err != nil {
		t.Fatal(err)
	}
	if err := InstallSchemaTool(dir); err != nil {
		t.Fatalf("reinstall generated toolkit: %v", err)
	}
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("installed %s differs: %v", rel, err)
		}
	}
	path := filepath.Join(dir, "internal/postgres/schematool/main.go")
	if err := os.WriteFile(path, []byte("package main // handwritten\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InstallSchemaTool(dir); err == nil {
		t.Fatal("overwrote handwritten toolkit")
	}
}

func TestSchemaToolRefusesDirectoryLink(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "internal")); err != nil {
		t.Fatal(err)
	}
	if err := InstallSchemaTool(dir); err == nil {
		t.Fatal("followed output directory symlink")
	}
}

func TestSchemaToolScaffoldModes(t *testing.T) {
	for _, mode := range []struct{ partitioned, tenant bool }{{}, {tenant: true}, {partitioned: true}, {partitioned: true, tenant: true}} {
		files, err := RenderDomain(Options{Name: "probe", AppkitVersion: "(devel)", Partitioned: mode.partitioned, Tenant: mode.tenant, WorkflowRef: "0123456789abcdef0123456789abcdef01234567"})
		if err != nil {
			t.Fatal(err)
		}
		want := "./internal/postgres/schematool -domain probe"
		if mode.partitioned {
			want += " -partitioned"
		}
		if !bytes.Contains(files["Makefile"], []byte(want+" -check")) {
			t.Fatalf("mode=%+v: wrong tool invocation", mode)
		}
		if !bytes.Contains(files["internal/postgres/schematool/main.go"], []byte(schemaToolHeader)) {
			t.Fatal("missing source ownership header")
		}
		if !bytes.Contains(files["sqlc.yaml"], []byte(`schema: "db/migrations"`)) {
			t.Fatal("optional snapshot changed default sqlc input")
		}
		for _, path := range []string{"db/schema.sql", "db/schema.lock.json"} {
			if _, exists := files[path]; exists {
				t.Fatalf("scaffold fabricated a snapshot without database replay: %s", path)
			}
		}
	}
}
