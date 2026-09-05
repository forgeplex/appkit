package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/forgeplex/appkit/ruleset"
	"github.com/jackc/pgx/v5/pgconn"
	yaml "go.yaml.in/yaml/v3"
)

// This test is copied with the tool into internal/postgres/schematool. It makes
// the normal domain go test gate check both input hashes and, when CI supplies a
// test server, the schema actually obtained by replaying migrations. A manually
// recomputed snapshot hash must not be sufficient to pass that database gate.
func TestRepositorySnapshot(t *testing.T) {
	dir, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	adopted, err := verifyRepositorySnapshot(ctx, dir, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	if !adopted {
		t.Skip("repository has not adopted a standalone sqlc schema snapshot")
	}
}

func verifyRepositorySnapshot(ctx context.Context, dir, dsn string) (bool, error) {
	config, err := readRepoFile(dir, ".appkit.yml")
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil // framework source, not a copied domain tool
	}
	if err != nil {
		return true, err
	}
	cfg, err := ruleset.ParseAppConfig(config)
	if err != nil {
		return true, err
	}
	sqlc, err := readRepoFile(dir, "sqlc.yaml")
	if err != nil {
		return true, err
	}
	document, err := decodeSQLCConfig(sqlc)
	if err != nil {
		return true, err
	}
	adopted := usesSnapshotSQLC(document)
	for _, path := range []string{snapshotPath, lockPath} {
		_, err := os.Lstat(filepath.Join(dir, path))
		if err == nil {
			adopted = true
		} else if !errors.Is(err, fs.ErrNotExist) {
			return true, err
		}
	}
	if !adopted {
		return false, nil
	}
	if err := requireSnapshotSQLC(sqlc); err != nil {
		return true, err
	}
	// checkSource validates the lock's domain and partitioned mode against the
	// same public config parser used by the CLI, not values trusted from the lock.
	if err := checkSource(dir, cfg.Domain, cfg.Partitioned); err != nil {
		return true, err
	}
	if dsn != "" {
		if err := generate(ctx, dir, cfg.Domain, cfg.Partitioned, dsn, true); err != nil {
			return true, err
		}
	}
	return true, nil
}

// The snapshot gate deliberately covers one PostgreSQL schema input. Decode
// YAML normally so comments, aliases and unrelated nested "schema" fields do
// not change which path sqlc actually uses.
func requireSnapshotSQLC(config []byte) error {
	path, err := sqlcSchemaPath(config)
	if err != nil {
		return err
	}
	if path != snapshotPath {
		return errors.New("snapshot sqlc.yaml must use schema: db/schema.sql")
	}
	return nil
}

func sqlcSchemaPath(config []byte) (string, error) {
	document, err := decodeSQLCConfig(config)
	if err != nil {
		return "", err
	}
	if document["version"] != "2" && document["version"] != 2 {
		return "", errors.New("schema snapshot requires sqlc configuration version 2")
	}
	entries, ok := document["sql"].([]any)
	if !ok || len(entries) != 1 {
		return "", errors.New("schema snapshot requires exactly one sqlc SQL entry")
	}
	entry, ok := entries[0].(map[string]any)
	if !ok || entry["engine"] != "postgresql" {
		return "", errors.New("schema snapshot requires a PostgreSQL sqlc SQL entry")
	}
	input := entry["schema"]
	if paths, ok := input.([]any); ok {
		if len(paths) != 1 {
			return "", errors.New("schema snapshot requires exactly one sqlc schema input")
		}
		input = paths[0]
	}
	path, ok := input.(string)
	if !ok || path == "" {
		return "", errors.New("sqlc schema input must be one nonempty string path")
	}
	return path, nil
}

func decodeSQLCConfig(config []byte) (map[string]any, error) {
	// Decode the entire document, including unknown metadata, so the official
	// decoder rejects duplicate keys and resolves aliases/merges before adoption
	// is detected. Snapshot-specific shape restrictions apply only after adoption.
	decoder := yaml.NewDecoder(bytes.NewReader(config))
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("invalid sqlc.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("sqlc.yaml must contain exactly one YAML document")
	}
	return document, nil
}

