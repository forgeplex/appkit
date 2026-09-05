package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func catalogTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is required for scratch database catalog tests")
	}
	// fromScratch uses this connection only to create/drop its exact randomly
	// named database. DDL replay never targets the supplied database, including
	// when CI supplies a TCP service instead of the local private Unix socket.
	return dsn
}

func catalogFromDDL(t *testing.T, schema, ddl string) (string, error) {
	t.Helper()
	dsn := catalogTestDSN(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	return fromScratch(ctx, dsn, schema, fstest.MapFS{"0001_fixture.sql": &fstest.MapFile{Data: []byte(ddl)}})
}

const catalogFixture = `
CREATE TYPE public.status AS ENUM ('ready', 'done');
COMMENT ON TYPE public.status IS 'workflow';
CREATE FUNCTION public.z_identity(value integer) RETURNS integer LANGUAGE sql IMMUTABLE AS 'SELECT value';
CREATE FUNCTION public.a_default(value integer DEFAULT public.z_identity(2)) RETURNS integer LANGUAGE sql AS 'SELECT value';
CREATE FUNCTION public.touch() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN NEW.updated_at := now(); RETURN NEW; END$$;
COMMENT ON FUNCTION public.touch() IS 'touch timestamp';
CREATE TABLE public.z_parent (
 id bigint GENERATED ALWAYS AS IDENTITY (START WITH 7 INCREMENT BY 3 CACHE 4 MAXVALUE 100000 CYCLE),
 child_id bigint,
 tenant_id text CONSTRAINT tenant_required NOT NULL,
 state public.status NOT NULL DEFAULT 'ready',
 refs jsonb NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now(),
 CONSTRAINT parent_pk PRIMARY KEY(id),
 CONSTRAINT refs_shape CHECK (CASE WHEN jsonb_typeof(refs)='object' THEN
  refs ? 'merchant_account_id' AND refs-ARRAY['merchant_account_id']::text[]='{}'::jsonb
  AND jsonb_typeof(refs->'merchant_account_id')='string'
  AND refs->>'merchant_account_id' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  AND refs->>'merchant_account_id'<>'00000000-0000-0000-0000-000000000000'
 ELSE false END IS TRUE)
);
CREATE TABLE public.a_child(id bigint PRIMARY KEY, parent_id bigint, value integer DEFAULT public.a_default(), CONSTRAINT child_parent_fk FOREIGN KEY(parent_id) REFERENCES public.z_parent(id) DEFERRABLE INITIALLY DEFERRED);
ALTER TABLE public.z_parent ADD CONSTRAINT parent_child_fk FOREIGN KEY(child_id) REFERENCES public.a_child(id);
ALTER TABLE public.z_parent ADD CONSTRAINT tenant_length CHECK (length(tenant_id)<100) NOT VALID;
CREATE UNIQUE INDEX parent_tenant_child ON public.z_parent(tenant_id,child_id) NULLS NOT DISTINCT WHERE child_id IS NOT NULL;
CREATE INDEX parent_account ON public.z_parent(tenant_id,((refs->>'merchant_account_id')::uuid),id DESC);
CREATE INDEX parent_refs ON public.z_parent USING gin(refs jsonb_path_ops);
CREATE VIEW public.z_base AS SELECT id,state,refs FROM public.z_parent;
CREATE VIEW public.a_top AS SELECT id,state,refs FROM public.z_base;
COMMENT ON VIEW public.a_top IS 'nested view';
COMMENT ON COLUMN public.a_top.refs IS 'nested refs';
CREATE TRIGGER parent_touch BEFORE UPDATE ON public.z_parent FOR EACH ROW EXECUTE FUNCTION public.touch();
ALTER TABLE public.z_parent ENABLE ALWAYS TRIGGER parent_touch;
CREATE TRIGGER child_touch BEFORE DELETE ON public.a_child FOR EACH ROW EXECUTE FUNCTION public.touch();
ALTER TABLE public.a_child DISABLE TRIGGER child_touch;
ALTER TABLE public.z_parent ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.z_parent FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON public.z_parent USING (tenant_id=current_setting('app.tenant_id')) WITH CHECK (tenant_id=current_setting('app.tenant_id'));
CREATE POLICY ready_only ON public.z_parent AS RESTRICTIVE FOR SELECT TO PUBLIC USING (state='ready');
COMMENT ON POLICY tenant ON public.z_parent IS 'tenant isolation';
COMMENT ON TABLE public.z_parent IS E'order\nreference \'snapshot\'';
COMMENT ON COLUMN public.z_parent.refs IS E'backslash \\ and quote \' retained';
CREATE TABLE public."odd""table"("odd""column" text DEFAULT E'quoted\'value');
INSERT INTO public.z_parent(tenant_id,refs) VALUES ('a','{"merchant_account_id":"11111111-1111-4111-8111-111111111111"}');
`

func TestCatalogRoundTripDeterministic(t *testing.T) {
	first, err := catalogFromDDL(t, "public", catalogFixture)
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalogFromDDL(t, "public", catalogFixture)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("fresh databases produced different catalog snapshots")
	}
	replayed, err := catalogFromDDL(t, "public", first)
	if err != nil {
		t.Fatal("catalog snapshot did not replay: ", err)
	}
	if first != replayed {
		t.Fatalf("catalog roundtrip changed structure\nfirst:\n%s\nreplayed:\n%s", first, replayed)
	}
	for _, want := range []string{"CREATE TYPE \"public\".\"status\"", "GENERATED ALWAYS AS IDENTITY", "START WITH 7 INCREMENT BY 3", "CACHE 4 CYCLE", "merchant_account_id", "jsonb_path_ops", "NULLS NOT DISTINCT", "DEFERRABLE INITIALLY DEFERRED", "NOT VALID", "FORCE ROW LEVEL SECURITY", "AS RESTRICTIVE FOR SELECT TO PUBLIC", "ENABLE ALWAYS TRIGGER", "DISABLE TRIGGER", "COMMENT ON COLUMN", "CONSTRAINT \"tenant_required\" NOT NULL", `"odd""table"`, `E'backslash \\ and quote '' retained'`} {
		if !strings.Contains(first, want) {
			t.Errorf("snapshot missing %q", want)
		}
	}
	if strings.Contains(first, "schema_migrations") || strings.Contains(first, "appkit_sqlc_") || strings.Contains(first, "setval(") {
		t.Fatal("snapshot includes metadata, random scratch identity or sequence runtime state")
	}
	if strings.Index(first, `CREATE VIEW "public"."z_base"`) > strings.Index(first, `CREATE VIEW "public"."a_top"`) {
		t.Fatal("dependent views were alphabetized instead of dependency ordered")
	}
	if strings.Index(first, `CREATE TABLE "public"."z_parent"`) > strings.Index(first, `ADD CONSTRAINT "child_parent_fk"`) {
		t.Fatal("foreign key emitted before all referenced tables")
	}
	if os.Getenv("APPKIT_SCHEMA_TEST_SQLC") == "1" {
		checkCatalogSQLC(t, first)
	}
}

