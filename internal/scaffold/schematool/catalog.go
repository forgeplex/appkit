package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// catalog is a code-generation/review snapshot, not a deployment backup. In
// particular it does not export owners, ACLs, data, or sequence runtime state.
// Unsupported structure is an error, never a partially successful snapshot.
func catalog(ctx context.Context, pool *pgxpool.Pool, schema string) (string, error) {
	if schema == "" || strings.ContainsRune(schema, 0) {
		return "", errors.New("catalog requires a nonempty schema identifier")
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return "", safeDBError("begin catalog snapshot", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	// Deparsers must not omit qualification based on a caller's search_path or
	// format date constants using that caller's timezone/date style.
	if _, err := tx.Exec(ctx, `SET LOCAL search_path = pg_catalog;
SET LOCAL timezone = 'UTC'; SET LOCAL datestyle = 'ISO, YMD';
SET LOCAL intervalstyle = 'postgres'; SET LOCAL standard_conforming_strings = on;
SET LOCAL quote_all_identifiers = off; SET LOCAL extra_float_digits = 3;
SET LOCAL bytea_output = 'hex'; SET LOCAL lc_monetary = 'C'`); err != nil {
		return "", safeDBError("configure catalog deparsers", err)
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname=$1)`, schema).Scan(&exists); err != nil {
		return "", safeDBError("find catalog schema", err)
	}
	if !exists {
		return "", errors.New("catalog schema does not exist")
	}
	if err := rejectCatalogFeatures(ctx, tx, schema); err != nil {
		return "", err
	}
	var out strings.Builder
	emit := func(s string) { out.WriteString(strings.TrimSuffix(strings.TrimSpace(s), ";") + ";\n\n") }
	emit("CREATE SCHEMA IF NOT EXISTS " + quoteName(schema))
	// Quoted SQL/PLpgSQL bodies can reference tables created later, as in a dump.
	// SQL-standard parsed bodies and table-row argument types are rejected below.
	emit("SET check_function_bodies = false")
	if err := catalogEnums(ctx, tx, schema, emit); err != nil {
		return "", err
	}
	if err := catalogFunctions(ctx, tx, schema, emit); err != nil {
		return "", err
	}
	relations, err := catalogRows[catRelation](ctx, tx, `
SELECT c.oid,c.relname,c.relkind::text,c.relpersistence::text,c.relrowsecurity,c.relforcerowsecurity,
       obj_description(c.oid,'pg_class'),
       CASE WHEN c.relkind='v' THEN pg_get_viewdef(c.oid,false) ELSE '' END
FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname=$1 AND c.relkind IN ('r','v') AND c.relname<>'schema_migrations'
ORDER BY c.relname COLLATE "C"`, schema)
	if err != nil {
		return "", err
	}
	for i := range relations {
		r := &relations[i]
		r.columns, err = catalogColumns(ctx, tx, r.oid)
		if err != nil {
			return "", err
		}
		if r.kind != "r" {
			continue
		}
		var cols []string
		for _, col := range r.columns {
			definition := quoteName(col.name) + " " + col.typ
			if col.identity != "" {
				identity, err := catalogIdentity(ctx, tx, r.oid, col.number, col.identity)
				if err != nil {
					return "", err
				}
				definition += " " + identity
			} else if col.def != "" {
				definition += " DEFAULT " + col.def
			}
			if col.notNull {
				if col.notNullName != "" {
					definition += " CONSTRAINT " + quoteName(col.notNullName)
				}
				definition += " NOT NULL"
			}
			cols = append(cols, "    "+definition)
		}
		prefix := "CREATE TABLE "
		if r.persistence == "u" {
			prefix = "CREATE UNLOGGED TABLE "
		}
		emit(prefix + qualified(schema, r.name) + " (\n" + strings.Join(cols, ",\n") + "\n)")
	}
	// PK/UNIQUE must exist before any FK, even when table names sort oppositely
	// or the reference graph is cyclic. Constraint-owned indexes are not repeated.
	constraints, err := catalogRows[catConstraint](ctx, tx, `
SELECT c.relname,k.conname,k.contype::text,pg_get_constraintdef(k.oid,false)
FROM pg_constraint k JOIN pg_class c ON c.oid=k.conrelid
JOIN pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname=$1 AND c.relname<>'schema_migrations' AND k.contype IN ('p','u','c','f')
ORDER BY CASE WHEN k.contype='f' THEN 1 ELSE 0 END,c.relname COLLATE "C",k.conname COLLATE "C"`, schema)
	if err != nil {
		return "", err
	}
	for _, c := range constraints {
		emit("ALTER TABLE " + qualified(schema, c.table) + " ADD CONSTRAINT " + quoteName(c.name) + " " + c.def)
	}
	indexes, err := catalogRows[catDefinition](ctx, tx, `
SELECT pg_get_indexdef(i.indexrelid,0,false)
FROM pg_index i JOIN pg_class c ON c.oid=i.indrelid
JOIN pg_class idx ON idx.oid=i.indexrelid JOIN pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname=$1 AND c.relname<>'schema_migrations'
AND NOT EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conindid=i.indexrelid)
ORDER BY c.relname COLLATE "C",idx.relname COLLATE "C"`, schema)
	if err != nil {
		return "", err
	}
	for _, index := range indexes {
		emit(index.def)
	}
	if err := catalogViews(ctx, tx, schema, relations, emit); err != nil {
		return "", err
	}
	for _, r := range relations {
		name := qualified(schema, r.name)
		if r.kind == "r" {
			enabled, forced := "DISABLE", "NO FORCE"
			if r.rls {
				enabled = "ENABLE"
			}
			if r.force {
				forced = "FORCE"
			}
			emit("ALTER TABLE " + name + " " + enabled + " ROW LEVEL SECURITY")
			emit("ALTER TABLE " + name + " " + forced + " ROW LEVEL SECURITY")
		}
		kind := "TABLE"
		if r.kind == "v" {
			kind = "VIEW"
		}
		if r.comment != nil {
			emit("COMMENT ON " + kind + " " + name + " IS " + quoteLiteral(*r.comment))
		}
		for _, col := range r.columns {
			if r.kind == "v" && col.def != "" {
				emit("ALTER VIEW " + name + " ALTER COLUMN " + quoteName(col.name) + " SET DEFAULT " + col.def)
			}
			if col.comment != nil {
				emit("COMMENT ON COLUMN " + name + "." + quoteName(col.name) + " IS " + quoteLiteral(*col.comment))
			}
		}
	}
	if err := catalogTriggersPolicies(ctx, tx, schema, emit); err != nil {
		return "", err
	}
	return out.String(), nil
}

func quoteName(name string) string         { return pgx.Identifier{name}.Sanitize() }
func qualified(schema, name string) string { return pgx.Identifier{schema, name}.Sanitize() }
func quoteLiteral(value string) string {
	return "E'" + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), "'", "''") + "'"
}

type catRelation struct {
	oid                     uint32
	name, kind, persistence string
	rls, force              bool
	comment                 *string
	view                    string
	columns                 []catColumn
}

type catColumn struct {
	number                     int16
	name, typ                  string
	notNull                    bool
	def, identity, notNullName string
	comment                    *string
}
type catConstraint struct{ table, name, kind, def string }
type catDefinition struct{ def string }

// Rows are scanned explicitly so this file does not depend on the destination
// domain's internal packages, struct-tag conventions, or new AppKit APIs.
func catalogRows[T any](ctx context.Context, tx pgx.Tx, query string, args ...any) ([]T, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, safeDBError("query catalog", err)
	}
	result, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (T, error) {
		var value T
		var scanErr error
		switch v := any(&value).(type) {
		case *catRelation:
			scanErr = row.Scan(&v.oid, &v.name, &v.kind, &v.persistence, &v.rls, &v.force, &v.comment, &v.view)
		case *catColumn:
			scanErr = row.Scan(&v.number, &v.name, &v.typ, &v.notNull, &v.def, &v.identity, &v.notNullName, &v.comment)
		case *catConstraint:
			scanErr = row.Scan(&v.table, &v.name, &v.kind, &v.def)
		case *catDefinition:
			scanErr = row.Scan(&v.def)
		case *catEnum:
			scanErr = row.Scan(&v.name, &v.labels, &v.comment)
		case *catObject:
			scanErr = row.Scan(&v.oid, &v.key, &v.def, &v.comment)
		case *catDependency:
			scanErr = row.Scan(&v.from, &v.to)
		case *catTrigger:
			scanErr = row.Scan(&v.table, &v.name, &v.def, &v.enabled, &v.comment)
		case *catPolicy:
			scanErr = row.Scan(&v.table, &v.name, &v.command, &v.permissive, &v.roles, &v.using, &v.check, &v.comment)
		default:
			scanErr = errors.New("unsupported catalog row record")
		}
		return value, scanErr
	})
	if err != nil {
		return nil, safeDBError("read catalog rows", err)
	}
	return result, nil
}

func catalogColumns(ctx context.Context, tx pgx.Tx, oid uint32) ([]catColumn, error) {
	return catalogRows[catColumn](ctx, tx, `
SELECT a.attnum,a.attname,format_type(a.atttypid,a.atttypmod),a.attnotnull,
       COALESCE(pg_get_expr(d.adbin,d.adrelid,false),''),a.attidentity::text,
       COALESCE((SELECT k.conname FROM pg_constraint k WHERE k.conrelid=a.attrelid
                 AND k.contype='n' AND k.conkey=ARRAY[a.attnum]),''),
       col_description(a.attrelid,a.attnum)
FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
WHERE a.attrelid=$1 AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`, oid)
}

func catalogIdentity(ctx context.Context, tx pgx.Tx, oid uint32, attnum int16, mode string) (string, error) {
	var schema, name string
	var start, increment, min, max, cache int64
	var cycle bool
	err := tx.QueryRow(ctx, `
SELECT n.nspname,c.relname,s.seqstart,s.seqincrement,s.seqmin,s.seqmax,s.seqcache,s.seqcycle
FROM pg_depend d JOIN pg_class c ON c.oid=d.objid JOIN pg_namespace n ON n.oid=c.relnamespace
JOIN pg_sequence s ON s.seqrelid=c.oid
WHERE d.classid='pg_class'::regclass AND d.refclassid='pg_class'::regclass
AND d.refobjid=$1 AND d.refobjsubid=$2 AND d.deptype='i'`, oid, attnum).Scan(&schema, &name, &start, &increment, &min, &max, &cache, &cycle)
	if err != nil {
		return "", safeDBError("read identity sequence definition", err)
	}
	identity := "ALWAYS"
	if mode == "d" {
		identity = "BY DEFAULT"
	}
	cycling := "NO CYCLE"
	if cycle {
		cycling = "CYCLE"
	}
	return fmt.Sprintf("GENERATED %s AS IDENTITY (SEQUENCE NAME %s START WITH %d INCREMENT BY %d MINVALUE %d MAXVALUE %d CACHE %d %s)", identity, qualified(schema, name), start, increment, min, max, cache, cycling), nil
}

type catEnum struct {
	name    string
	labels  []string
	comment *string
}

func catalogEnums(ctx context.Context, tx pgx.Tx, schema string, emit func(string)) error {
	items, err := catalogRows[catEnum](ctx, tx, `
SELECT t.typname,COALESCE(array_agg(e.enumlabel ORDER BY e.enumsortorder) FILTER (WHERE e.oid IS NOT NULL),'{}'),obj_description(t.oid,'pg_type')
FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace LEFT JOIN pg_enum e ON e.enumtypid=t.oid
WHERE n.nspname=$1 AND t.typtype='e' GROUP BY t.oid,t.typname ORDER BY t.typname COLLATE "C"`, schema)
	if err != nil {
		return err
	}
	for _, item := range items {
		labels := make([]string, len(item.labels))
		for i, label := range item.labels {
			labels[i] = quoteLiteral(label)
		}
		emit("CREATE TYPE " + qualified(schema, item.name) + " AS ENUM (" + strings.Join(labels, ", ") + ")")
		if item.comment != nil {
			emit("COMMENT ON TYPE " + qualified(schema, item.name) + " IS " + quoteLiteral(*item.comment))
		}
	}
	return nil
}

type catObject struct {
	oid      uint32
	key, def string
	comment  *string
}
type catDependency struct{ from, to uint32 }

func catalogFunctions(ctx context.Context, tx pgx.Tx, schema string, emit func(string)) error {
	items, err := catalogRows[catObject](ctx, tx, `
SELECT p.oid,quote_ident(n.nspname)||'.'||quote_ident(p.proname)||'('||pg_get_function_identity_arguments(p.oid)||')',
       pg_get_functiondef(p.oid),obj_description(p.oid,'pg_proc')
FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
WHERE n.nspname=$1 ORDER BY p.proname COLLATE "C",pg_get_function_identity_arguments(p.oid) COLLATE "C"`, schema)
	if err != nil {
		return err
	}
	deps, err := catalogRows[catDependency](ctx, tx, `
SELECT d.objid,d.refobjid FROM pg_depend d JOIN pg_proc p ON p.oid=d.objid
JOIN pg_namespace n ON n.oid=p.pronamespace
WHERE n.nspname=$1 AND d.classid='pg_proc'::regclass AND d.refclassid='pg_proc'::regclass`, schema)
	if err != nil {
		return err
	}
	ordered, err := orderCatalogObjects(items, deps)
	if err != nil {
		return err
	}
	for _, item := range ordered {
		emit(item.def)
		if item.comment != nil {
			emit("COMMENT ON FUNCTION " + item.key + " IS " + quoteLiteral(*item.comment))
		}
	}
	return nil
}

func catalogViews(ctx context.Context, tx pgx.Tx, schema string, relations []catRelation, emit func(string)) error {
	var items []catObject
	for _, r := range relations {
		if r.kind != "v" {
			continue
		}
		columns := make([]string, len(r.columns))
		for i, col := range r.columns {
			columns[i] = quoteName(col.name)
		}
		items = append(items, catObject{oid: r.oid, key: r.name, def: "CREATE VIEW " + qualified(schema, r.name) + " (" + strings.Join(columns, ", ") + ") AS\n" + r.view})
	}
	deps, err := catalogRows[catDependency](ctx, tx, `
SELECT DISTINCT r.ev_class,d.refobjid FROM pg_rewrite r JOIN pg_class c ON c.oid=r.ev_class
JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_depend d ON d.objid=r.oid
WHERE n.nspname=$1 AND c.relkind='v' AND d.classid='pg_rewrite'::regclass
AND d.refclassid='pg_class'::regclass AND d.refobjid<>r.ev_class`, schema)
	if err != nil {
		return err
	}
	ordered, err := orderCatalogObjects(items, deps)
	if err != nil {
		return err
	}
	for _, item := range ordered {
		emit(item.def)
	}
	return nil
}

func orderCatalogObjects(items []catObject, deps []catDependency) ([]catObject, error) {
	byID := make(map[uint32]catObject, len(items))
	for _, item := range items {
		byID[item.oid] = item
	}
	parents := map[uint32][]uint32{}
	for _, dep := range deps {
		if _, ok := byID[dep.to]; ok {
			parents[dep.from] = append(parents[dep.from], dep.to)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].key < items[j].key })
	for id := range parents {
		sort.Slice(parents[id], func(i, j int) bool { return byID[parents[id][i]].key < byID[parents[id][j]].key })
	}
	var result []catObject
	state := map[uint32]int{}
	var visit func(uint32) error
	visit = func(id uint32) error {
		if state[id] == 2 {
			return nil
		}
		if state[id] == 1 {
			return errors.New("catalog cannot export a cyclic function/view dependency")
		}
		state[id] = 1
		for _, parent := range parents[id] {
			if err := visit(parent); err != nil {
				return err
			}
		}
		state[id] = 2
		result = append(result, byID[id])
		return nil
	}
	for _, item := range items {
		if err := visit(item.oid); err != nil {
			return nil, err
		}
	}
	return result, nil
}

type catTrigger struct {
	table, name, def, enabled string
	comment                   *string
}
type catPolicy struct {
	table, name, command string
	permissive           bool
	roles                []string
	using, check         string
	comment              *string
}

func catalogTriggersPolicies(ctx context.Context, tx pgx.Tx, schema string, emit func(string)) error {
	triggers, err := catalogRows[catTrigger](ctx, tx, `
SELECT c.relname,t.tgname,pg_get_triggerdef(t.oid,false),t.tgenabled::text,obj_description(t.oid,'pg_trigger')
FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname=$1 AND c.relname<>'schema_migrations' AND NOT t.tgisinternal
ORDER BY c.relname COLLATE "C",t.tgname COLLATE "C"`, schema)
	if err != nil {
		return err
	}
	for _, t := range triggers {
		emit(t.def)
		mode := map[string]string{"O": "ENABLE", "D": "DISABLE", "R": "ENABLE REPLICA", "A": "ENABLE ALWAYS"}[t.enabled]
		if mode == "" {
			return errors.New("unsupported catalog trigger state")
		}
		emit("ALTER TABLE " + qualified(schema, t.table) + " " + mode + " TRIGGER " + quoteName(t.name))
		if t.comment != nil {
			emit("COMMENT ON TRIGGER " + quoteName(t.name) + " ON " + qualified(schema, t.table) + " IS " + quoteLiteral(*t.comment))
		}
	}
	policies, err := catalogRows[catPolicy](ctx, tx, `
SELECT c.relname,p.polname,p.polcmd::text,p.polpermissive,
       ARRAY(SELECT CASE WHEN x=0 THEN 'PUBLIC' ELSE quote_ident(pg_get_userbyid(x)) END
             FROM unnest(p.polroles) x ORDER BY CASE WHEN x=0 THEN '' ELSE pg_get_userbyid(x)::text END COLLATE "C"),
       COALESCE(pg_get_expr(p.polqual,p.polrelid,false),''),COALESCE(pg_get_expr(p.polwithcheck,p.polrelid,false),''),obj_description(p.oid,'pg_policy')
FROM pg_policy p JOIN pg_class c ON c.oid=p.polrelid JOIN pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname=$1 AND c.relname<>'schema_migrations' ORDER BY c.relname COLLATE "C",p.polname COLLATE "C"`, schema)
	if err != nil {
		return err
	}
	for _, p := range policies {
		mode := "PERMISSIVE"
		if !p.permissive {
			mode = "RESTRICTIVE"
		}
		command := map[string]string{"*": "ALL", "r": "SELECT", "a": "INSERT", "w": "UPDATE", "d": "DELETE"}[p.command]
		if command == "" {
			return errors.New("unsupported catalog policy command")
		}
		definition := "CREATE POLICY " + quoteName(p.name) + " ON " + qualified(schema, p.table) + " AS " + mode + " FOR " + command + " TO " + strings.Join(p.roles, ", ")
		if p.using != "" {
			definition += " USING (" + p.using + ")"
		}
		if p.check != "" {
			definition += " WITH CHECK (" + p.check + ")"
		}
		emit(definition)
		if p.comment != nil {
			emit("COMMENT ON POLICY " + quoteName(p.name) + " ON " + qualified(schema, p.table) + " IS " + quoteLiteral(*p.comment))
		}
	}
	return nil
}

func rejectCatalogFeatures(ctx context.Context, tx pgx.Tx, schema string) error {
	// This is deliberately conservative. Adding a supported feature requires its
	// DDL renderer and roundtrip coverage, not deleting the corresponding guard.
	checks := []struct{ feature, query string }{
		{"partitioned, inherited, foreign, materialized, temporary or composite relations", `
SELECT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname=$1 AND c.relname<>'schema_migrations' AND
(c.relkind NOT IN ('r','v','i','S','t') OR c.relispartition OR c.relpersistence='t'
 OR EXISTS(SELECT 1 FROM pg_inherits h WHERE h.inhrelid=c.oid OR h.inhparent=c.oid)))`},
		{"relation storage options, tablespaces, non-heap access methods or replica identity", `
SELECT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace LEFT JOIN pg_am am ON am.oid=c.relam
WHERE n.nspname=$1 AND c.relname<>'schema_migrations' AND
(c.reloptions IS NOT NULL OR c.reltablespace<>0 OR (c.relkind='r' AND (am.amname<>'heap' OR c.relreplident<>'d'))))`},
		{"custom types other than enums and their arrays", `