func usesSnapshotSQLC(document map[string]any) bool {
	// Inspect every actual SQL input, including legacy packages, without treating
	// comments or unrelated metadata as adoption. A snapshot in an unsupported
	// configuration must still activate the strict gate instead of bypassing it.
	for _, key := range []string{"sql", "packages"} {
		entries, _ := document[key].([]any)
		for _, value := range entries {
			entry, _ := value.(map[string]any)
			inputs, ok := entry["schema"].([]any)
			if !ok {
				inputs = []any{entry["schema"]}
			}
			for _, input := range inputs {
				name, ok := input.(string)
				if ok && filepath.ToSlash(filepath.Clean(name)) == snapshotPath {
					return true
				}
			}
		}
	}
	return false
}

func TestCommandArguments(t *testing.T) {
	t.Setenv("TEST_DATABASE_URL", "")
	for _, args := range [][]string{
		{}, {"-domain", "Demo"}, {"-domain", "demo_test"},
		{"-domain", "../demo"}, {"-domain", "demo", "extra"},
		{"-domain", "demo", "-unknown"}, {"-domain", "demo"},
		{"-domain", "demo", "-check", "-check-source"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if err := command(args); err == nil {
				t.Fatal("invalid arguments unexpectedly succeeded")
			}
		})
	}
	dir := schemaToolFixture(t)
	writeOfflineSnapshot(t, dir)
	// Offline verification must not attempt a connection even when the ambient
	// test URL cannot be parsed; callers can use this gate without PostgreSQL.
	t.Setenv("TEST_DATABASE_URL", "postgres://hidden-secret@%zz/invalid")
	if err := command([]string{"-dir", dir, "-domain", "demo", "-partitioned", "-check-source"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-dir", dir, "-domain", "other", "-partitioned", "-check-source"},
		{"-dir", dir, "-domain", "demo", "-check-source"},
		{"-dir", dir, "-domain", "demo", "-partitioned"},
	} {
		err := command(args)
		if err == nil || strings.Contains(err.Error(), "hidden-secret") {
			t.Fatalf("expected redacted error, got %v", err)
		}
	}
}

func TestCaptureFingerprintsAndFreezesInputs(t *testing.T) {
	dir := schemaToolFixture(t)
	writeSchemaTestFile(t, dir, "db/migrations/README.md", "not a migration")
	writeSchemaTestFile(t, dir, "internal/postgres/schematool/extra.go", "package main\n// helper\n")
	writeSchemaTestFile(t, dir, "internal/postgres/schematool/extra_test.go", "package main\n")
	source, hashes, err := capture(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		".appkit.yml", "go.mod", "go.sum", "db/migrations/0001_widgets.sql", "db/migrations/0002_index.sql",
		"internal/postgres/schematool/main.go", "internal/postgres/schematool/catalog.go", "internal/postgres/schematool/extra.go",
	}
	if len(hashes) != len(want) || len(source) != 2 {
		t.Fatalf("unexpected captured set: sources=%v, migrations=%v", hashes, source)
	}
	for _, path := range want {
		if got := hashes[path]; got != digest(readSchemaTestFile(t, dir, path)) {
			t.Errorf("incorrect digest for %s: %s", path, got)
		}
	}
	original := append([]byte(nil), source["0001_widgets.sql"].Data...)
	writeSchemaTestFile(t, dir, "db/migrations/0001_widgets.sql", "changed after capture")
	if !bytes.Equal(source["0001_widgets.sql"].Data, original) {
		t.Fatal("captured migration changed after source file was edited")
	}
	if got := digest([]byte("abc")); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("digest is not SHA-256: %s", got)
	}
}

