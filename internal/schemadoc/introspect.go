package schemadoc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/pgmigrate"
)

// Introspect 把 db/migrations 应用到一次性临时库，再从 pg_catalog 读回结构。
//
// 为什么建临时库而不是直接读 o.DSN 指的库：产出必须是 db/migrations 的纯函数。
// 直接读现有开发库的话，谁手工 ALTER 过一次，生成物就跟着歪，而 CI 上是干净库，
// 于是本地绿、CI 红，且没人看得出为什么。用一个新库名建库也不可能毁掉已有数据。
//
// 为什么不用 pg_dump：它要求客户端版本 ≥ 服务端（CI runner 上的 client 可能低于
// postgres:18），要额外装二进制，而关系图需要的外键结构无论如何都得查 catalog。
// CLI 本来就已经链接了 pgx，这条路零新依赖。
func Introspect(ctx context.Context, o Options) (Schema, error) {
	if o.DSN == "" {
		return Schema{}, fmt.Errorf("需要一个 Postgres 连接串：设 TEST_DATABASE_URL 或传 -dsn" +
			"（迁移会应用到由它派生的一次性临时库，不会动这个库本身）")
	}
	if o.Schema == "" {
		return Schema{}, fmt.Errorf("缺少 schema 名（.appkit.yml 的 domain）")
	}
	migDir := filepath.Join(o.Dir, filepath.FromSlash(migrationsDir))
	if _, err := os.Stat(migDir); err != nil {
		return Schema{}, fmt.Errorf("找不到迁移目录 %s: %w", migrationsDir, err)
	}

	cfg, err := pgxpool.ParseConfig(o.DSN)
	if err != nil {
		return Schema{}, fmt.Errorf("解析连接串: %w", err)
	}
	admin, err := pgx.ConnectConfig(ctx, cfg.ConnConfig.Copy())
	if err != nil {
		return Schema{}, fmt.Errorf("连接数据库: %w", err)
	}
	defer func() { _ = admin.Close(context.WithoutCancel(ctx)) }()

	tmp, err := tempDBName()
	if err != nil {
		return Schema{}, err
	}
	quoted := pgx.Identifier{tmp}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		return Schema{}, fmt.Errorf("创建临时库 %s: %w", tmp, err)
	}
	// 剥离取消信号：临时库必须尽力删掉，否则 Ctrl-C 会在服务器上留下垃圾库。
	defer func() {
		_, _ = admin.Exec(context.WithoutCancel(ctx), "DROP DATABASE IF EXISTS "+quoted+" WITH (FORCE)")
	}()

	tmpCfg := cfg.Copy()
	tmpCfg.ConnConfig.Database = tmp
	pool, err := pgxpool.NewWithConfig(ctx, tmpCfg)
	if err != nil {
		return Schema{}, fmt.Errorf("连接临时库: %w", err)
	}
	defer pool.Close()

	// 复用生产的迁移 runner：产出因此是服务启动时同一条代码路径派生的，
	// 而不是另写一遍 SQL 解析——那种副本迟早会和真实行为分叉。
	set := appkit.MigrationSet{Schema: o.Schema, FS: os.DirFS(migDir), Module: o.Schema}
	if err := pgmigrate.Runner(pool)(ctx, []appkit.MigrationSet{set}); err != nil {
		return Schema{}, fmt.Errorf("应用 %s: %w", migrationsDir, err)
	}
	return readCatalog(ctx, pool, o.Schema)
}

func tempDBName() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成临时库名: %w", err)
	}
	return "appkit_schema_" + hex.EncodeToString(b[:]), nil
}

// relation 是 readCatalog 内部用的中间态：oid 与 attnum→列名 只在读取期需要，
// 不进 Schema——Schema 是给渲染看的模型，塞进 oid 只会让纯函数测试难写。
type relation struct {
	oid   uint32
	table Table
	attrs map[int16]string
}