func checkCatalogSQLC(t *testing.T, schema string) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"schema.sql": schema,
		"query.sql":  "-- name: ListParentRefs :many\nSELECT id,state,refs FROM public.z_parent WHERE refs @> sqlc.arg(ref_filter)::jsonb;\n",
		"sqlc.yaml":  "version: \"2\"\nsql:\n  - engine: postgresql\n    schema: schema.sql\n    queries: query.sql\n    gen:\n      go:\n        package: sqlc\n        out: generated\n        sql_package: pgx/v5\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), "go", "run", "github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0", "generate", "-f", filepath.Join(dir, "sqlc.yaml"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pinned sqlc rejected current DDL: %v\n%s", err, output)
	}
}

func TestCatalogFixedSchemaAndExplicitUnsupported(t *testing.T) {
	fixed, err := catalogFromDDL(t, "example", `CREATE TYPE example.status AS ENUM ('a'); CREATE TABLE example.items(id integer PRIMARY KEY,state example.status,refs jsonb);`)
	if err != nil || !strings.Contains(fixed, `CREATE TABLE "example"."items"`) || !strings.Contains(fixed, `"state" example.status`) {
		t.Fatalf("fixed schema qualification: %v\n%s", err, fixed)
	}
	for _, tc := range []struct{ name, ddl, feature string }{
		{"domain", "CREATE DOMAIN public.positive AS integer CHECK(VALUE>0);", "custom types"},
		{"sequence", "CREATE SEQUENCE public.counter;", "non-identity sequences"},
		{"serial", "CREATE TABLE public.items(id serial);", "non-identity sequences"},
		{"partition", "CREATE TABLE public.items(id integer) PARTITION BY RANGE(id);", "partitioned"},
		{"inherit", "CREATE TABLE public.parent(id integer); CREATE TABLE public.child() INHERITS(public.parent);", "inherited"},
		{"generated", "CREATE TABLE public.items(id integer, doubled integer GENERATED ALWAYS AS (id*2) STORED);", "generated columns"},
		{"collation", `CREATE TABLE public.items(value text COLLATE "C");`, "collation"},
		{"materialized", "CREATE MATERIALIZED VIEW public.items AS SELECT 1 id;", "materialized"},
		{"view_option", "CREATE VIEW public.items WITH (security_barrier=true) AS SELECT 1 id;", "storage options"},
		{"exclusion", "CREATE TABLE public.items(value int4range, EXCLUDE USING gist(value WITH &&));", "constraint types"},
		{"row_array", "CREATE TABLE public.items(id integer); CREATE TABLE public.arr(items public.items[]);", "composite column types"},
		{"foreign_view", "CREATE SCHEMA other; CREATE TABLE other.items(id integer); CREATE VIEW public.items AS SELECT * FROM other.items;", "foreign-schema"},
		{"metadata_view", "CREATE VIEW public.metadata AS SELECT * FROM public.schema_migrations;", "metadata view"},
		{"foreign_function", "CREATE SCHEMA other; CREATE FUNCTION other.value() RETURNS integer LANGUAGE sql AS 'SELECT 1'; CREATE TABLE public.items(value integer DEFAULT other.value());", "cross-schema routine"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := catalogFromDDL(t, "public", tc.ddl)
			if err == nil || body != "" || !strings.Contains(err.Error(), tc.feature) {
				t.Fatalf("unsupported feature was omitted or error was obscured: %q, %v", body, err)
			}
		})
	}
}

func TestCatalogDependencyOrderAndQuoting(t *testing.T) {
	items := []catObject{{oid: 20, key: "a"}, {oid: 99, key: "z"}, {oid: 6, key: "m"}}
	ordered, err := orderCatalogObjects(items, []catDependency{{from: 20, to: 99}, {from: 99, to: 6}, {from: 6, to: 1234}})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, item := range ordered {
		keys = append(keys, item.key)
	}
	if !reflect.DeepEqual(keys, []string{"m", "z", "a"}) {
		t.Fatal(keys)
	}
	if _, err := orderCatalogObjects(items, []catDependency{{from: 20, to: 99}, {from: 99, to: 20}}); err == nil {
		t.Fatal("cycle did not fail closed")
	}
	if qualified(`a"b`, `c.d`) != `"a""b"."c.d"` || quoteLiteral("a'\\b") != `E'a''\\b'` {
		t.Fatal("catalog identifiers or literals escaped incorrectly")
	}
}