func TestCaptureRejectsInvalidInputs(t *testing.T) {
	tests := map[string]func(*testing.T, string){
		"no migrations": func(t *testing.T, dir string) {
			removeSchemaTestFile(t, dir, "db/migrations/0001_widgets.sql")
			removeSchemaTestFile(t, dir, "db/migrations/0002_index.sql")
		},
		"invalid filename": func(t *testing.T, dir string) { writeSchemaTestFile(t, dir, "db/migrations/bad.sql", "SELECT 1;") },
		"migration directory": func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "db/migrations/0003_directory.sql"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"no tool source": func(t *testing.T, dir string) {
			removeSchemaTestFile(t, dir, "internal/postgres/schematool/main.go")
			removeSchemaTestFile(t, dir, "internal/postgres/schematool/catalog.go")
		},
		"missing module": func(t *testing.T, dir string) { removeSchemaTestFile(t, dir, "go.mod") },
		"missing sums":   func(t *testing.T, dir string) { removeSchemaTestFile(t, dir, "go.sum") },
		"missing config": func(t *testing.T, dir string) { removeSchemaTestFile(t, dir, ".appkit.yml") },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			dir := schemaToolFixture(t)
			mutate(t, dir)
			if _, _, err := capture(dir); err == nil {
				t.Fatal("invalid capture unexpectedly succeeded")
			}
		})
	}
}

