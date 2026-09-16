package dbaccess

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

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
	login := "access_login_" + suffix
	permission := "access_perm_" + suffix
	ident := func(parts ...string) string { return pgx.Identifier(parts).Sanitize() }
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+ident(schema)+" CASCADE")
		_, _ = pool.Exec(ctx, "DROP ROLE IF EXISTS "+ident(login))
		_, _ = pool.Exec(ctx, "DROP ROLE IF EXISTS "+ident(permission))
	})
	if _, err := pool.Exec(ctx, "CREATE ROLE "+ident(login)+" LOGIN INHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+ident(schema)+"; CREATE TABLE "+ident(schema, "items")+" (id bigint PRIMARY KEY, value text); CREATE SEQUENCE "+ident(schema, "item_seq")+"; CREATE FUNCTION "+ident(schema, "echo")+"(text) RETURNS text LANGUAGE sql IMMUTABLE AS 'SELECT $1'"); err != nil {
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
	}
	sql, err := RenderSQL(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("execute rendered SQL: %v\n%s", err, sql)
	}

	var canSelect, canUpdate bool
	if err := pool.QueryRow(ctx, "SELECT has_table_privilege($1, $2, 'SELECT'), has_table_privilege($1, $2, 'UPDATE')", login, schema+".items").Scan(&canSelect, &canUpdate); err != nil {
		t.Fatal(err)
	}
	if !canSelect || canUpdate {
		t.Fatalf("effective table privileges: select=%t update=%t", canSelect, canUpdate)
	}
	var roleLogin, superuser, bypassRLS, createDB, createRole bool
	if err := pool.QueryRow(ctx, "SELECT rolcanlogin, rolsuper, rolbypassrls, rolcreatedb, rolcreaterole FROM pg_roles WHERE rolname=$1", permission).Scan(&roleLogin, &superuser, &bypassRLS, &createDB, &createRole); err != nil {
		t.Fatal(err)
	}
	if roleLogin || superuser || bypassRLS || createDB || createRole {
		t.Fatalf("unsafe permission role attributes: login=%t super=%t bypass=%t createdb=%t createrole=%t", roleLogin, superuser, bypassRLS, createDB, createRole)
	}
}