func readCatalog(ctx context.Context, pool *pgxpool.Pool, schema string) (Schema, error) {
	rels, err := readRelations(ctx, pool, schema)
	if err != nil {
		return Schema{}, err
	}
	// 策略挂到所属表上：未 ENABLE 但挂了策略的表也标出来（RLS 零值 +
	// 策略），渲染层会点出这个「装饰态」而不是装作没有。
	pols, err := readPolicies(ctx, pool, schema)
	if err != nil {
		return Schema{}, err
	}
	for i := range rels {
		if p := pols[rels[i].table.Name]; len(p) > 0 {
			if rels[i].table.RLS == nil {
				rels[i].table.RLS = &RLS{}
			}
			rels[i].table.RLS.Policies = p
		}
	}
	attrs := map[uint32]map[int16]string{}
	names := map[uint32]string{}
	for i := range rels {
		if err := readColumns(ctx, pool, &rels[i]); err != nil {
			return Schema{}, err
		}
		attrs[rels[i].oid] = rels[i].attrs
		names[rels[i].oid] = rels[i].table.Name
	}
	for i := range rels {
		if err := readConstraints(ctx, pool, &rels[i], attrs, names); err != nil {
			return Schema{}, err
		}
		if err := readIndexes(ctx, pool, &rels[i]); err != nil {
			return Schema{}, err
		}
		if err := readTriggers(ctx, pool, &rels[i]); err != nil {
			return Schema{}, err
		}
	}

	s := Schema{Name: schema, Tables: make([]Table, 0, len(rels))}
	for _, r := range rels {
		s.Tables = append(s.Tables, r.table)
	}
	sort.Slice(s.Tables, func(i, j int) bool { return s.Tables[i].Name < s.Tables[j].Name })

	if err := rejectUnsupportedTypes(ctx, pool, schema); err != nil {
		return Schema{}, err
	}
	if err := rejectLooseSequences(ctx, pool, schema); err != nil {
		return Schema{}, err
	}
	if s.Enums, err = readEnums(ctx, pool, schema); err != nil {
		return Schema{}, err
	}
	if s.Functions, err = readFunctions(ctx, pool, schema); err != nil {
		return Schema{}, err
	}
	return s, nil
}

// unsupported 报告一个「渲染不了」的特性。
//
// 这是本包的硬规矩：渲染不了就报错，绝不输出看起来像 DDL、实际漏掉了约束的东西。
// 会有人读它并当真——一份残缺但可信的 schema 视图，比没有视图危险得多。
func unsupported(table, feature string) error {
	return fmt.Errorf("%s：appkit schema 尚不支持渲染「%s」。"+
		"生成一份漏掉它的 DDL 会误导读者，因此这里直接失败——"+
		"请到 appkit 提 issue 补上渲染支持", table, feature)
}