func TestCheckSourceDetectsDrift(t *testing.T) {
	mutations := map[string]func(*testing.T, string){
		"migration modified": func(t *testing.T, dir string) {
			writeSchemaTestFile(t, dir, "db/migrations/0002_index.sql", "SELECT 'private-value';")
		},
		"migration added": func(t *testing.T, dir string) {
			writeSchemaTestFile(t, dir, "db/migrations/0003_added.sql", "SELECT 1;")
		},
		"migration removed": func(t *testing.T, dir string) { removeSchemaTestFile(t, dir, "db/migrations/0002_index.sql") },
		"tool changed": func(t *testing.T, dir string) {
			writeSchemaTestFile(t, dir, "internal/postgres/schematool/main.go", "private-value")
		},
		"catalog changed": func(t *testing.T, dir string) {
			writeSchemaTestFile(t, dir, "internal/postgres/schematool/catalog.go", "private-value")
		},
		"helper added": func(t *testing.T, dir string) {
			writeSchemaTestFile(t, dir, "internal/postgres/schematool/helper.go", "package main\n")
		},
		"module changed": func(t *testing.T, dir string) { writeSchemaTestFile(t, dir, "go.mod", "module example.com/changed\n") },
		"sums changed":   func(t *testing.T, dir string) { writeSchemaTestFile(t, dir, "go.sum", "private-value\n") },
		"config changed": func(t *testing.T, dir string) {
			writeSchemaTestFile(t, dir, ".appkit.yml", string(readSchemaTestFile(t, dir, ".appkit.yml"))+"# private-value\n")
		},
		"snapshot changed": func(t *testing.T, dir string) {
			writeSchemaTestFile(t, dir, snapshotPath, snapshotHeader+"SELECT 'private-value';\n")
		},
		"snapshot unowned": func(t *testing.T, dir string) {
			writeSchemaTestFile(t, dir, snapshotPath, "SELECT 'private-value';\n")
			updateTestManifest(t, dir, func(lock *manifest) { lock.Snapshot = digest(readSchemaTestFile(t, dir, snapshotPath)) })
		},
		"snapshot missing": func(t *testing.T, dir string) { removeSchemaTestFile(t, dir, snapshotPath) },
		"lock missing":     func(t *testing.T, dir string) { removeSchemaTestFile(t, dir, lockPath) },
		"lock malformed":   func(t *testing.T, dir string) { writeSchemaTestFile(t, dir, lockPath, "{") },
		"format changed": func(t *testing.T, dir string) {
			updateTestManifest(t, dir, func(lock *manifest) { lock.Format++ })
		},
		"domain changed": func(t *testing.T, dir string) {
			updateTestManifest(t, dir, func(lock *manifest) { lock.Domain = "other" })
		},
		"partition changed": func(t *testing.T, dir string) {
			updateTestManifest(t, dir, func(lock *manifest) { lock.Partitioned = false })
		},
		"source removed from lock": func(t *testing.T, dir string) {
			updateTestManifest(t, dir, func(lock *manifest) { delete(lock.Sources, "go.mod") })
		},
		"source substituted in lock": func(t *testing.T, dir string) {
			updateTestManifest(t, dir, func(lock *manifest) { delete(lock.Sources, "go.mod"); lock.Sources["go.work"] = "invalid" })
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			dir := schemaToolFixture(t)
			writeOfflineSnapshot(t, dir)
			if err := checkSource(dir, "demo", true); err != nil {
				t.Fatalf("clean fixture: %v", err)
			}
			mutate(t, dir)
			err := checkSource(dir, "demo", true)
			if err == nil || strings.Contains(err.Error(), "private-value") {
				t.Fatalf("expected source drift without contents, got %v", err)
			}
		})
	}
	t.Run("test files are not exporter inputs", func(t *testing.T) {
		dir := schemaToolFixture(t)
		writeOfflineSnapshot(t, dir)
		writeSchemaTestFile(t, dir, "internal/postgres/schematool/main_test.go", "package main\n")
		if err := checkSource(dir, "demo", true); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRepositorySymlinksRejected(t *testing.T) {
	for _, path := range []string{
		".appkit.yml", "go.mod", "go.sum", "db/migrations/0001_widgets.sql",
		"internal/postgres/schematool/main.go", snapshotPath, lockPath,
		"db", "db/migrations", "internal/postgres", "internal/postgres/schematool",
	} {
		t.Run(path, func(t *testing.T) {
			dir := schemaToolFixture(t)
			writeOfflineSnapshot(t, dir)
			original := filepath.Join(dir, path)
			target := filepath.Join(t.TempDir(), "original")
			if err := os.Rename(original, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, original); err != nil {
				t.Fatal(err)
			}
			if err := checkSource(dir, "demo", true); err == nil {
				t.Fatal("repository-controlled symlink was followed")
			}
		})
	}
	for _, path := range []string{"../outside", "/absolute", "db/../outside", ""} {
		if err := checkPath(t.TempDir(), path); err == nil {
			t.Errorf("invalid repository path %q accepted", path)
		}
	}
}

func TestSafeDBError(t *testing.T) {
	secret := "postgres://private-user:private-password@private-host/private-db"
	if err := safeDBError("test", nil); err != nil {
		t.Fatal(err)
	}
	for name, original := range map[string]error{
		"connection":       errors.New(secret),
		"postgres":         &pgconn.PgError{Code: "23514", Message: secret, Detail: secret, Hint: secret, Where: secret},
		"wrapped postgres": fmt.Errorf("%s: %w", secret, &pgconn.PgError{Code: "23514", Message: secret}),
		"canceled":         fmt.Errorf("%s: %w", secret, context.Canceled),
		"deadline":         fmt.Errorf("%s: %w", secret, context.DeadlineExceeded),
	} {
		t.Run(name, func(t *testing.T) {
			err := safeDBError("schema operation", original)
			if err == nil || !strings.Contains(err.Error(), "schema operation") || strings.Contains(err.Error(), "private-") {
				t.Fatalf("unsafe database error: %v", err)
			}
			if strings.Contains(name, "postgres") && !strings.Contains(err.Error(), "23514") {
				t.Fatalf("SQLSTATE lost: %v", err)
			}
			for _, identity := range []error{context.Canceled, context.DeadlineExceeded} {
				if errors.Is(original, identity) != errors.Is(err, identity) {
					t.Fatalf("context error identity changed: %v", err)
				}
			}
			if errors.Is(err, original) {
				t.Fatal("raw database diagnostic remains in the error chain")
			}
		})
	}
}

func TestRepositorySnapshotAdoption(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*testing.T, string)
		adopted bool
		wantErr bool
	}{
		{"valid", func(*testing.T, string) {}, true, false},
		{"framework source", func(t *testing.T, dir string) { removeSchemaTestFile(t, dir, ".appkit.yml") }, false, false},
		{"not adopted", func(t *testing.T, dir string) {
			removeSchemaTestFile(t, dir, snapshotPath)
			removeSchemaTestFile(t, dir, lockPath)
			writeSchemaTestFile(t, dir, "sqlc.yaml", "version: '2'\nsql:\n  - engine: postgresql\n    schema: db/migrations\n")
		}, false, false},
		{"comment and metadata do not adopt", func(t *testing.T, dir string) {
			removeSchemaTestFile(t, dir, snapshotPath)
			removeSchemaTestFile(t, dir, lockPath)
			writeSchemaTestFile(t, dir, "sqlc.yaml", "# make schema-sqlc will produce db/schema.sql\nversion: 2\nmetadata: {schema: db/schema.sql}\nsql: [{engine: postgresql, schema: db/migrations}]\n")
		}, false, false},
		{"snapshot alias adopts", func(t *testing.T, dir string) {
			removeSchemaTestFile(t, dir, snapshotPath)
			removeSchemaTestFile(t, dir, lockPath)
			writeSchemaTestFile(t, dir, "sqlc.yaml", "version: 2\nmetadata: &snapshot db/schema.sql\nsql: [{engine: postgresql, schema: *snapshot}]\n")
		}, true, true},
		{"points to missing snapshot", func(t *testing.T, dir string) {
			removeSchemaTestFile(t, dir, snapshotPath)
			removeSchemaTestFile(t, dir, lockPath)
		}, true, true},
		{"missing lock", func(t *testing.T, dir string) { removeSchemaTestFile(t, dir, lockPath) }, true, true},
		{"missing schema", func(t *testing.T, dir string) { removeSchemaTestFile(t, dir, snapshotPath) }, true, true},
		{"sqlc points to migrations", func(t *testing.T, dir string) {
			writeSchemaTestFile(t, dir, "sqlc.yaml", "version: '2'\nsql:\n  - engine: postgresql\n    schema: db/migrations\n")
		}, true, true},
		{"lock disagrees with config", func(t *testing.T, dir string) {
			updateTestManifest(t, dir, func(lock *manifest) { lock.Domain = "other" })
		}, true, true},
		{"mode disagrees with config", func(t *testing.T, dir string) {
			updateTestManifest(t, dir, func(lock *manifest) { lock.Partitioned = false })
		}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := schemaToolFixture(t)
			writeOfflineSnapshot(t, dir)
			tt.mutate(t, dir)
			adopted, err := verifyRepositorySnapshot(context.Background(), dir, "")
			if adopted != tt.adopted || (err != nil) != tt.wantErr {
				t.Fatalf("adopted=%v, err=%v; want adopted=%v, error=%v", adopted, err, tt.adopted, tt.wantErr)
			}
		})
	}
}

