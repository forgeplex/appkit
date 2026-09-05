package pgtx

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// tenantGUC 是租户身份在事务里的落点：NewTenant 的 Do 在开启事务后
// set_config 落值（事务级，结束自动还原），RLS 策略经 appkit_current_tenant()
// 读它。名字带点是 Postgres 自定义 GUC 的要求；固定一个名字让策略 DDL
// 与 Transactor 的约定只有一处。
const tenantGUC = "app.tenant_id"

// tenantScopeGUC 是「读全部租户」模式的落点（值固定为 all）：与租户 GUC
// 分开——读全部的开关绝不能从租户值那条通道（令牌 tid）伸进来，否则一枚
// tid 取特殊值的令牌就是全库可读。
const tenantScopeGUC = "app.tenant_scope"

// 策略名固定：VerifyTenantRLS 按名点检，TenantPolicySQL 重复应用时按名先删。
const (
	policyIsolation = "tenant_isolation"
	policyReadAll   = "tenant_isolation_read_all"
)

// TenantScopeSQL 生成租户上下文函数的 DDL，落在域的基础迁移里（库函数是
// DDL 唯一事实源）。两个函数：
//
//   - appkit_read_all_tenants()：事务是否处于读全部模式（tx.WithReadAllTenants
//     → GUC app.tenant_scope=all）；
//   - appkit_current_tenant()：当前租户。GUC 缺失或为空时响亮报错而不是返回
//     NULL——NULL 会让策略静默匹配零行，「查询成功但什么都看不见」比错误更
//     危险。事务外的查询没有事务级 GUC，同样在这里被拦下：租户域的业务查询
//     必须经 NewTenant 的 Do。唯一的例外是读全部模式：无租户的系统级批处理
//     跨租户读是合法的，此时返回 NULL——读靠 read_all 策略放行，写入的
//     WITH CHECK 比对 NULL 恒不成立，仍被拒。
//
// CREATE OR REPLACE：升级 appkit 后在新迁移里再调一次即可刷新。
func TenantScopeSQL(schema string) string {
	return tenantScopeSQL(quoteIdent(schema) + ".")
}

// TenantScopeSQLBare 是 TenantScopeSQL 的无前缀形态，给「分区 + 行级」
// 双层隔离的域用（NewRoutedTenant）：函数建在每个分区 schema 里，落位由
// 迁移期的 search_path 决定，与分区域域的基础迁移同一纪律。
func TenantScopeSQLBare() string {
	return tenantScopeSQL("")
}

func tenantScopeSQL(prefix string) string {
	var b strings.Builder
	b.WriteString("-- 租户上下文函数：RLS 策略据此取当前租户与读全部模式（appkit pgtx.TenantScopeSQL 生成）。\n")
	fmt.Fprintf(&b, "CREATE OR REPLACE FUNCTION %sappkit_read_all_tenants() RETURNS boolean\n", prefix)
	b.WriteString("LANGUAGE sql STABLE AS $$\n")
	b.WriteString("    SELECT coalesce(current_setting('" + tenantScopeGUC + "', true), '') = 'all'\n")
	b.WriteString("$$;\n")
	fmt.Fprintf(&b, "CREATE OR REPLACE FUNCTION %sappkit_current_tenant() RETURNS text\n", prefix)
	b.WriteString("LANGUAGE plpgsql STABLE AS $$\n")
	b.WriteString("DECLARE\n    v text := nullif(current_setting('" + tenantGUC + "', true), '');\n")
	b.WriteString("BEGIN\n")
	b.WriteString("    IF v IS NULL THEN\n")
	b.WriteString("        IF coalesce(current_setting('" + tenantScopeGUC + "', true), '') = 'all' THEN\n")
	b.WriteString("            RETURN NULL;\n")
	b.WriteString("        END IF;\n")
	b.WriteString("        RAISE EXCEPTION '" + tenantGUC + " 未设置——租户域的业务查询必须经 pgtx.NewTenant 的 Do，且 ctx 带租户身份（authn 从令牌 tid 焊入 callctx）'\n")
	b.WriteString("            USING ERRCODE = '42501';\n")
	b.WriteString("    END IF;\n")
	b.WriteString("    RETURN v;\n")
	b.WriteString("END;\n$$;\n")
	return b.String()
}

