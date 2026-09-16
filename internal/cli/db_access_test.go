package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	m, err := dbaccess.LoadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	wantSQL, err := dbaccess.RenderSQL(*m)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	diagnostics.Reset()
	if err := dbAccess([]string{"render", "-manifest", manifest}, &out, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), wantSQL) || diagnostics.Len() != 0 {
		t.Fatalf("stdout render polluted: stdout=%q diagnostics=%q", out.String(), diagnostics.String())
	}

	sqlPath := filepath.Join(dir, "0003_access.sql")
	if err := dbAccess([]string{"render", "-manifest", manifest, "-out", sqlPath}, &out, &diagnostics); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(sqlPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got&0o022 != 0 {
		t.Fatalf("generated migration is group/other writable: mode=%o", got)
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

func TestDBAccessRenderRequiresExistingParentAndPreservesMode(t *testing.T) {
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
	missing := filepath.Join(dir, "missing", "0003_access.sql")
	if err := dbAccess([]string{"render", "-manifest", manifest, "-out", missing}, &out, &diagnostics); err == nil {
		t.Fatal("created a missing parent directory")
	}
	if _, err := os.Stat(filepath.Dir(missing)); !os.IsNotExist(err) {
		t.Fatalf("missing parent was created: %v", err)
	}

	privateDir := filepath.Join(dir, "private-migrations")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := dbAccess([]string{"render", "-manifest", manifest, "-out", filepath.Join(privateDir, "0003_access.sql")}, &out, &diagnostics); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(privateDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("parent mode changed to %o, want 700", got)
	}

	target := filepath.Join(dir, "unexpected.sql")
	if err := os.WriteFile(target, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(privateDir, "0004_access.sql")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if err := dbAccess([]string{"render", "-manifest", manifest, "-out", link}, &out, &diagnostics); err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Fatalf("did not reject output symlink explicitly: %v", err)
	}
	data, err = os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "preserve" {
		t.Fatalf("symlink target changed: %q", data)
	}

	realParent := filepath.Join(dir, "real-parent")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(dir, "linked-parent")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if err := dbAccess([]string{"render", "-manifest", manifest, "-out", filepath.Join(linkedParent, "0005_access.sql")}, &out, &diagnostics); err == nil || !strings.Contains(err.Error(), "符号链接目录") {
		t.Fatalf("did not reject symlink parent explicitly: %v", err)
	}
}

func TestWriteNewAccessSQLPublishesAtomicallyWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "0001_access.sql")
	bodies := [][]byte{bytes.Repeat([]byte("a"), 1<<20), bytes.Repeat([]byte("b"), 1<<20)}
	errs := make(chan error, len(bodies))
	var ready sync.WaitGroup
	ready.Add(len(bodies))
	start := make(chan struct{})
	for _, body := range bodies {
		body := body
		go func() {
			ready.Done()
			<-start
			errs <- writeNewAccessSQL(path, body)
		}()
	}
	ready.Wait()
	close(start)
	var successes int
	for range bodies {
		if err := <-errs; err == nil {
			successes++
		} else if !strings.Contains(err.Error(), "拒绝覆盖") {
			t.Fatalf("unexpected concurrent publish error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful publishers=%d, want 1", successes)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bodies[0]) && !bytes.Equal(got, bodies[1]) {
		t.Fatalf("published partial content: %d bytes", len(got))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("temporary files remained after publish: %v", entries)
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
