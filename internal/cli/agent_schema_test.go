package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forgeplex/appkit/internal/workspace"
)

func TestAgentSchemaRequiresExplicitDatabaseAuthorization(t *testing.T) {
	t.Setenv("TEST_DATABASE_URL", "postgres://never-use:secret@127.0.0.1:1/forbidden")
	var out, diagnostics bytes.Buffer
	err := runAgent("plan", []string{"schema", "-dir", filepath.Join(t.TempDir(), "missing"), "-dsn", "another-secret"}, &out, &diagnostics)
	var exit *agentExit
	if !errors.As(err, &exit) || exit.code != 2 || !strings.Contains(out.String(), "-allow-temp-db") {
		t.Fatalf("must reject before database/workspace access: %v %s", err, out.String())
	}
	if strings.Contains(out.String()+diagnostics.String(), "secret") {
		t.Fatal("authorization error leaked DSN")
	}
	t.Setenv("TEST_DATABASE_URL", "")
	out.Reset()
	err = runAgent("plan", []string{"schema", "-allow-temp-db"}, &out, &diagnostics)
	if !errors.As(err, &exit) || exit.code != 2 || !strings.Contains(out.String(), "TEST_DATABASE_URL") {
		t.Fatalf("missing DSN not rejected: %v %s", err, out.String())
	}
}

func TestAgentSchemaCLIRealDatabaseOfflineApply(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	root := agentDomain(t)
	if err := os.WriteFile(filepath.Join(root, ".appkit.yml"), []byte("version: 1\ndomain: sample\nmodule: example.com/sample\npartitioned: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "db/migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "db/migrations/0001.sql"), []byte("CREATE TABLE orders (id bigint PRIMARY KEY);"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, diagnostics bytes.Buffer
	if err := runAgent("plan", []string{"schema", "-dir", root, "-allow-temp-db", "-timeout", "1m"}, &out, &diagnostics); err != nil {
		t.Fatalf("schema plan: %v %s %s", err, out.String(), diagnostics.String())
	}
	plan, err := workspace.ParsePlan(out.Bytes())
	if err != nil || diagnostics.Len() != 0 || len(plan.DirectoryGuards()) != 2 {
		t.Fatalf("invalid/noisy plan: %v %s", err, diagnostics.String())
	}
	if strings.Contains(out.String(), dsn) {
		t.Fatal("DSN appears in serialized plan")
	}
	if _, err := os.Stat(filepath.Join(root, "db/SCHEMA.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plan wrote output: %v", err)
	}
	planPath := filepath.Join(t.TempDir(), "schema-plan.json")
	if err := os.WriteFile(planPath, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	// Application and replay must use only captured bytes, not reconnect to a
	// database or re-run any SQL. There is no database flag on apply.
	t.Setenv("TEST_DATABASE_URL", "postgres://unreachable:invalid@127.0.0.1:1/absent")
	for _, want := range []workspace.ApplyDisposition{workspace.ApplyCommitted, workspace.ApplyReplayed} {
		out.Reset()
		if err := runAgent("apply", []string{"-dir", root, "-plan", planPath}, &out, &diagnostics); err != nil {
			t.Fatalf("offline apply: %v %s", err, out.String())
		}
		var result struct {
			OK   bool           `json:"ok"`
			Data agentApplyData `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.OK || result.Data.Disposition != want {
			t.Fatalf("apply result: %v %s", err, out.String())
		}
	}
	data, err := os.ReadFile(filepath.Join(root, "db/SCHEMA.md"))
	if err != nil || !strings.Contains(string(data), "logical-template") {
		t.Fatalf("missing logical template: %v", err)
	}
}