SELECT EXISTS(SELECT 1 FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace
WHERE n.nspname=$1 AND t.typtype<>'e'
AND NOT EXISTS(SELECT 1 FROM pg_type base WHERE base.typarray=t.oid)
AND NOT EXISTS(SELECT 1 FROM pg_class c WHERE c.oid=t.typrelid AND c.relkind IN ('r','v')))`},
		{"non-identity sequences or identity sequence type overrides", `
SELECT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_sequence s ON s.seqrelid=c.oid
WHERE n.nspname=$1 AND c.relkind='S' AND NOT EXISTS(
 SELECT 1 FROM pg_depend d JOIN pg_attribute a ON a.attrelid=d.refobjid AND a.attnum=d.refobjsubid
 JOIN pg_class owner ON owner.oid=a.attrelid
 WHERE d.classid='pg_class'::regclass AND d.refclassid='pg_class'::regclass AND d.objid=c.oid
 AND d.deptype='i' AND a.attidentity IN ('a','d') AND a.atttypid=s.seqtypid AND owner.relnamespace=n.oid AND owner.relname<>'schema_migrations'))`},
		{"generated columns, custom column collation/storage or foreign/composite column types", `
SELECT EXISTS(SELECT 1 FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace
JOIN pg_type t ON t.oid=a.atttypid JOIN pg_namespace tn ON tn.oid=t.typnamespace
WHERE n.nspname=$1 AND c.relkind IN ('r','v') AND c.relname<>'schema_migrations' AND a.attnum>0 AND NOT a.attisdropped AND
(a.attgenerated<>'' OR a.attcollation<>t.typcollation OR a.attstorage<>t.typstorage OR a.attcompression<>''
 OR tn.nspname NOT IN ('pg_catalog',$1) OR t.typtype='c'
 OR EXISTS(SELECT 1 FROM pg_type element WHERE element.oid=t.typelem AND element.typtype='c')))`},
		{"unsupported constraint types, inherited NOT NULL, or foreign-schema/metadata foreign keys", `