// TenantPolicySQL 生成一张租户业务表的行级安全策略 DDL，写在建表的同一
// 迁移文件里（与 COMMENT ON TABLE 同款纪律）。四件缺一不可：
//   - ENABLE：策略生效；
//   - FORCE：表主（通常就是应用角色）也被策略约束——没有它，RLS 对
//     owner 是装饰；
//   - tenant_isolation 策略：tenant_id 等于当前租户才可见、才可写
//     （WITH CHECK 拦住「把别家的 tenant_id 写进去」）；
//   - tenant_isolation_read_all 策略：只对 SELECT、只在读全部模式
//     （tx.WithReadAllTenants）放开全部行。同类策略取并集，所以写路径
//     不受它影响——写永远只能落当前租户。
//
// 输出可重复应用（策略先 DROP IF EXISTS 再建）：升级 appkit 后在新迁移
// 里对每张租户表再调一次即可。迁移文件内要跨租户回填数据时，把本函数
// 的输出放在回填语句之后（同一文件内 DDL 顺序自控）。
func TenantPolicySQL(schema, table string) string {
	return tenantPolicySQL(quoteIdent(schema)+".", quoteIdent(table))
}

// TenantPolicySQLBare 是 TenantPolicySQL 的无前缀形态（配 TenantScopeSQLBare
// 与 NewRoutedTenant）：表名与函数名都不带 schema，由迁移期的 search_path
// 落位到当前分区。
func TenantPolicySQLBare(table string) string {
	return tenantPolicySQL("", quoteIdent(table))
}

func tenantPolicySQL(prefix, table string) string {
	tbl := prefix + table
	var b strings.Builder
	b.WriteString("-- 行级安全：tenant_id 隔离（appkit pgtx.TenantPolicySQL 生成）。漏写 WHERE 也查不到别家的行。\n")
	b.WriteString("-- 可重复应用：升级 appkit 后在新迁移里再调一次即刷新策略。\n")
	fmt.Fprintf(&b, "ALTER TABLE %s ENABLE ROW LEVEL SECURITY;\n", tbl)
	fmt.Fprintf(&b, "ALTER TABLE %s FORCE ROW LEVEL SECURITY;\n", tbl)
	fmt.Fprintf(&b, "DROP POLICY IF EXISTS %s ON %s;\n", policyIsolation, tbl)
	fmt.Fprintf(&b, "CREATE POLICY %s ON %s\n", policyIsolation, tbl)
	fmt.Fprintf(&b, "    USING (tenant_id = %sappkit_current_tenant())\n", prefix)
	fmt.Fprintf(&b, "    WITH CHECK (tenant_id = %sappkit_current_tenant());\n", prefix)
	b.WriteString("-- 读全部模式（tx.WithReadAllTenants）只放开 SELECT；写入仍走上面的策略，只能落当前租户。\n")
	fmt.Fprintf(&b, "DROP POLICY IF EXISTS %s ON %s;\n", policyReadAll, tbl)
	fmt.Fprintf(&b, "CREATE POLICY %s ON %s FOR SELECT\n", policyReadAll, tbl)
	fmt.Fprintf(&b, "    USING (%sappkit_read_all_tenants());\n", prefix)
	return b.String()
}

// quoteIdent 校验并转义标识符：白名单先拦（防注入与怪名字），再用
// pgx 的 Identifier 转义成带引号的形式，双保险与 outbox 一致。
func quoteIdent(name string) string {
	if !schemaRe.MatchString(name) {
		panic(fmt.Sprintf("pgtx: 标识符 %q 不合法（须匹配 %s）——库函数只生成可预期形态的 DDL", name, schemaRe))
	}
	return "\"" + name + "\""
}

