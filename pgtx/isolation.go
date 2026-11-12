package pgtx

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// IsolationOption 配置 VerifyTenantIsolation。
type IsolationOption func(*isolationConfig)

type isolationConfig struct {
	exempt []string
}

// TenantExempt 显式声明 schema 里不按租户隔离的表——全部租户共用的字典、
// 费率、渠道配置这类平台级数据。
//
// 这是一个安全决定，所以要写出来：没有 tenant_id 列的表不受 RLS 约束，
// 任何查询都能读到全部行。VerifyTenantIsolation 把「没有 tenant_id 又不在
// 这份清单里」当成遗漏拒绝启动，于是「这张表故意不隔离」和「忘了加
// tenant_id」不再长得一样——与路由的 MountPublic 同一个道理，公开是显式
// 决定而不是剩余状态。
//
// appkit 自己的基础设施表（outbox、inbox、idempotency_keys、audit_log、
// schema_migrations）不必列——按表名加核心列的结构识别，同名但结构不同
// 的业务表不算，照常要列。带 tenant_id 列的表不能豁免，清单里出现不
// 存在的表名同样报错——清单必须与 schema 一致，否则它迟早变成没人敢删的
// 历史遗留。经父表策略间接隔离的子表（没有自己的 tenant_id 列）也列在
// 这里，隔离由它自己的策略负责，本校验不替它背书。
func TenantExempt(tables ...string) IsolationOption {
	return func(c *isolationConfig) { c.exempt = append(c.exempt, tables...) }
}

// infraTables 是 appkit 自己建的基础设施表及其核心列（列名 → format_type
// 的类型名）：relay、claim、迁移 runner 都不带租户身份，这些表没有
// tenant_id 列，天然不按租户隔离，不必逐域声明豁免。
//
// 只按表名认是不够的：没用审计的域自己建一张 audit_log、消息类的域建一张
// inbox，名字相同、装的却是租户数据。所以认的是「表名 + 核心列齐全且
// 类型相符」——核心列取自 v0.1.0 以来每个版本都有的那些，后来追加的列
// （outbox.claim_token、schema_migrations.checksum）不在其内，新旧版本
// 建出来的表都认；同名但结构不符的表不算基础设施表，照常要求显式豁免。
//
// 本包不能 import outbox / idem / audit / pgmigrate（它们依赖本包），所以
// 这里是它们 DDL 的一份摘要；真库用例把库函数的真实 DDL 建出来过一遍
// 校验，摘要与 DDL 脱节时用例变红。清单与 internal/schemadoc 的
// frameworkTables 是同一份。
var infraTables = map[string]map[string]string{
	"outbox": {
		"id": "uuid", "topic": "text", "payload": "jsonb", "meta": "jsonb",
		"attempts": "integer", "next_attempt_at": pgTimestamptz, "claimed_until": pgTimestamptz,
		"failed_at": pgTimestamptz, "last_error": "text", "created_at": pgTimestamptz,
		"published_at": pgTimestamptz,
	},
	"inbox": {
		"consumer": "text", "event_id": "uuid", "topic": "text", "processed_at": pgTimestamptz,
	},
	"idempotency_keys": {
		"key": "text", "payload_hash": "bytea", "owner_token": "uuid", "state": "text",
		"status": "integer", "headers": "jsonb", "body": "bytea",
		"created_at": pgTimestamptz, "completed_at": pgTimestamptz,
	},
	"audit_log": {
		"id": "bigint", "at": pgTimestamptz, "actor": "text", "action": "text",
		"entity": "text", "entity_id": "text", "before": "jsonb", "after": "jsonb", "meta": "jsonb",
	},
	"schema_migrations": {
		"version": "text", "applied_at": pgTimestamptz,
	},
}

// pgTimestamptz 是 format_type 对 timestamptz 的写法。
const pgTimestamptz = "timestamp with time zone"

// infraMismatch 报告与基础设施表同名的 rel 哪里不是它；结构相符时返回空串。
// 只在 rel.name 是基础设施表名时有意义。
func infraMismatch(rel isolationRelation) string {
	if rel.kind != relKindTable {
		return "不是普通表"
	}
	core := infraTables[rel.name]
	var missing, wrong []string
	for _, col := range slices.Sorted(maps.Keys(core)) {
		switch got, ok := rel.columns[col]; {
		case !ok:
			missing = append(missing, col)
		case got != core[col]:
			wrong = append(wrong, fmt.Sprintf("%s 是 %s（应为 %s）", col, got, core[col]))
		}
	}
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "缺列 "+strings.Join(missing, "、"))
	}
	if len(wrong) > 0 {
		parts = append(parts, "列类型不符："+strings.Join(wrong, "、"))
	}
	return strings.Join(parts, "；")
}