SELECT EXISTS(SELECT 1 FROM pg_constraint k JOIN pg_class c ON c.oid=k.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname=$1 AND c.relname<>'schema_migrations' AND (k.contype NOT IN ('p','u','c','f','n','t')
 OR (k.contype='n' AND k.connoinherit) OR (k.contype='f' AND NOT EXISTS(
 SELECT 1 FROM pg_class r WHERE r.oid=k.confrelid AND r.relnamespace=n.oid AND r.relname<>'schema_migrations'))))`},
		{"invalid/clustered indexes or custom index collations/operator classes", `
SELECT EXISTS(SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indrelid JOIN pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname=$1 AND c.relname<>'schema_migrations' AND (NOT i.indisvalid OR NOT i.indisready OR i.indisclustered
 OR EXISTS(SELECT 1 FROM unnest(i.indcollation) x JOIN pg_collation co ON co.oid=x JOIN pg_namespace cn ON cn.oid=co.collnamespace WHERE cn.nspname<>'pg_catalog')
 OR EXISTS(SELECT 1 FROM unnest(i.indclass) x JOIN pg_opclass op ON op.oid=x JOIN pg_namespace ons ON ons.oid=op.opcnamespace WHERE ons.nspname<>'pg_catalog')))`},
		{"procedures/aggregates, non-SQL/PLpgSQL routines, parsed SQL bodies or row-type routine signatures", `