func TestRepositorySnapshotOptionalAdoption(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		artifact     string
		adopted      bool
		wantErr      bool
	}{
		{
			name:   "multiple SQL entries without a snapshot",
			config: "version: 2\nsql: [{engine: postgresql, schema: db/migrations}, {engine: mysql, schema: db/mysql}]\n",
		},
		{
			name:   "multiple schema inputs without a snapshot",
			config: "version: 2\nsql: [{engine: postgresql, schema: [db/migrations, db/shared]}]\n",
		},
		{
			name:   "non-PostgreSQL without a snapshot",
			config: "version: 2\nsql: [{engine: sqlite, schema: db/migrations}]\n",
		},
		{
			name:   "legacy config without a snapshot",
			config: "version: 1\npackages: [{engine: postgresql, schema: db/migrations}]\n",
		},
		{
			name:   "unused snapshot anchor does not adopt",
			config: "version: 2\ndefaults: &entry {engine: postgresql, schema: db/schema.sql}\nsql: [{<<: *entry, schema: [db/migrations, db/shared]}]\n",
		},
		{
			name:    "snapshot in a later SQL entry adopts",
			config:  "version: 2\nsql: [{engine: postgresql, schema: db/migrations}, {engine: postgresql, schema: db/schema.sql}]\n",
			adopted: true, wantErr: true,
		},
		{
			name:    "snapshot in a later schema input adopts",
			config:  "version: 2\nsql: [{engine: postgresql, schema: [db/migrations, db/schema.sql]}]\n",
			adopted: true, wantErr: true,
		},
		{
			name:    "aliased snapshot input adopts",
			config:  "version: 2\ninputs: &inputs [db/migrations, db/schema.sql]\nsql: [{engine: postgresql, schema: *inputs}]\n",
			adopted: true, wantErr: true,
		},
		{
			name:    "merged snapshot entry adopts",
			config:  "version: 2\ndefaults: &entry {engine: postgresql, schema: db/schema.sql}\nsql: [{engine: postgresql, schema: db/migrations}, {<<: *entry}]\n",
			adopted: true, wantErr: true,
		},
		{
			name:    "equivalent relative snapshot path adopts",
			config:  "version: 2\nsql: [{engine: postgresql, schema: ./db/schema.sql}]\n",
			adopted: true, wantErr: true,
		},
		{
			name:    "legacy snapshot input adopts",
			config:  "version: 1\npackages: [{engine: postgresql, schema: db/schema.sql}]\n",
			adopted: true, wantErr: true,
		},
		{
			name:     "snapshot file enables strict checks",
			config:   "version: 2\nsql: [{engine: sqlite, schema: db/migrations}]\n",
			artifact: snapshotPath, adopted: true, wantErr: true,
		},
		{
			name:     "lock file enables strict checks",
			config:   "version: 2\nsql: [{engine: postgresql, schema: [db/migrations, db/shared]}]\n",
			artifact: lockPath, adopted: true, wantErr: true,
		},
		{
			name:    "duplicate keys still rejected before adoption",
			config:  "version: 2\nsql: [{engine: sqlite, schema: db/migrations}]\nmetadata: {value: 1, value: 2}\n",
			adopted: true, wantErr: true,
		},
		{
			name:    "duplicate SQL entries cannot hide a snapshot",
			config:  "version: 2\nsql: [{engine: postgresql, schema: db/schema.sql}]\nsql: [{engine: sqlite, schema: db/migrations}]\n",
			adopted: true, wantErr: true,
		},
		{
			name:    "multiple documents still rejected before adoption",
			config:  "version: 2\nsql: [{engine: sqlite, schema: db/migrations}]\n---\nnull\n",
			adopted: true, wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := schemaToolFixture(t)
			writeSchemaTestFile(t, dir, "sqlc.yaml", tc.config)
			if tc.artifact != "" {
				writeSchemaTestFile(t, dir, tc.artifact, "existing snapshot artifact")
			}
			// An inactive gate must not use the ambient test connection either.
			adopted, err := verifyRepositorySnapshot(context.Background(), dir, "postgres://hidden-secret@%zz/invalid")
			if adopted != tc.adopted || (err != nil) != tc.wantErr {
				t.Fatalf("adopted=%v, err=%v; want adopted=%v, error=%v", adopted, err, tc.adopted, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "hidden-secret") {
				t.Fatalf("test connection details leaked: %v", err)
			}
		})
	}
}