// VerifyTenantIsolation 在 Setup 期校验租户域的行级隔离既完整又没被改松，
// 是 VerifyTenantRLS 的严格形态。逐项校验，一次报全：
//
//   - 覆盖面：schema 里每张表（普通表、分区表、物化视图）要么带 tenant_id
//     列并挂全策略，要么是 appkit 的基础设施表（按表名加核心列结构识别，
//     同名的业务表不算），要么在 TenantExempt 里显式豁免。没有 tenant_id
//     又没声明的表拒绝启动——「忘了加列」不再是静默的不隔离；
//   - 策略定义：tenant_isolation / tenant_isolation_read_all 不只要存在，
//     还必须是 TenantPolicySQL 生成的形态（命令、PERMISSIVE、TO PUBLIC、
//     USING / WITH CHECK 表达式、引用的函数就是本 schema 的上下文函数）。
//     同名策略被改成恒真，点名拒绝；
//   - 没有额外的宽策略：PERMISSIVE 策略之间取并集，租户表上多挂一条就能
//     放行隔离策略拦下的行，所以除上面两条外不允许再有 PERMISSIVE 策略。
//     RESTRICTIVE 策略只会收紧，允许；
//   - 上下文函数：appkit_current_tenant() / appkit_read_all_tenants() 的
//     定义必须是 TenantScopeSQL 的输出——函数被换成恒真，策略文本再规范
//     也是装饰；
//   - 连接角色不得是 superuser/BYPASSRLS（与 VerifyTenantRLS 相同）。
//
// 策略的两种书写形态都接受：tenant_id = f()（v0.9.x 的生成物）与
// tenant_id = (SELECT f())（当前生成物）。存量域切到本函数不需要补迁移，
// 只需要把没有 tenant_id 的业务表在 TenantExempt 里列出来。
//
// 唯一认可的手写改动是把某张表的 tenant_isolation_read_all 写成
// USING (false)：这张表不参与读全部模式，平台运营的跨租户读也看不到它
// （凭据、密文）。这是收紧，不是放宽。
//
// 原生分区表的父表与每个分区各自需要策略：经父表访问只应用父表的策略，
// 直接访问分区只应用分区自己的策略。视图不在校验视野内。
//
// 「分区 + 行级」的域对每个分区 schema 各调一次。只读目录，不执行 DDL。
// 这是启动期守卫，不是 SQL 沙箱：迁移与应用代码仍是受信的，启动之后
// 再改策略要到下次启动才会被发现。
func VerifyTenantIsolation(ctx context.Context, db DB, schema string, opts ...IsolationOption) error {
	if !schemaRe.MatchString(schema) {
		return fmt.Errorf("pgtx: VerifyTenantIsolation 的 schema 名 %q 不合法", schema)
	}
	var cfg isolationConfig
	for _, o := range opts {
		o(&cfg)
	}
	exempt := make(map[string]bool, len(cfg.exempt))
	for _, name := range cfg.exempt {
		if !schemaRe.MatchString(name) {
			return fmt.Errorf("pgtx: TenantExempt 的表名 %q 不合法（须匹配 %s）", name, schemaRe)
		}
		exempt[name] = true
	}

	rels, err := loadIsolationRelations(ctx, db, schema)
	if err != nil {
		return err
	}
	policies, err := loadIsolationPolicies(ctx, db, schema)
	if err != nil {
		return err
	}
	fns, err := loadScopeFunctions(ctx, db, schema)
	if err != nil {
		return err
	}

	var problems []string
	tenantTables := 0
	seen := make(map[string]isolationRelation, len(rels))
	for _, rel := range rels {
		seen[rel.name] = rel
		switch {
		case rel.hasTenant && rel.kind == relKindMatview:
			problems = append(problems, fmt.Sprintf(
				"%s：物化视图不支持行级安全，带 tenant_id 的物化视图对任何查询都是全租户可见——改成普通表并挂策略，或移出本 schema",
				rel.name))
		case rel.hasTenant:
			tenantTables++
			problems = append(problems, tenantTableProblems(rel, policies[rel.name], fns)...)
		case exempt[rel.name]:
		case infraTables[rel.name] == nil:
			problems = append(problems, fmt.Sprintf(
				"%s：没有 tenant_id 列，也不在豁免清单里——全部租户共用的表在 pgtx.TenantExempt 里显式列出；属于租户的表补 tenant_id 列并用 pgtx.TenantPolicySQL 挂策略",
				rel.name))
		default:
			// 与基础设施表同名：结构相符才免声明。
			if mismatch := infraMismatch(rel); mismatch != "" {
				problems = append(problems, fmt.Sprintf(
					"%s：与 appkit 的基础设施表同名，但结构不是它（%s）——不能按基础设施表免声明。全部租户共用就在 pgtx.TenantExempt 里显式列出；属于租户的表补 tenant_id 列并用 pgtx.TenantPolicySQL 挂策略",
					rel.name, mismatch))
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(exempt)) {
		rel, ok := seen[name]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf(
				"豁免清单里的 %s 在 schema 里不存在——清单过期了，从 pgtx.TenantExempt 里删掉", name))
		case rel.hasTenant:
			problems = append(problems, fmt.Sprintf(
				"豁免清单里的 %s 带 tenant_id 列——带租户列的表必须挂策略，不能豁免", name))
		}
	}

	if tenantTables > 0 {
		problems = append(problems, fns.problems()...)
		problem, err := bypassRoleProblem(ctx, db, schema)
		if err != nil {
			return err
		}
		if problem != "" {
			problems = append(problems, problem)
		}
	}
	if len(problems) > 0 {
		return errors.New("pgtx: 租户域 " + schema + " 的行级隔离不完整：\n  " + strings.Join(problems, "\n  "))
	}
	return nil
}

