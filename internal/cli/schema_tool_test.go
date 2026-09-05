package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSchemaToolCommand(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".appkit.yml"), []byte("version: 1\nkind: domain\ndomain: probe\nmodule: example.com/probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runSchemaTool([]string{"-dir", dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "internal/postgres/schematool/main.go")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"db/schema.sql", "db/migrations", "go.mod", "sqlc.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, path)); !os.IsNotExist(err) {
			t.Fatalf("installer unexpectedly modified %s: %v", path, err)
		}
	}
	if err := runSchemaTool([]string{"-dir", dir, "extra"}); err == nil {
		t.Fatal("accepted extra argument")
	}
	if err := os.WriteFile(filepath.Join(dir, ".appkit.yml"), []byte("version: 1\nkind: system\nmodule: example.com/probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runSchemaTool([]string{"-dir", dir}); err == nil {
		t.Fatal("installed in composition repository")
	}
}