func readRelations(ctx context.Context, pool *pgxpool.Pool, schema string) ([]relation, error) {
	const q = `
SELECT c.oid, c.relname, c.relkind::text,
       COALESCE(obj_description(c.oid, 'pg_class'), ''),
       c.relispartition, c.relrowsecurity, c.relforcerowsecurity, c.relhassubclass,
       CASE WHEN c.relkind IN ('v','m') THEN pg_get_viewdef(c.oid, true) ELSE '' END
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relkind NOT IN ('i','I','S','t')
ORDER BY c.relname`
	rows, err := pool.Query(ctx, q, schema)
	if err != nil {
		return nil, fmt.Errorf("查询 schema %q 的表: %w", schema, err)
	}
	defer rows.Close()

	var out []relation
	for rows.Next() {
		var (
			r                      relation
			kind, viewDef          string
			partition, rls, forced bool
			hasSubclasses          bool
		)
		if err := rows.Scan(&r.oid, &r.table.Name, &kind, &r.table.Comment,
			&partition, &rls, &forced, &hasSubclasses, &viewDef); err != nil {
			return nil, fmt.Errorf("读取表清单: %w", err)
		}
		switch kind {
		case kindRelTable:
			r.table.Kind = KindTable
		case kindRelView:
			r.table.Kind, r.table.ViewDef = KindView, viewDef
		case kindRelMat:
			r.table.Kind, r.table.ViewDef = KindMatView, viewDef
		case "p":
			return nil, unsupported(r.table.Name, "分区表")
		case "f":
			return nil, unsupported(r.table.Name, "外部表")
		case "c":
			return nil, unsupported(r.table.Name, "独立复合类型")
		default:
			return nil, unsupported(r.table.Name, "relkind "+kind)
		}
		switch {
		case partition:
			return nil, unsupported(r.table.Name, "分区子表")
		case hasSubclasses:
			return nil, unsupported(r.table.Name, "表继承")
		}
		// RLS 是租户域的常态（pgtx.TenantPolicySQL），如实读取并渲染；
		// 未 ENABLE 但挂了策略的「装饰态」也照实读——渲染层会点出来。
		if rls {
			r.table.RLS = &RLS{Enabled: true, Force: forced}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// readPolicies 读 schema 的全部行级安全策略，按表名分组。
func readPolicies(ctx context.Context, pool *pgxpool.Pool, schema string) (map[string][]RLSPolicy, error) {
	rows, err := pool.Query(ctx, `
SELECT tablename, policyname, cmd,
       COALESCE(roles::text, ''), COALESCE(qual, ''), COALESCE(with_check, '')
FROM pg_policies
WHERE schemaname = $1
ORDER BY tablename, policyname`, schema)
	if err != nil {
		return nil, fmt.Errorf("查询 schema %q 的 RLS 策略: %w", schema, err)
	}
	defer rows.Close()

	out := map[string][]RLSPolicy{}
	for rows.Next() {
		var table string
		var p RLSPolicy
		if err := rows.Scan(&table, &p.Name, &p.Cmd, &p.Roles, &p.Using, &p.WithCheck); err != nil {
			return nil, fmt.Errorf("读取 RLS 策略: %w", err)
		}
		out[table] = append(out[table], p)
	}
	return out, rows.Err()
}

func readColumns(ctx context.Context, pool *pgxpool.Pool, r *relation) error {
	const q = `
SELECT a.attnum, a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull,
       COALESCE(pg_get_expr(d.adbin, d.adrelid), ''),
       a.attidentity::text, a.attgenerated::text,
       COALESCE(col_description(a.attrelid, a.attnum), '')
FROM pg_attribute a
LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`
	rows, err := pool.Query(ctx, q, r.oid)
	if err != nil {
		return fmt.Errorf("查询 %s 的列: %w", r.table.Name, err)
	}
	defer rows.Close()

	r.attrs = map[int16]string{}
	for rows.Next() {
		var (
			num                 int16
			c                   Column
			identity, generated string
		)
		if err := rows.Scan(&num, &c.Name, &c.Type, &c.NotNull, &c.Default,
			&identity, &generated, &c.Comment); err != nil {
			return fmt.Errorf("读取 %s 的列: %w", r.table.Name, err)
		}
		if generated != "" {
			return unsupported(r.table.Name+"."+c.Name, "生成列")
		}
		switch identity {
		case "a":
			c.Identity = "GENERATED ALWAYS AS IDENTITY"
		case "d":
			c.Identity = "GENERATED BY DEFAULT AS IDENTITY"
		}
		r.attrs[num] = c.Name
		r.table.Columns = append(r.table.Columns, c)
	}
	return rows.Err()
}

func readConstraints(ctx context.Context, pool *pgxpool.Pool, r *relation,
	attrs map[uint32]map[int16]string, names map[uint32]string) error {
	const q = `
SELECT con.conname, con.contype::text, pg_get_constraintdef(con.oid),
       con.confrelid, COALESCE(con.conkey, '{}'), COALESCE(con.confkey, '{}')
FROM pg_constraint con
WHERE con.conrelid = $1
ORDER BY con.conname`
	rows, err := pool.Query(ctx, q, r.oid)
	if err != nil {
		return fmt.Errorf("查询 %s 的约束: %w", r.table.Name, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			c               Constraint
			typ             string
			refOID          uint32
			conkey, confkey []int16
		)
		if err := rows.Scan(&c.Name, &typ, &c.Def, &refOID, &conkey, &confkey); err != nil {
			return fmt.Errorf("读取 %s 的约束: %w", r.table.Name, err)
		}
		switch ConstraintType(typ) {
		case ConstraintPrimaryKey, ConstraintUnique, ConstraintForeignKey, ConstraintCheck:
			c.Type = ConstraintType(typ)
		case "x":
			return unsupported(r.table.Name, "排他约束（EXCLUDE）")
		default:
			continue // 't'：外键内部触发器约束等，不属于 schema 表面
		}
		if c.Columns, err = resolve(conkey, r.attrs); err != nil {
			return fmt.Errorf("%s 的约束 %s: %w", r.table.Name, c.Name, err)
		}
		if c.Type == ConstraintForeignKey {
			ref, ok := names[refOID]
			if !ok {
				return fmt.Errorf("%s 的外键 %s 指向 schema %q 之外的表——"+
					"跨 schema 外键会让域之间偷偷耦合，appkit check 本就禁止", r.table.Name, c.Name, r.table.Name)
			}
			c.RefTable = ref
			if c.RefColumns, err = resolve(confkey, attrs[refOID]); err != nil {
				return fmt.Errorf("%s 的外键 %s: %w", r.table.Name, c.Name, err)
			}
		}
		r.table.Constraints = append(r.table.Constraints, c)
	}
	return rows.Err()
}

// resolve 把 attnum 数组翻成列名。
func resolve(nums []int16, attrs map[int16]string) ([]string, error) {
	out := make([]string, 0, len(nums))
	for _, n := range nums {
		name, ok := attrs[n]
		if !ok {
			return nil, fmt.Errorf("列号 %d 找不到对应列", n)
		}
		out = append(out, name)
	}
	return out, nil
}

func readIndexes(ctx context.Context, pool *pgxpool.Pool, r *relation) error {
	// 排除约束自带的索引：它们已经在 CONSTRAINT 子句里出现过一次。
	const q = `
SELECT c.relname, pg_get_indexdef(i.indexrelid)
FROM pg_index i
JOIN pg_class c ON c.oid = i.indexrelid
WHERE i.indrelid = $1
  AND NOT EXISTS (SELECT 1 FROM pg_constraint con WHERE con.conindid = i.indexrelid)
ORDER BY c.relname`
	rows, err := pool.Query(ctx, q, r.oid)
	if err != nil {
		return fmt.Errorf("查询 %s 的索引: %w", r.table.Name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var i Index
		if err := rows.Scan(&i.Name, &i.Def); err != nil {
			return fmt.Errorf("读取 %s 的索引: %w", r.table.Name, err)
		}
		r.table.Indexes = append(r.table.Indexes, i)
	}
	return rows.Err()
}

func readTriggers(ctx context.Context, pool *pgxpool.Pool, r *relation) error {
	const q = `
SELECT tgname, pg_get_triggerdef(oid)
FROM pg_trigger WHERE tgrelid = $1 AND NOT tgisinternal ORDER BY tgname`
	rows, err := pool.Query(ctx, q, r.oid)
	if err != nil {
		return fmt.Errorf("查询 %s 的触发器: %w", r.table.Name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var t Trigger
		if err := rows.Scan(&t.Name, &t.Def); err != nil {
			return fmt.Errorf("读取 %s 的触发器: %w", r.table.Name, err)
		}
		r.table.Triggers = append(r.table.Triggers, t)
	}
	return rows.Err()
}

func readEnums(ctx context.Context, pool *pgxpool.Pool, schema string) ([]Enum, error) {
	const q = `
SELECT t.typname, e.enumlabel
FROM pg_type t
JOIN pg_namespace n ON n.oid = t.typnamespace
JOIN pg_enum e ON e.enumtypid = t.oid
WHERE n.nspname = $1
ORDER BY t.typname, e.enumsortorder`
	rows, err := pool.Query(ctx, q, schema)
	if err != nil {
		return nil, fmt.Errorf("查询枚举类型: %w", err)
	}
	defer rows.Close()

	var out []Enum
	for rows.Next() {
		var name, label string
		if err := rows.Scan(&name, &label); err != nil {
			return nil, fmt.Errorf("读取枚举类型: %w", err)
		}
		if len(out) == 0 || out[len(out)-1].Name != name {
			out = append(out, Enum{Name: name})
		}
		out[len(out)-1].Values = append(out[len(out)-1].Values, label)
	}
	return out, rows.Err()
}

func readFunctions(ctx context.Context, pool *pgxpool.Pool, schema string) ([]Function, error) {
	const q = `
SELECT p.proname,
       pg_get_function_identity_arguments(p.oid),
       pg_get_function_result(p.oid)
FROM pg_proc p
JOIN pg_namespace n ON n.oid = p.pronamespace
WHERE n.nspname = $1
ORDER BY p.proname, pg_get_function_identity_arguments(p.oid)`
	rows, err := pool.Query(ctx, q, schema)
	if err != nil {
		return nil, fmt.Errorf("查询函数: %w", err)
	}
	defer rows.Close()

	var out []Function
	for rows.Next() {
		var f Function
		if err := rows.Scan(&f.Name, &f.Signature, &f.Returns); err != nil {
			return nil, fmt.Errorf("读取函数: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// rejectUnsupportedTypes 拦住 domain / range / multirange：它们参与列的类型定义，
// 漏掉就等于漏掉了一条约束。
func rejectUnsupportedTypes(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	const q = `
SELECT t.typname, t.typtype::text
FROM pg_type t
JOIN pg_namespace n ON n.oid = t.typnamespace
WHERE n.nspname = $1 AND t.typtype IN ('d','r','m')
ORDER BY t.typname`
	name, typ, found, err := firstPair(ctx, pool, q, schema)
	if err != nil {
		return fmt.Errorf("查询自定义类型: %w", err)
	}
	if !found {
		return nil
	}
	return unsupported(schema+"."+name, map[string]string{
		"d": "domain 类型", "r": "range 类型", "m": "multirange 类型",
	}[typ])
}

// rejectLooseSequences 拦住不属于任何列的独立序列。属于 serial/identity 列的
// 序列不必单独渲染——列上的 nextval 默认值已经把它说清楚了。
func rejectLooseSequences(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	const q = `
SELECT c.relname, ''
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relkind = 'S'
  AND NOT EXISTS (
    SELECT 1 FROM pg_depend d
    WHERE d.objid = c.oid AND d.classid = 'pg_class'::regclass AND d.deptype IN ('a','i'))
ORDER BY c.relname`
	name, _, found, err := firstPair(ctx, pool, q, schema)
	if err != nil {
		return fmt.Errorf("查询序列: %w", err)
	}
	if !found {
		return nil
	}
	return unsupported(schema+"."+name, "独立序列（不属于任何列）")
}

// firstPair 取两列查询的第一行，用于「有就报错」的守卫查询。
func firstPair(ctx context.Context, pool *pgxpool.Pool, q, arg string) (a, b string, found bool, err error) {
	rows, err := pool.Query(ctx, q, arg)
	if err != nil {
		return "", "", false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return "", "", false, rows.Err()
	}
	if err := rows.Scan(&a, &b); err != nil {
		return "", "", false, err
	}
	return a, b, true, nil
}