// pg_class.relkind 与 pg_policy.polcmd 的取值。
const (
	relKindTable   = "r"
	relKindMatview = "m"
	polCmdAll      = "*"
	polCmdSelect   = "r"
)

type isolationRelation struct {
	name      string
	kind      string            // r 普通表 / p 分区表 / m 物化视图
	columns   map[string]string // 列名 → format_type 的类型名
	enabled   bool
	forced    bool
	hasTenant bool
}

type isolationPolicy struct {
	name       string
	cmd        string
	permissive bool
	public     bool    // TO PUBLIC：不限定角色
	using      string  // pg_get_expr 原文，没有则为空
	check      string  // 同上
	funcs      []int64 // 表达式引用的用户函数 OID，去重升序
}

func loadIsolationRelations(ctx context.Context, db DB, schema string) ([]isolationRelation, error) {
	const q = `
SELECT c.relname, c.relkind::text, c.relrowsecurity, c.relforcerowsecurity,
       ARRAY(SELECT a.attname::text FROM pg_attribute a
             WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
             ORDER BY a.attnum),
       ARRAY(SELECT format_type(a.atttypid, a.atttypmod) FROM pg_attribute a
             WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
             ORDER BY a.attnum)
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relkind IN ('r', 'p', 'm')
ORDER BY c.relname`
	rows, err := db.Query(ctx, q, schema)
	if err != nil {
		return nil, fmt.Errorf("pgtx: 查询 schema %s 的表: %w", schema, err)
	}
	defer rows.Close()
	var out []isolationRelation
	for rows.Next() {
		var r isolationRelation
		var names, types []string
		if err := rows.Scan(&r.name, &r.kind, &r.enabled, &r.forced, &names, &types); err != nil {
			return nil, fmt.Errorf("pgtx: 读取 schema %s 的表: %w", schema, err)
		}
		r.columns = make(map[string]string, len(names))
		for i, name := range names {
			r.columns[name] = types[i]
		}
		_, r.hasTenant = r.columns["tenant_id"]
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgtx: 遍历 schema %s 的表: %w", schema, err)
	}
	return out, nil
}