func TestSnapshotSQLCPath(t *testing.T) {
	for _, path := range []string{"db/schema.sql", `"db/schema.sql"`, "'db/schema.sql'", "[db/schema.sql]", `"db/\u0073chema.sql"`} {
		if err := requireSnapshotSQLC([]byte("version: '2'\nsql:\n  - engine: postgresql\n    schema: " + path + " # generated\n")); err != nil {
			t.Fatal(err)
		}
	}
	for _, body := range []string{
		"version: 2\nsql: [{engine: postgresql, schema: db/schema.sql}]\n",
		"version: 2\nmetadata: {schema: ignored.sql}\nsql: [{engine: postgresql, schema: db/schema.sql}]\n",
		"version: 2\nmetadata: &snapshot db/schema.sql\nsql: [{engine: postgresql, schema: *snapshot}]\n",
		"version: 2\ndefaults: &entry {engine: postgresql, schema: db/schema.sql}\nsql: [*entry]\n",
		"version: 2\ndefaults: &entry {engine: postgresql, schema: db/migrations}\nsql: [{<<: *entry, schema: db/schema.sql}]\n",
		"version: 2\nsql:\n  - engine: postgresql\n    'schema':\n      - db/schema.sql\n",
		"version: 2\nsql:\n  - engine: postgresql\n    schema: |-\n      db/schema.sql\n",
	} {
		if err := requireSnapshotSQLC([]byte(body)); err != nil {
			t.Errorf("valid YAML rejected: %v; config=%q", err, body)
		}
	}
	for _, body := range []string{
		"# schema: db/schema.sql\n",
		"version: 1\nsql: [{engine: postgresql, schema: db/schema.sql}]\n",
		"version: 2\nsql: [{engine: sqlite, schema: db/schema.sql}]\n",
		"version: 2\nsql: [{engine: postgresql, schema: db/migrations}]\nnote: |\n    schema: db/schema.sql\n",
		"version: 2\nsql: [{engine: postgresql, schema: db/schema.sql}, {engine: postgresql, schema: other.sql}]\n",
		"version: 2\nsql: [{engine: postgresql, schema: [db/schema.sql, other.sql]}]\n",
		"version: 2\nsql: [{engine: postgresql, schema: []}]\n",
		"version: 2\nsql: [{engine: postgresql, schema: null}]\n",
		"version: 2\nsql: [{engine: postgresql, schema: 42}]\n",
		"version: 2\nsql: [{engine: postgresql, schema: {path: db/schema.sql}}]\n",
		"version: 2\nsql: [{engine: postgresql, schema: db/schema.sql, schema: other.sql}]\n",
		"version: 2\nsql: [{engine: postgresql, schema: db/schema.sql}]\nsql: []\n",
		"version: 2\nsql: [{engine: postgresql, schema: db/schema.sql}]\nmetadata: {value: 1, value: 2}\n",
		"version: 2\nsql: [{engine: postgresql, schema: db/schema.sql}]\n---\nnull\n",
		"version: 2\nsql: [{engine: postgresql, schema: db/schema.sql}]\n---\n",
		"version: 2\nsql:\n  - engine: postgresql\n    schema: \"'db/schema.sql'\"\n",
	} {
		if err := requireSnapshotSQLC([]byte(body)); err == nil {
			t.Errorf("ambiguous/non-snapshot config accepted: %q", body)
		}
	}
}