SELECT EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace JOIN pg_language l ON l.oid=p.prolang
WHERE n.nspname=$1 AND (p.prokind<>'f' OR l.lanname NOT IN ('sql','plpgsql') OR p.prosqlbody IS NOT NULL
 OR EXISTS(SELECT 1 FROM pg_type t JOIN pg_namespace tn ON tn.oid=t.typnamespace
 WHERE (t.oid=p.prorettype OR t.oid=ANY(COALESCE(p.proallargtypes,p.proargtypes::oid[])))
 AND (t.typtype='c' OR tn.nspname NOT IN ('pg_catalog',$1)
 OR EXISTS(SELECT 1 FROM pg_type element WHERE element.oid=t.typelem AND element.typtype='c')))))`},
		{"cross-schema routine dependencies", `
WITH local_objects AS (
 SELECT 'pg_proc'::regclass AS classid,p.oid FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1
 UNION ALL SELECT 'pg_attrdef'::regclass,d.oid FROM pg_attrdef d JOIN pg_class c ON c.oid=d.adrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1
 UNION ALL SELECT 'pg_constraint'::regclass,k.oid FROM pg_constraint k JOIN pg_namespace n ON n.oid=k.connamespace WHERE n.nspname=$1
 UNION ALL SELECT 'pg_trigger'::regclass,t.oid FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1
 UNION ALL SELECT 'pg_policy'::regclass,p.oid FROM pg_policy p JOIN pg_class c ON c.oid=p.polrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1
 UNION ALL SELECT 'pg_rewrite'::regclass,r.oid FROM pg_rewrite r JOIN pg_class c ON c.oid=r.ev_class JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1
 UNION ALL SELECT 'pg_class'::regclass,c.oid FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1)