// loadIsolationPolicies 读 schema 里全部 RLS 策略，按表名分组。函数的身份
// 取自 pg_depend（建策略时记录的 OID），不取自表达式文本里的名字：
// pg_get_expr 在函数经 search_path 可见时省略 schema 限定，别的 schema 里
// 一个同名函数在文本上与正主无从分辨。
func loadIsolationPolicies(ctx context.Context, db DB, schema string) (map[string][]isolationPolicy, error) {
	const q = `
SELECT c.relname, p.polname, p.polcmd::text, p.polpermissive,
       p.polroles = ARRAY[0]::oid[],
       coalesce(pg_get_expr(p.polqual, p.polrelid), ''),
       coalesce(pg_get_expr(p.polwithcheck, p.polrelid), ''),
       ARRAY(SELECT DISTINCT d.refobjid::bigint FROM pg_depend d
             WHERE d.classid = 'pg_catalog.pg_policy'::regclass AND d.objid = p.oid
               AND d.refclassid = 'pg_catalog.pg_proc'::regclass
             ORDER BY 1)
FROM pg_policy p
JOIN pg_class c ON c.oid = p.polrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1
ORDER BY c.relname, p.polname`
	rows, err := db.Query(ctx, q, schema)
	if err != nil {
		return nil, fmt.Errorf("pgtx: 查询 schema %s 的 RLS 策略: %w", schema, err)
	}
	defer rows.Close()
	out := map[string][]isolationPolicy{}
	for rows.Next() {
		var table string
		var p isolationPolicy
		if err := rows.Scan(&table, &p.name, &p.cmd, &p.permissive, &p.public, &p.using, &p.check, &p.funcs); err != nil {
			return nil, fmt.Errorf("pgtx: 读取 schema %s 的 RLS 策略: %w", schema, err)
		}
		out[table] = append(out[table], p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgtx: 遍历 schema %s 的 RLS 策略: %w", schema, err)
	}
	return out, nil
}

// tenantTableProblems 校验一张带 tenant_id 的表：ENABLE + FORCE、两条策略
// 的定义、没有额外的 PERMISSIVE 策略。缺件的报错与 VerifyTenantRLS 相同。
func tenantTableProblems(rel isolationRelation, pols []isolationPolicy, fns scopeFunctions) []string {
	switch {
	case !rel.enabled:
		return []string{fmt.Sprintf("%s：未 ENABLE ROW LEVEL SECURITY（建表迁移里用 pgtx.TenantPolicySQL 生成策略）", rel.name)}
	case !rel.forced:
		return []string{fmt.Sprintf("%s：未 FORCE ROW LEVEL SECURITY——应用角色通常是表主，不 FORCE 则 RLS 对它是装饰", rel.name)}
	}
	var isolation, readAll *isolationPolicy
	var problems []string
	for i := range pols {
		p := &pols[i]
		switch {
		case p.name == policyIsolation:
			isolation = p
		case p.name == policyReadAll:
			readAll = p
		case p.permissive:
			problems = append(problems, fmt.Sprintf(
				"%s：多出一条 PERMISSIVE 策略 %s——PERMISSIVE 策略之间取并集，它放行的行不再受租户隔离约束；删掉它，需要额外收紧就建 RESTRICTIVE 策略",
				rel.name, p.name))
		}
	}
	if isolation == nil {
		problems = append(problems, fmt.Sprintf("%s：开了 RLS 但没有 %s 策略——用 pgtx.TenantPolicySQL 重建", rel.name, policyIsolation))
	} else {
		wrong := policyShapeProblems(isolation, polCmdAll, "FOR ALL")
		if !isolationExprRe.MatchString(isolation.using) {
			wrong = append(wrong, "USING 是 "+showExpr(isolation.using))
		}
		if !isolationExprRe.MatchString(isolation.check) {
			wrong = append(wrong, "WITH CHECK 是 "+showExpr(isolation.check))
		}
		if fn := fns.current; fn != nil && !slices.Equal(isolation.funcs, []int64{fn.oid}) {
			wrong = append(wrong, "引用的函数不是本 schema 的 "+fnCurrentTenant+"()")
		}
		problems = append(problems, policyDefinitionProblem(rel.name, policyIsolation, wrong)...)
	}
	if readAll == nil {
		problems = append(problems, fmt.Sprintf(
			"%s：缺 %s 策略（升级前的旧形态）——新迁移里重跑 pgtx.TenantScopeSQL 与 pgtx.TenantPolicySQL 刷新",
			rel.name, policyReadAll))
	} else {
		wrong := policyShapeProblems(readAll, polCmdSelect, "FOR SELECT")
		if readAll.check != "" {
			wrong = append(wrong, "多了 WITH CHECK "+showExpr(readAll.check))
		}
		// 恒假的读全部策略是合法的收紧：这张表不参与读全部模式（凭据、密文
		// 这类连平台运营也不该跨租户读的表）。PERMISSIVE 策略取并集，恒假
		// 等于不额外放行任何行。
		if off := readAll.using == "false" && len(readAll.funcs) == 0; !off {
			if !readAllExprRe.MatchString(readAll.using) {
				wrong = append(wrong, "USING 是 "+showExpr(readAll.using))
			}
			if fn := fns.readAll; fn != nil && !slices.Equal(readAll.funcs, []int64{fn.oid}) {
				wrong = append(wrong, "引用的函数不是本 schema 的 "+fnReadAllTenants+"()")
			}
		}
		problems = append(problems, policyDefinitionProblem(rel.name, policyReadAll, wrong)...)
	}
	return problems
}

func policyShapeProblems(p *isolationPolicy, cmd, cmdLabel string) []string {
	var wrong []string
	if p.cmd != cmd {
		wrong = append(wrong, "不是 "+cmdLabel)
	}
	if !p.permissive {
		wrong = append(wrong, "不是 PERMISSIVE")
	}
	if !p.public {
		wrong = append(wrong, "限定了角色（应为 TO PUBLIC）")
	}
	return wrong
}

func policyDefinitionProblem(table, policy string, wrong []string) []string {
	if len(wrong) == 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"%s：策略 %s 的定义不是 pgtx.TenantPolicySQL 的输出（%s）——新迁移里重跑 pgtx.TenantPolicySQL 重建",
		table, policy, strings.Join(wrong, "；"))}
}