func TestGenerateProtectsOutputsAndReplays(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is required for scratch-database replay")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := schemaToolFixture(t)
	if err := generate(ctx, dir, "demo", true, dsn, false); err != nil {
		t.Fatal(err)
	}
	original := map[string][]byte{
		snapshotPath: readSchemaTestFile(t, dir, snapshotPath),
		lockPath:     readSchemaTestFile(t, dir, lockPath),
	}
	if adopted, err := verifyRepositorySnapshot(ctx, dir, dsn); err != nil || !adopted {
		t.Fatalf("fresh replay failed: adopted=%v, err=%v", adopted, err)
	}
	for _, path := range []string{snapshotPath, lockPath} {
		t.Run("handwritten "+path, func(t *testing.T) {
			restoreSchemaOutputs(t, dir, original)
			writeSchemaTestFile(t, dir, path, "handwritten SQL or manifest\n")
			before := map[string][]byte{
				snapshotPath: readSchemaTestFile(t, dir, snapshotPath),
				lockPath:     readSchemaTestFile(t, dir, lockPath),
			}
			err := generate(ctx, dir, "demo", true, dsn, false)
			if err == nil || !strings.Contains(err.Error(), "unowned") {
				t.Fatalf("handwritten file was not refused: %v", err)
			}
			assertSchemaOutputs(t, dir, before)
		})
	}
	for _, path := range []string{snapshotPath, lockPath} {
		t.Run("symlink "+path, func(t *testing.T) {
			restoreSchemaOutputs(t, dir, original)
			removeSchemaTestFile(t, dir, path)
			victimDir := t.TempDir()
			writeSchemaTestFile(t, victimDir, "victim", "must not be overwritten\n")
			if err := os.Symlink(filepath.Join(victimDir, "victim"), filepath.Join(dir, path)); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { removeSchemaTestFile(t, dir, path) })
			if err := generate(ctx, dir, "demo", true, dsn, false); err == nil {
				t.Fatal("output symlink was followed")
			}
			if got := string(readSchemaTestFile(t, victimDir, "victim")); got != "must not be overwritten\n" {
				t.Fatalf("symlink victim modified: %q", got)
			}
			for other, want := range original {
				if other != path && !bytes.Equal(readSchemaTestFile(t, dir, other), want) {
					t.Fatalf("other output %s was replaced before the unsafe destination was checked", other)
				}
			}
		})
	}
	t.Run("forged offline hash still fails replay", func(t *testing.T) {
		restoreSchemaOutputs(t, dir, original)
		writeSchemaTestFile(t, dir, snapshotPath, string(original[snapshotPath])+"CREATE TABLE invented (id integer);\n")
		updateTestManifest(t, dir, func(lock *manifest) { lock.Snapshot = digest(readSchemaTestFile(t, dir, snapshotPath)) })
		if err := checkSource(dir, "demo", true); err != nil {
			t.Fatalf("offline hashes should match the explicitly forged fixture: %v", err)
		}
		before := map[string][]byte{
			snapshotPath: readSchemaTestFile(t, dir, snapshotPath),
			lockPath:     readSchemaTestFile(t, dir, lockPath),
		}
		if adopted, err := verifyRepositorySnapshot(ctx, dir, dsn); !adopted || err == nil {
			t.Fatalf("database replay accepted forged snapshot: adopted=%v, err=%v", adopted, err)
		}
		assertSchemaOutputs(t, dir, before)
	})
}

func schemaToolFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for path, body := range map[string]string{
		".appkit.yml":                             "version: 1\nkind: domain\ndomain: demo\nmodule: github.com/forgeplex/demo\npartitioned: true\n",
		"go.mod":                                  "module github.com/forgeplex/demo\n\ngo 1.26.0\n",
		"go.sum":                                  "",
		"sqlc.yaml":                               "version: '2'\nsql:\n  - engine: postgresql\n    schema: db/schema.sql\n    queries: db/queries\n",
		"db/migrations/0001_widgets.sql":          "CREATE TABLE widgets (id integer PRIMARY KEY, name text NOT NULL);\n",
		"db/migrations/0002_index.sql":            "CREATE INDEX widgets_name_idx ON widgets (name);\n",
		"internal/postgres/schematool/main.go":    "package main\n// schema command fixture\n",
		"internal/postgres/schematool/catalog.go": "package main\n// catalog fixture\n",
	} {
		writeSchemaTestFile(t, dir, path, body)
	}
	return dir
}

func writeOfflineSnapshot(t *testing.T, dir string) {
	t.Helper()
	_, hashes, err := capture(dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := snapshotHeader + "CREATE TABLE widgets (id integer PRIMARY KEY, name text NOT NULL);\n"
	writeSchemaTestFile(t, dir, snapshotPath, snapshot)
	writeTestManifest(t, dir, manifest{Format: 1, Domain: "demo", Partitioned: true, Sources: hashes, Snapshot: digest([]byte(snapshot))})
}

func updateTestManifest(t *testing.T, dir string, update func(*manifest)) {
	t.Helper()
	var lock manifest
	if err := json.Unmarshal(readSchemaTestFile(t, dir, lockPath), &lock); err != nil {
		t.Fatal(err)
	}
	update(&lock)
	writeTestManifest(t, dir, lock)
}

func writeTestManifest(t *testing.T, dir string, lock manifest) {
	t.Helper()
	encoded, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeSchemaTestFile(t, dir, lockPath, string(encoded)+"\n")
}

func restoreSchemaOutputs(t *testing.T, dir string, outputs map[string][]byte) {
	t.Helper()
	for path, body := range outputs {
		writeSchemaTestFile(t, dir, path, string(body))
	}
}

func assertSchemaOutputs(t *testing.T, dir string, outputs map[string][]byte) {
	t.Helper()
	for path, want := range outputs {
		if !bytes.Equal(readSchemaTestFile(t, dir, path), want) {
			t.Errorf("output %s was modified", path)
		}
	}
}

func writeSchemaTestFile(t *testing.T, dir, path, body string) {
	t.Helper()
	filename := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readSchemaTestFile(t *testing.T, dir, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, path))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func removeSchemaTestFile(t *testing.T, dir, path string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, path)); err != nil {
		t.Fatal(err)
	}
}
