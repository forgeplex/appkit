package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forgeplex/appkit/internal/dbaccess"
)

func TestDBAccessValidateRenderAndCheck(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "access.yaml")
	data, err := os.ReadFile("../dbaccess/testdata/access.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, diagnostics bytes.Buffer
	if err := dbAccess([]string{"validate", "-manifest", manifest}, &out, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "service=admin_api") || diagnostics.Len() != 0 {
		t.Fatalf("unexpected output: %q diagnostics=%q", out.String(), diagnostics.String())
	}

	sqlPath := filepath.Join(dir, "0003_access.sql")
	if err := dbAccess([]string{"render", "-manifest", manifest, "-out", sqlPath}, &out, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if err := dbAccess([]string{"render", "-manifest", manifest, "-out", sqlPath}, &out, &diagnostics); err == nil {
		t.Fatal("overwrote an existing migration")
	}
	if err := dbAccess([]string{"check", "-manifest", manifest, "-sql", sqlPath}, &out, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sqlPath, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := dbAccess([]string{"check", "-manifest", manifest, "-sql", sqlPath}, &out, &diagnostics); err == nil {
		t.Fatal("accepted drifted SQL")
	}
}

// Compile-time check that testdata remains a valid public example rather than
// a CLI-only fixture with weaker parsing.
func TestDBAccessExampleParsesWithPackage(t *testing.T) {
	f, err := os.Open("../dbaccess/testdata/access.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := dbaccess.Parse(f); err != nil {
		t.Fatal(err)
	}
}