// 策略表达式的规范形态，按 pg_get_expr 的输出匹配。它随 search_path（函数
// 带不带 schema 限定）、列类型（varchar 列多一层 ::text）与书写形态（裸调用 /
// 标量子查询）变化，所以匹配的是形态而不是整串原文；空白只在记号之间放宽，
// 绝不整体抹掉——抹掉空白后 NOT f() 会变成 NOTf()，与带 schema 限定的调用
// 无从分辨。这里的函数名只定「形态」，函数的身份由 pg_depend 的 OID 另行校验。
const exprQualifier = `(?:(?:"[^"]+"|[A-Za-z_][A-Za-z0-9_$]*)\.)?`

var (
	isolationExprRe = regexp.MustCompile(
		`^[(\s]*(?:tenant_id|\(tenant_id\)::text)\s*=\s*(?:` +
			exprCall(fnCurrentTenant) + `|\(\s*` + exprSubquery(fnCurrentTenant) + `\s*\))[)\s]*$`)
	readAllExprRe = regexp.MustCompile(
		`^[(\s]*(?:` + exprCall(fnReadAllTenants) + `|` + exprSubquery(fnReadAllTenants) + `)[)\s]*$`)
)

// exprCall 匹配裸调用 f()，schema 限定可有可无。
func exprCall(fn string) string { return exprQualifier + fn + `\(\)` }

// exprSubquery 匹配标量子查询体 SELECT f() AS f（不含外层括号）。
func exprSubquery(fn string) string {
	return `SELECT\s+` + exprCall(fn) + `(?:\s+AS\s+` + fn + `)?`
}

// showExpr 把表达式放进报错：空的写明「空」，过长的截断（报错要能一屏读完）。
func showExpr(expr string) string {
	const limit = 120
	if expr == "" {
		return "空"
	}
	if r := []rune(expr); len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return expr
}

// scopeFunction 是一个上下文函数在目录里的定义。
type scopeFunction struct {
	oid       int64
	lang      string
	volatile  string
	secdef    bool
	retset    bool
	rettype   string
	hasConfig bool // 函数级 SET 子句（proconfig）
	src       string
}

// scopeFunctions 是 schema 里的两个上下文函数；nil 表示不存在。
type scopeFunctions struct {
	current *scopeFunction
	readAll *scopeFunction
}

