package schemadoc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit/pgtx"
)

func TestRenderLogicalTemplateLabels(t *testing.T) {
	for _, tables := range [][]Table{nil, {{Name: "probe", Kind: KindTable, Columns: []Column{{Name: "id", Type: "bigint"}}}}} {
		s := Schema{Name: "sample", Tables: tables, Enums: []Enum{{Name: "state", Values: []string{"ready"}}}}
		plain, err := Render(s)
		if err != nil {
			t.Fatal(err)
		}
		s.LogicalTemplate = true
		logical, err := Render(s)
		if err != nil {
			t.Fatal(err)
		}
		if len(logical) != len(plain) {
			t.Fatal("mode changed output paths")
		}
		for name, content := range logical {
			if !strings.Contains(content, "logical-template") || !strings.Contains(content, "代表 schema") || !strings.Contains(content, "运行时分区") {
				t.Errorf("%s lacks logical-template scope: %s", name, content)
			}
			if strings.Contains(plain[name], "logical-template") {
				t.Errorf("nonpartitioned output changed: %s", name)
			}
		}
		again, err := Render(s)
		if err != nil || !reflect.DeepEqual(logical, again) {
			t.Fatalf("logical rendering not deterministic: %v", err)
		}
	}
}

func TestIntrospectCapturedPartitionedTenantPostgres(t *testing.T) {
	dsn := schemaTestDSN(t)
	snapshot := fstest.MapFS{
		"0001.sql": {Data: []byte(pgtx.TenantScopeSQLBare() + `
CREATE TYPE state AS ENUM ('ready', 'done');
CREATE TABLE probe (id bigint PRIMARY KEY, tenant_id text NOT NULL, state state NOT NULL DEFAULT 'ready');
COMMENT ON TABLE probe IS 'logical fixture';
` + pgtx.TenantPolicySQLBare("probe"))},
		"0002.sql": {Data: []byte("ALTER TABLE probe ADD COLUMN note text;")},
	}
	// Dir is deliberately unusable. Supplied migrations must be the only source,
	// and the caller's bytes must not be modified by rendering/introspection.
	dir := filepath.Join(t.TempDir(), "not-a-directory")
	mustWrite(t, dir, "not a repository")
	before, err := fs.ReadFile(snapshot, "0001.sql")
	if err != nil {
		t.Fatal(err)
	}
	before = append([]byte(nil), before...)
	s, err := Introspect(t.Context(), Options{Dir: dir, DSN: dsn, Schema: "logical_sample", Partitioned: true, Migrations: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "logical_sample" || !s.LogicalTemplate {
		t.Fatalf("wrong representative model: %+v", s)
	}
	var probe Table
	for _, table := range s.Tables {
		if table.Name == "probe" {
			probe = table
		}
	}
	if probe.Name == "" || probe.RLS == nil || !probe.RLS.Enabled || !probe.RLS.Force || len(probe.RLS.Policies) != 2 {
		t.Fatalf("logical tenant policies not captured: %+v", probe)
	}
	if colOf(t, probe, "note").Type != "text" || len(s.Enums) != 1 || len(s.Functions) < 2 {
		t.Fatalf("snapshot migrations/catalog incomplete: %+v", s)
	}
	files, err := Render(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"logical-template", "CREATE TABLE logical_sample.probe", "FORCE ROW LEVEL SECURITY", "tenant_isolation_read_all"} {
		if !strings.Contains(files["db/schema/probe.sql"], want) {
			t.Errorf("logical SQL missing %q", want)
		}
	}
	after, _ := fs.ReadFile(snapshot, "0001.sql")
	if string(before) != string(after) {
		t.Fatal("introspection mutated captured migrations")
	}
	plain, err := Introspect(t.Context(), Options{Dir: dir, DSN: dsn, Schema: s.Name, Migrations: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	s.LogicalTemplate = false
	if !reflect.DeepEqual(s, plain) {
		t.Fatal("logical mode changed the migration/catalog model beyond labeling")
	}
}

func TestIntrospectEmptyMigrationSnapshotPostgres(t *testing.T) {
	s, err := Introspect(t.Context(), Options{DSN: schemaTestDSN(t), Schema: "empty_sample", Partitioned: true, Migrations: fstest.MapFS{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Tables) != 1 || s.Tables[0].Name != "schema_migrations" || len(s.Enums) != 0 || len(s.Functions) != 0 {
		t.Fatalf("empty migrations should have only framework history: %+v", s)
	}
	files, err := Render(s)
	if err != nil || !strings.Contains(files[docFile], "0 张业务表") || !strings.Contains(files[docFile], "logical-template") {
		t.Fatalf("empty logical overview: %v %v", files, err)
	}
}

func TestReadCatalogEmptySchemaPostgres(t *testing.T) {
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, schemaTestDSN(t))
	if err != nil {
		t.Fatal(connectionFailure("test connect", err))
	}
	defer pool.Close()
	name, err := tempDBName()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), scratchCleanupTimeout)
		defer cancel()
		if _, err := pool.Exec(cleanup, "DROP SCHEMA "+pgx.Identifier{name}.Sanitize()); err != nil {
			t.Error(err)
		}
	}()
	for _, schema := range []string{name, name + "_missing"} {
		s, err := readCatalog(ctx, pool, schema)
		if err != nil || s.Name != schema || len(s.Tables)+len(s.Enums)+len(s.Functions) != 0 {
			t.Fatalf("empty catalog %q: %+v %v", schema, s, err)
		}
		if _, err := Render(s); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIntrospectInvalidConnectionDoesNotExposeDSN(t *testing.T) {
	const dsn = "postgres://user:very-secret-password@localhost/database?connect_timeout=invalid"
	_, err := Introspect(t.Context(), Options{DSN: dsn, Schema: "sample", Migrations: fstest.MapFS{}})
	if err == nil || strings.Contains(err.Error(), "very-secret-password") || strings.Contains(err.Error(), dsn) {
		t.Fatalf("unsafe connection diagnostic: %v", err)
	}
}

func TestRedactLazyConnectionFailures(t *testing.T) {
	const secret = "private-connection-detail"
	const dsn = "postgres://private-user:private-password@127.0.0.1/private-database?sslmode=disable"
	for _, cause := range []error{errors.New(secret), context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			cfg, err := pgconn.ParseConfig(dsn)
			if err != nil {
				t.Fatal(err)
			}
			// Build a real driver ConnectError without opening a network socket.
			cfg.DialFunc = func(context.Context, string, string) (net.Conn, error) {
				return nil, cause
			}
			_, err = pgconn.ConnectConfig(t.Context(), cfg)
			var original *pgconn.ConnectError
			if !errors.As(err, &original) {
				t.Fatalf("fixture did not produce ConnectError: %v", err)
			}
			wrapped := fmt.Errorf("runner: %w", fmt.Errorf("begin: %w", err))
			got := redactConnectionFailure("scratch query", wrapped)
			var escaped *pgconn.ConnectError
			if got == nil || errors.As(got, &escaped) {
				t.Fatalf("driver config escaped boundary: %v", got)
			}
			for _, sensitive := range []string{secret, "private-user", "private-password", "private-database", "127.0.0.1"} {
				if strings.Contains(got.Error(), sensitive) {
					t.Fatalf("connection diagnostic exposes %q: %v", sensitive, got)
				}
			}
			if cause != nil && (errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded)) && !errors.Is(got, cause) {
				t.Fatalf("cancellation identity lost: %v", got)
			}
		})
	}
	parse := pgconn.NewParseConfigError(dsn, secret, errors.New(secret))
	got := redactConnectionFailure("scratch query", fmt.Errorf("catalog: %w", fmt.Errorf("connect: %w", parse)))
	var escaped *pgconn.ParseConfigError
	if got == nil || errors.As(got, &escaped) || strings.Contains(got.Error(), secret) || strings.Contains(got.Error(), "private-") {
		t.Fatalf("parse error escaped boundary: %v", got)
	}
	sql := &pgconn.PgError{Severity: "ERROR", Code: "42703", Message: "column fixture_note does not exist"}
	wrapped := fmt.Errorf("migration 0002.sql: %w", sql)
	if got := redactConnectionFailure("scratch query", wrapped); got != wrapped || !errors.Is(got, sql) || !strings.Contains(got.Error(), "0002.sql") || !strings.Contains(got.Error(), sql.Message) {
		t.Fatalf("ordinary SQL diagnostic changed: %v", got)
	}
	if got := redactConnectionFailure("scratch query", nil); got != nil {
		t.Fatalf("nil error changed: %v", got)
	}
}

func TestIntrospectCancellationCleansScratchDatabasePostgres(t *testing.T) {
	dsn := schemaTestDSN(t)
	observer, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(connectionFailure("test observer", err))
	}
	defer observer.Close()
	marker, err := tempDBName()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := fstest.MapFS{"0001.sql": {Data: []byte(fmt.Sprintf("SELECT pg_sleep(20) /* %s */;", marker))}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := Introspect(ctx, Options{DSN: dsn, Schema: "cancel_sample", Partitioned: true, Migrations: snapshot})
		done <- err
	}()
	finished := false
	defer func() {
		cancel()
		if !finished {
			select {
			case <-done:
			case <-time.After(2*scratchCleanupTimeout + 2*time.Second):
				t.Error("canceled introspection did not finish cleanup within budget")
			}
		}
	}()
	// Observe only this invocation's unique SQL marker. Other packages may run
	// scratch introspection concurrently, so a global prefix count is not safe.
	poll, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var database string
	for database == "" {
		err := observer.QueryRow(poll, `SELECT datname FROM pg_stat_activity
WHERE datname LIKE 'appkit_schema_%' AND query LIKE $1 LIMIT 1`, "%"+marker+"%").Scan(&database)
		if err == nil {
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(connectionFailure("observe scratch migration", err))
		}
		select {
		case err := <-done:
			finished = true
			t.Fatalf("introspection stopped before cancellation: %v", err)
		case <-poll.Done():
			t.Fatal("scratch migration did not become observable")
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case err := <-done:
		finished = true
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation identity lost: %v", err)
		}
	case <-time.After(2*scratchCleanupTimeout + 2*time.Second):
		t.Fatal("introspection cleanup exceeded its independent budget")
	}
	var remains bool
	if err := observer.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", database).Scan(&remains); err != nil {
		t.Fatal(err)
	}
	if remains {
		t.Fatalf("canceled introspection left its scratch database %s", database)
	}
}