SELECT EXISTS(SELECT 1 FROM local_objects o JOIN pg_depend d ON d.classid=o.classid AND d.objid=o.oid
JOIN pg_proc target ON target.oid=d.refobjid JOIN pg_namespace n ON n.oid=target.pronamespace
WHERE d.refclassid='pg_proc'::regclass AND n.nspname NOT IN ('pg_catalog',$1))`},
		{"non-view rules or foreign-schema/metadata view dependencies", `
SELECT EXISTS(SELECT 1 FROM pg_rewrite r JOIN pg_class c ON c.oid=r.ev_class JOIN pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname=$1 AND c.relname<>'schema_migrations' AND (r.rulename<>'_RETURN' OR EXISTS(
 SELECT 1 FROM pg_depend d JOIN pg_class target ON target.oid=d.refobjid JOIN pg_namespace tn ON tn.oid=target.relnamespace
 WHERE d.classid='pg_rewrite'::regclass AND d.objid=r.oid AND d.refclassid='pg_class'::regclass
 AND (tn.nspname NOT IN ('pg_catalog',$1) OR target.relname='schema_migrations'))))`},
		{"schema-local extension, collation, operator or text-search objects", `
SELECT EXISTS(SELECT 1 FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace WHERE n.nspname=$1)
OR EXISTS(SELECT 1 FROM pg_collation c JOIN pg_namespace n ON n.oid=c.collnamespace WHERE n.nspname=$1)
OR EXISTS(SELECT 1 FROM pg_operator o JOIN pg_namespace n ON n.oid=o.oprnamespace WHERE n.nspname=$1)
OR EXISTS(SELECT 1 FROM pg_opclass o JOIN pg_namespace n ON n.oid=o.opcnamespace WHERE n.nspname=$1)
OR EXISTS(SELECT 1 FROM pg_opfamily o JOIN pg_namespace n ON n.oid=o.opfnamespace WHERE n.nspname=$1)
OR EXISTS(SELECT 1 FROM pg_conversion o JOIN pg_namespace n ON n.oid=o.connamespace WHERE n.nspname=$1)
OR EXISTS(SELECT 1 FROM pg_ts_config o JOIN pg_namespace n ON n.oid=o.cfgnamespace WHERE n.nspname=$1)
OR EXISTS(SELECT 1 FROM pg_ts_dict o JOIN pg_namespace n ON n.oid=o.dictnamespace WHERE n.nspname=$1)
OR EXISTS(SELECT 1 FROM pg_ts_parser o JOIN pg_namespace n ON n.oid=o.prsnamespace WHERE n.nspname=$1)
OR EXISTS(SELECT 1 FROM pg_ts_template o JOIN pg_namespace n ON n.oid=o.tmplnamespace WHERE n.nspname=$1)`},
	}
	for _, check := range checks {
		var found bool
		if err := tx.QueryRow(ctx, check.query, schema).Scan(&found); err != nil {
			return safeDBError("inspect supported catalog features", err)
		}
		if found {
			return fmt.Errorf("catalog does not support %s; refusing an incomplete schema snapshot", check.feature)
		}
	}
	return nil
}