func loadScopeFunctions(ctx context.Context, db DB, schema string) (scopeFunctions, error) {
	const q = `
SELECT p.proname::text, p.oid::bigint, l.lanname::text, p.provolatile::text, p.prosecdef, p.proretset,
       p.prorettype::regtype::text, p.proconfig IS NOT NULL, p.prosrc
FROM pg_proc p
JOIN pg_namespace n ON n.oid = p.pronamespace
JOIN pg_language l ON l.oid = p.prolang
WHERE n.nspname = $1 AND p.proname::text = ANY($2::text[]) AND p.pronargs = 0 AND p.prokind = 'f'`
	rows, err := db.Query(ctx, q, schema, []string{fnCurrentTenant, fnReadAllTenants})
	if err != nil {
		return scopeFunctions{}, fmt.Errorf("pgtx: 查询 schema %s 的租户上下文函数: %w", schema, err)
	}
	defer rows.Close()
	var out scopeFunctions
	for rows.Next() {
		var name string
		var f scopeFunction
		if err := rows.Scan(&name, &f.oid, &f.lang, &f.volatile, &f.secdef, &f.retset, &f.rettype, &f.hasConfig, &f.src); err != nil {
			return scopeFunctions{}, fmt.Errorf("pgtx: 读取 schema %s 的租户上下文函数: %w", schema, err)
		}
		switch name {
		case fnCurrentTenant:
			out.current = &f
		case fnReadAllTenants:
			out.readAll = &f
		}
	}
	if err := rows.Err(); err != nil {
		return scopeFunctions{}, fmt.Errorf("pgtx: 遍历 schema %s 的租户上下文函数: %w", schema, err)
	}
	return out, nil
}

func (f scopeFunctions) problems() []string {
	var problems []string
	problems = append(problems, scopeFunctionProblems(fnCurrentTenant, f.current, "plpgsql", "text", acceptedCurrentTenantBodies)...)
	problems = append(problems, scopeFunctionProblems(fnReadAllTenants, f.readAll, "sql", "boolean", acceptedReadAllTenantsBodies)...)
	return problems
}

func scopeFunctionProblems(name string, f *scopeFunction, lang, rettype string, bodies []string) []string {
	if f == nil {
		return []string{fmt.Sprintf("上下文函数 %s() 不存在——新迁移里重跑 pgtx.TenantScopeSQL", name)}
	}
	var wrong []string
	if f.lang != lang {
		wrong = append(wrong, "语言是 "+f.lang)
	}
	if f.rettype != rettype || f.retset {
		wrong = append(wrong, "返回类型不是 "+rettype)
	}
	if f.volatile != "s" {
		wrong = append(wrong, "不是 STABLE")
	}
	if f.secdef {
		wrong = append(wrong, "是 SECURITY DEFINER")
	}
	if f.hasConfig {
		wrong = append(wrong, "带函数级 SET 子句")
	}
	if !slices.ContainsFunc(bodies, func(body string) bool { return sameBody(body, f.src) }) {
		wrong = append(wrong, "函数体被改写")
	}
	if len(wrong) == 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"上下文函数 %s() 的定义不是 pgtx.TenantScopeSQL 的输出（%s）——新迁移里重跑 pgtx.TenantScopeSQL",
		name, strings.Join(wrong, "；"))}
}

// sameBody 比较两段函数体，忽略空白的多少（迁移文件过一遍格式化不该让
// 校验变红），不忽略任何别的东西。
func sameBody(a, b string) bool {
	return strings.Join(strings.Fields(a), " ") == strings.Join(strings.Fields(b), " ")
}

// bypassRoleProblem 报告连接角色是否绕过 RLS（superuser 或 BYPASSRLS）；
// 不绕过时返回空串。
func bypassRoleProblem(ctx context.Context, db DB, schema string) (string, error) {
	var bypass bool
	if err := db.QueryRow(ctx,
		"SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user").Scan(&bypass); err != nil {
		return "", fmt.Errorf("pgtx: 查连接角色的 RLS 豁免: %w", err)
	}
	if !bypass {
		return "", nil
	}
	var role string
	_ = db.QueryRow(ctx, "SELECT current_user").Scan(&role)
	return fmt.Sprintf(
		"连接角色 %s 是 superuser 或 BYPASSRLS——RLS 对它静默不生效，行级隔离只是装饰。"+
			"给应用配普通角色（CREATE ROLE <app> LOGIN PASSWORD '…'; GRANT USAGE ON SCHEMA %s TO <app>; "+
			"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %s TO <app>），"+
			"迁移仍可用高权限角色跑",
		role, schema, schema), nil
}
