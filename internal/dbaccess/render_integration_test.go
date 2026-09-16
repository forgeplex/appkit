package dbaccess

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/pgmigrate"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var integrationSequence atomic.Int64

func TestRenderSQLExecutesOnPostgres(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过 PostgreSQL 集成测试")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	suffix := fmt.Sprintf("%d_%d", time.Now().UnixNano(), integrationSequence.Add(1))
	schema := "access_test_" + suffix
	migrationSchema := "access_migration_" + suffix
	dependencyMigrationSchema := "access_dependency_" + suffix
	login := "access_login_" + suffix
	permission := "access_perm_" + suffix
	ident := func(parts ...string) string { return pgx.Identifier(parts).Sanitize() }
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+ident(schema)+" CASCADE")
		_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+ident(migrationSchema)+" CASCADE")
		_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+ident(dependencyMigrationSchema)+" CASCADE")
		_, _ = pool.Exec(ctx, "DROP ROLE IF EXISTS "+ident(login))
		_, _ = pool.Exec(ctx, "DROP ROLE IF EXISTS "+ident(permission))
	})
	if _, err := pool.Exec(ctx, "CREATE ROLE "+ident(login)+" LOGIN INHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "CREATE ROLE "+ident(permission)+" NOLOGIN INHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+ident(schema)+"; CREATE TABLE "+ident(schema, "items")+" (id bigint PRIMARY KEY, tenant_id text NOT NULL, value text); CREATE TABLE "+ident(schema, "blocked_items")+" (id bigint PRIMARY KEY); CREATE TABLE "+ident(schema, "missing_items")+" (id bigint PRIMARY KEY, tenant_id text NOT NULL); CREATE POLICY access_test_policy ON "+ident(schema, "items")+" USING (true) WITH CHECK (true); CREATE SEQUENCE "+ident(schema, "item_seq")+"; CREATE FUNCTION "+ident(schema, "echo")+"(text) RETURNS text LANGUAGE sql IMMUTABLE AS 'SELECT $1'; GRANT TRIGGER ON "+ident(schema, "items")+" TO "+ident(permission)+"; GRANT UPDATE, TRIGGER ON "+ident(schema, "blocked_items")+" TO "+ident(permission)+"; GRANT INSERT (id), UPDATE (id) ON "+ident(schema, "blocked_items")+" TO "+ident(permission)); err != nil {
		t.Fatal(err)
	}

	m := Manifest{
		Version: 1,
		Service: "access_test",
		Roles: Roles{
			Login:      Role{Name: login, Attributes: RoleAttributes{Login: true, Inherit: true}},
			Permission: Role{Name: permission, Managed: true, Attributes: RoleAttributes{Inherit: true}},
		},
		Grants: Grants{
			Schemas:   map[string][]string{schema: {"USAGE"}},
			Tables:    map[string][]string{schema + ".items": {"SELECT"}},
			Sequences: map[string][]string{schema + ".item_seq": {"USAGE"}},
			Functions: []FunctionGrant{{Schema: schema, Name: "echo", Arguments: []string{"text"}, Privileges: []string{"EXECUTE"}}},
		},
		RLS: map[string]RLSRequirement{
			schema + ".items": {Enabled: true, Forced: true, RequiredPolicies: []string{"access_test_policy"}},
		},
		Forbidden: Forbidden{
			Privileges: []string{"TRIGGER"},
			Mutations:  []string{schema + ".blocked_items"},
		},
	}
	sql, err := RenderSQL(m)
	if err != nil {
		t.Fatal(err)
	}
	run := pgmigrate.Runner(pool)
	first := fstest.MapFS{"001_access.sql": {Data: sql}}
	if err := run(ctx, []appkit.MigrationSet{{Schema: migrationSchema, FS: first, Module: "dbaccess-test"}}); err != nil {
		t.Fatalf("execute rendered SQL: %v\n%s", err, sql)
	}
	missingPolicy := m
	missingPolicy.RLS = map[string]RLSRequirement{
		schema + ".missing_items": {Enabled: true, Forced: true, RequiredPolicies: []string{"missing_policy"}},
	}
	missingSQL, err := RenderSQL(missingPolicy)
	if err != nil {
		t.Fatal(err)
	}
	withMissing := fstest.MapFS{
		"001_access.sql":         {Data: sql},
		"002_missing_policy.sql": {Data: missingSQL},
	}
	if err := run(ctx, []appkit.MigrationSet{{Schema: migrationSchema, FS: withMissing, Module: "dbaccess-test"}}); err == nil {
		t.Fatal("rendered migration accepted a missing required RLS policy")
	}
	var missingApplied int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+ident(migrationSchema, "schema_migrations")+" WHERE version='002_missing_policy.sql'").Scan(&missingApplied); err != nil {
		t.Fatal(err)
	}
	if missingApplied != 0 {
		t.Fatal("failed RLS policy migration was recorded as applied")
	}
	var missingRLSEnabled, missingRLSForced bool
	if err := pool.QueryRow(ctx, `SELECT c.relrowsecurity, c.relforcerowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname='missing_items'`, schema).Scan(&missingRLSEnabled, &missingRLSForced); err != nil {
		t.Fatal(err)
	}
	if missingRLSEnabled || missingRLSForced {
		t.Fatalf("failed policy assertion did not roll back RLS DDL: enabled=%t forced=%t", missingRLSEnabled, missingRLSForced)
	}

	missingDependency := m
	missingDependency.Grants.Tables = map[string][]string{schema + ".not_created": {"SELECT"}}
	missingDependency.RLS = nil
	missingDependency.Forbidden = Forbidden{}
	dependencySQL, err := RenderSQL(missingDependency)
	if err != nil {
		t.Fatal(err)
	}
	dependencyFS := fstest.MapFS{"001_access_before_objects.sql": {Data: dependencySQL}}
	if err := run(ctx, []appkit.MigrationSet{{Schema: dependencyMigrationSchema, FS: dependencyFS, Module: "dbaccess-dependency-test"}}); err == nil {
		t.Fatal("access migration unexpectedly ran before its referenced objects existed")
	}
	var dependencyApplied int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+ident(dependencyMigrationSchema, "schema_migrations")+" WHERE version='001_access_before_objects.sql'").Scan(&dependencyApplied); err != nil {
		t.Fatal(err)
	}
	if dependencyApplied != 0 {
		t.Fatal("failed dependency migration was recorded as applied")
	}

	var canSelect, canUpdate, canTrigger bool
	if err := pool.QueryRow(ctx, "SELECT has_table_privilege($1, $2, 'SELECT'), has_table_privilege($1, $2, 'UPDATE'), has_table_privilege($1, $2, 'TRIGGER')", login, schema+".items").Scan(&canSelect, &canUpdate, &canTrigger); err != nil {
		t.Fatal(err)
	}
	if !canSelect || canUpdate || canTrigger {
		t.Fatalf("effective table privileges: select=%t update=%t trigger=%t", canSelect, canUpdate, canTrigger)
	}
	var canUpdateBlocked, canTriggerBlocked bool
	if err := pool.QueryRow(ctx, "SELECT has_table_privilege($1, $2, 'UPDATE'), has_table_privilege($1, $2, 'TRIGGER')", login, schema+".blocked_items").Scan(&canUpdateBlocked, &canTriggerBlocked); err != nil {
		t.Fatal(err)
	}
	if canUpdateBlocked || canTriggerBlocked {
		t.Fatalf("negative-only forbidden object retained privileges: update=%t trigger=%t", canUpdateBlocked, canTriggerBlocked)
	}
	var canInsertBlockedColumn, canUpdateBlockedColumn bool
	if err := pool.QueryRow(ctx, "SELECT has_column_privilege($1, $2, 'id', 'INSERT'), has_column_privilege($1, $2, 'id', 'UPDATE')", login, schema+".blocked_items").Scan(&canInsertBlockedColumn, &canUpdateBlockedColumn); err != nil {
		t.Fatal(err)
	}
	if canInsertBlockedColumn || canUpdateBlockedColumn {
		t.Fatalf("forbidden mutation retained column privileges: insert=%t update=%t", canInsertBlockedColumn, canUpdateBlockedColumn)
	}
	var rlsEnabled, rlsForced, policyExists bool
	if err := pool.QueryRow(ctx, `
SELECT c.relrowsecurity, c.relforcerowsecurity,
       EXISTS (SELECT 1 FROM pg_policies p WHERE p.schemaname=$1 AND p.tablename='items' AND p.policyname='access_test_policy')
FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname=$1 AND c.relname='items'`, schema).Scan(&rlsEnabled, &rlsForced, &policyExists); err != nil {
		t.Fatal(err)
	}
	if !rlsEnabled || !rlsForced || !policyExists {
		t.Fatalf("RLS contract: enabled=%t forced=%t policy=%t", rlsEnabled, rlsForced, policyExists)
	}
	var roleLogin, superuser, bypassRLS, createDB, createRole bool
	if err := pool.QueryRow(ctx, "SELECT rolcanlogin, rolsuper, rolbypassrls, rolcreatedb, rolcreaterole FROM pg_roles WHERE rolname=$1", permission).Scan(&roleLogin, &superuser, &bypassRLS, &createDB, &createRole); err != nil {
		t.Fatal(err)
	}
	if roleLogin || superuser || bypassRLS || createDB || createRole {
		t.Fatalf("unsafe permission role attributes: login=%t super=%t bypass=%t createdb=%t createrole=%t", roleLogin, superuser, bypassRLS, createDB, createRole)
	}
}