// VerifyTenantRLS 在 Setup 期校验租户域的隔离不是装饰。查两件事，一次
// 报全：
//
//   - schema 里凡带 tenant_id 列的表都开了 RLS（ENABLE + FORCE）且两条
//     策略齐全（tenant_isolation / tenant_isolation_read_all）——建表忘了
//     挂策略、策略被后来者删掉、或还是升级前的旧形态，启动即红并点名表；
//   - 连接角色不得是 superuser/BYPASSRLS——这两类角色绕过 RLS，隔离
//     对其静默失效（superuser 永远绕过，表主也要 FORCE 才被约束）。
//
// 基础设施表（outbox/idem/audit）没有 tenant_id 列，天然不在校验视野。
// 一个租户表都没有时是 no-op——刚生成还没写业务表的域不受角色约束打搅。
// 「分区 + 行级」的域对每个分区 schema 各调一次。
func VerifyTenantRLS(ctx context.Context, db DB, schema string) error {
	if !schemaRe.MatchString(schema) {
		return fmt.Errorf("pgtx: VerifyTenantRLS 的 schema 名 %q 不合法", schema)
	}
	const q = `
SELECT c.relname,
       c.relrowsecurity,
       c.relforcerowsecurity,
       EXISTS (SELECT 1 FROM pg_policies p
               WHERE p.schemaname = $1 AND p.tablename = c.relname AND p.policyname = $2),
       EXISTS (SELECT 1 FROM pg_policies p
               WHERE p.schemaname = $1 AND p.tablename = c.relname AND p.policyname = $3)
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'tenant_id'
                   AND a.attnum > 0 AND NOT a.attisdropped
WHERE n.nspname = $1 AND c.relkind = 'r'
ORDER BY c.relname`
	rows, err := db.Query(ctx, q, schema, policyIsolation, policyReadAll)
	if err != nil {
		return fmt.Errorf("pgtx: 查询租户表的 RLS 状态: %w", err)
	}
	defer rows.Close()

	var problems []string
	n := 0
	for rows.Next() {
		var name string
		var enabled, forced, hasIsolation, hasReadAll bool
		if err := rows.Scan(&name, &enabled, &forced, &hasIsolation, &hasReadAll); err != nil {
			return fmt.Errorf("pgtx: 读取租户表 RLS 状态: %w", err)
		}
		n++
		switch {
		case !enabled:
			problems = append(problems,
				fmt.Sprintf("%s：未 ENABLE ROW LEVEL SECURITY（建表迁移里用 pgtx.TenantPolicySQL 生成策略）", name))
		case !forced:
			problems = append(problems,
				fmt.Sprintf("%s：未 FORCE ROW LEVEL SECURITY——应用角色通常是表主，不 FORCE 则 RLS 对它是装饰", name))
		case !hasIsolation:
			problems = append(problems,
				fmt.Sprintf("%s：开了 RLS 但没有 %s 策略——用 pgtx.TenantPolicySQL 重建", name, policyIsolation))
		case !hasReadAll:
			problems = append(problems,
				fmt.Sprintf("%s：缺 %s 策略（升级前的旧形态）——新迁移里重跑 pgtx.TenantScopeSQL 与 pgtx.TenantPolicySQL 刷新",
					name, policyReadAll))
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("pgtx: 遍历租户表: %w", err)
	}
	if n == 0 {
		return nil
	}

	var bypass bool
	if err := db.QueryRow(ctx,
		"SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user").Scan(&bypass); err != nil {
		return fmt.Errorf("pgtx: 查连接角色的 RLS 豁免: %w", err)
	}
	if bypass {
		var role string
		_ = db.QueryRow(ctx, "SELECT current_user").Scan(&role)
		problems = append(problems, fmt.Sprintf(
			"连接角色 %s 是 superuser 或 BYPASSRLS——RLS 对它静默不生效，行级隔离只是装饰。"+
				"给应用配普通角色（CREATE ROLE <app> LOGIN PASSWORD '…'; GRANT USAGE ON SCHEMA %s TO <app>; "+
				"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %s TO <app>），"+
				"迁移仍可用高权限角色跑",
			role, schema, schema))
	}
	if len(problems) > 0 {
		return errors.New("pgtx: 租户域 " + schema + " 的行级隔离不完整：\n  " + strings.Join(problems, "\n  "))
	}
	return nil
}