type fakeScratchAdmin struct {
	exec  func(context.Context, string) error
	close func(context.Context) error
}

func (a fakeScratchAdmin) Exec(ctx context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, a.exec(ctx, sql)
}
func (a fakeScratchAdmin) Close(ctx context.Context) error { return a.close(ctx) }

func TestScratchCleanupSurvivesCancellationAndReportsFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assertBudget := func(ctx context.Context) {
		t.Helper()
		deadline, ok := ctx.Deadline()
		if ctx.Err() != nil || !ok || time.Until(deadline) > scratchCleanupTimeout {
			t.Fatalf("cleanup did not get independent bounded context: %v %v", ctx.Err(), deadline)
		}
	}
	closed := false
	err := cleanupDatabase(ctx, "appkit_schema_fixture", func(ctx context.Context) (scratchAdmin, error) {
		assertBudget(ctx)
		return fakeScratchAdmin{
			exec: func(ctx context.Context, sql string) error {
				assertBudget(ctx)
				if sql != `DROP DATABASE IF EXISTS "appkit_schema_fixture" WITH (FORCE)` {
					t.Fatalf("unexpected destructive target: %s", sql)
				}
				return errors.New("postgres://user:secret@host/database")
			},
			close: func(ctx context.Context) error {
				assertBudget(ctx)
				closed = true
				return errors.New("secret close failure")
			},
		}, nil
	})
	if err == nil || !closed || !strings.Contains(err.Error(), "人工删除") || !strings.Contains(err.Error(), "关闭管理连接失败") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("cleanup failure lost or leaked: %v; closed=%t", err, closed)
	}
	err = cleanupDatabase(ctx, "appkit_schema_fixture", func(ctx context.Context) (scratchAdmin, error) {
		assertBudget(ctx)
		return nil, context.DeadlineExceeded
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleanup deadline identity lost: %v", err)
	}
}

func schemaTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置")
	}
	return dsn
}