func TestRenderSQLConcurrentRoleCreation(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过 PostgreSQL 集成测试")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	suffix := fmt.Sprintf("%d_%d", time.Now().UnixNano(), integrationSequence.Add(1))
	login := "access_race_login_" + suffix
	permission := "access_race_perm_" + suffix
	migrationSchemas := []string{"access_race_a_" + suffix, "access_race_b_" + suffix}
	ident := func(name string) string { return pgx.Identifier{name}.Sanitize() }
	t.Cleanup(func() {
		for _, schema := range migrationSchemas {
			_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+ident(schema)+" CASCADE")
		}
		_, _ = pool.Exec(ctx, "DROP ROLE IF EXISTS "+ident(login))
		_, _ = pool.Exec(ctx, "DROP ROLE IF EXISTS "+ident(permission))
	})
	if _, err := pool.Exec(ctx, "CREATE ROLE "+ident(login)+" LOGIN INHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE"); err != nil {
		t.Fatal(err)
	}
	m := Manifest{
		Version: 1,
		Service: "access_race",
		Roles: Roles{
			Login:      Role{Name: login, Attributes: RoleAttributes{Login: true, Inherit: true}},
			Permission: Role{Name: permission, Managed: true, Attributes: RoleAttributes{Inherit: true}},
		},
	}
	sql, err := RenderSQL(m)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	run := pgmigrate.Runner(pool)
	for _, migrationSchema := range migrationSchemas {
		migrationSchema := migrationSchema
		go func() {
			ready.Done()
			<-start
			err := run(ctx, []appkit.MigrationSet{{
				Schema: migrationSchema,
				FS:     fstest.MapFS{"001_access.sql": {Data: sql}},
				Module: "dbaccess-role-race",
			}})
			errs <- err
		}()
	}
	ready.Wait()
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent role creation failed: %v", err)
		}
	}
}
