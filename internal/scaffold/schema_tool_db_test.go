package scaffold

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/forgeplex/appkit/internal/archcheck"
	"golang.org/x/mod/modfile"
)

// Exercise the actual copied tool against a published framework dependency,
// including the transition from migration-based sqlc input to an owned snapshot.
// Every database operation runs through that tool's random scratch database;
// TEST_DATABASE_URL is only its administrative connection, never a replay target.
func TestSchemaToolScaffoldDatabaseAdoption(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL is required for scratch-database scaffold adoption")
	}
	goCommand, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("Go toolchain required for scaffold adoption: %v", err)
	}
	env := schemaAdoptionEnv()
	for _, mode := range []struct {
		name                string
		tenant, partitioned bool
	}{
		{name: "plain"},
		{name: "tenant", tenant: true},
		{name: "partitioned", partitioned: true},
		{name: "both", tenant: true, partitioned: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			name := "schemaprobe" + mode.name
			dir := filepath.Join(t.TempDir(), name)
			if err := Domain(Options{Name: name, Module: "example.com/" + name, Dir: dir, AppkitVersion: "v0.9.2", WorkflowRef: "b6d2b12dd1d3f37e3d9eee06b903e4968be8a708", Tenant: mode.tenant, Partitioned: mode.partitioned}, nil); err != nil {
				t.Fatal(err)
			}
			original := schemaAdoptionModule(t, dir)
			runSchemaAdoptionCommand(t, goCommand, dir, env, "mod", "tidy")
			assertSchemaAdoptionModule(t, dir, original)
			initialConfig := readFile(t, dir, "sqlc.yaml")
			if !strings.Contains(initialConfig, `schema: "db/migrations"`) || !strings.Contains(initialConfig, "db/schema.sql") {
				t.Fatal("fixture must retain migration input and the snapshot-adoption comment")
			}
			initial := runSchemaAdoptionCommand(t, goCommand, dir, env, "test", "-json", "-count=1", "./internal/postgres/schematool", "-run", "^TestRepositorySnapshot$")
			assertSchemaAdoptionTestAction(t, initial, "skip")

			generate := []string{"run", "./internal/postgres/schematool", "-domain", name}
			if mode.partitioned {
				generate = append(generate, "-partitioned")
			}
			runSchemaAdoptionCommand(t, goCommand, dir, env, generate...)
			for _, path := range []string{"db/schema.sql", "db/schema.lock.json"} {
				if info, err := os.Stat(filepath.Join(dir, path)); err != nil || info.Size() == 0 {
					t.Fatalf("tool did not produce nonempty %s: %v", path, err)
				}
			}
			if strings.Count(initialConfig, `schema: "db/migrations"`) != 1 {
				t.Fatal("ambiguous sqlc input in generated repository")
			}
			adoptedConfig := strings.Replace(initialConfig, `schema: "db/migrations"`, `schema: "db/schema.sql"`, 1)
			if err := os.WriteFile(filepath.Join(dir, "sqlc.yaml"), []byte(adoptedConfig), 0o600); err != nil {
				t.Fatal(err)
			}
			runSchemaAdoptionCommand(t, goCommand, dir, env, "run", "github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0", "generate")
			// sqlc may make an already pinned indirect package a direct import.
			// Resolve that normal change through the configured proxy, preserving
			// the pinned versions. Local offline runs supply a file proxy; a fresh
			// CI runner can download these same published versions normally.
			runSchemaAdoptionCommand(t, goCommand, dir, env, "mod", "tidy")
			assertSchemaAdoptionModule(t, dir, original)
			adopted := runSchemaAdoptionCommand(t, goCommand, dir, env, "test", "-json", "-count=1", "./internal/postgres/schematool", "-run", "^TestRepositorySnapshot$")
			assertSchemaAdoptionTestAction(t, adopted, "pass")
			runSchemaAdoptionCommand(t, goCommand, dir, env, append(append([]string(nil), generate...), "-check")...)
			if violations, err := archcheck.Run(dir); err != nil || len(violations) != 0 {
				t.Fatalf("generated snapshot/imports violate domain architecture: %v, %v", violations, err)
			}
			t.Log("PASS local architecture checks including snapshot SQL/imports")
			runSchemaAdoptionCommand(t, goCommand, dir, env, "build", "./...")
		})
	}
}

func schemaAdoptionEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "GOENV", "GOWORK", "GOFLAGS", "GOTOOLCHAIN", "GOPRIVATE", "GONOPROXY", "GOMAXPROCS", "GO111MODULE":
			continue
		}
		env = append(env, entry)
	}
	return append(env, "GOENV=off", "GOWORK=off", "GOFLAGS=-mod=readonly", "GOTOOLCHAIN=local", "GOMAXPROCS=2", "GO111MODULE=on", "GOPRIVATE=", "GONOPROXY=none")
}

func runSchemaAdoptionCommand(t *testing.T, command, dir string, env []string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.WaitDelay = 5 * time.Second
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("scaffold command go %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	t.Logf("PASS go %s", strings.Join(args, " "))
	return string(output)
}

func schemaAdoptionModule(t *testing.T, dir string) map[string]string {
	t.Helper()
	path := filepath.Join(dir, "go.mod")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := modfile.Parse(path, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Replace) != 0 {
		t.Fatal("scaffold adoption must not depend on local module replacements")
	}
	versions := map[string]string{}
	for _, dependency := range parsed.Require {
		versions[dependency.Mod.Path] = dependency.Mod.Version
	}
	if versions["github.com/forgeplex/appkit"] != "v0.9.2" {
		t.Fatal("scaffold must build against published AppKit v0.9.2")
	}
	return versions
}

func assertSchemaAdoptionModule(t *testing.T, dir string, original map[string]string) {
	t.Helper()
	resolved := schemaAdoptionModule(t, dir)
	for path, version := range original {
		if resolved[path] != version {
			t.Fatalf("scaffold dependency changed from pinned version: %s %s -> %s", path, version, resolved[path])
		}
	}
}

func assertSchemaAdoptionTestAction(t *testing.T, output, want string) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(output))
	action := ""
	for {
		var event struct{ Action, Test string }
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("invalid go test -json output: %v\n%s", err, output)
		}
		if event.Test == "TestRepositorySnapshot" && (event.Action == "pass" || event.Action == "skip" || event.Action == "fail") {
			action = event.Action
		}
	}
	if action != want {
		t.Fatalf("TestRepositorySnapshot action=%q, want %q\n%s", action, want, output)
	}
}
