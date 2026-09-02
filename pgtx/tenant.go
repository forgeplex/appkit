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

// TenantScopeSQL 生成租户上下文函数的 DDL，落在域的基础迁移里（库函数是
// DDL 唯一事实源）。策略（TenantPolicySQL）经它取当前租户；GUC 缺失或
// 为空时它响亮报错而不是返回 NULL——NULL 会让策略静默匹配零行，「查询
// 成功但什么都看不见」比错误更危险。事务外的查询没有事务级 GUC，同样
// 在这里被拦下：租户域的业务查询必须经 NewTenant 的 Do。
func TenantScopeSQL(schema string) string {
	var b strings.Builder
	b.WriteString("-- 租户上下文函数：RLS 策略据此取当前租户（appkit pgtx.TenantScopeSQL 生成）。\n")
	fmt.Fprintf(&b, "CREATE OR REPLACE FUNCTION %s.appkit_current_tenant() RETURNS text\n", quoteIdent(schema))
	b.WriteString("LANGUAGE plpgsql STABLE AS $$\n")
	b.WriteString("DECLARE\n    v text := nullif(current_setting('" + tenantGUC + "', true), '');\n")
	b.WriteString("BEGIN\n")
	b.WriteString("    IF v IS NULL THEN\n")
	b.WriteString("        RAISE EXCEPTION '" + tenantGUC + " 未设置——租户域的业务查询必须经 pgtx.NewTenant 的 Do，且 ctx 带租户身份（authn 从令牌 tid 焊入 callctx）'\n")
	b.WriteString("            USING ERRCODE = '42501';\n")
	b.WriteString("    END IF;\n")
	b.WriteString("    RETURN v;\n")
	b.WriteString("END;\n$$;\n")
	return b.String()
}

// TenantPolicySQL 生成一张租户业务表的行级安全策略 DDL，写在建表的同一
// 迁移文件里（与 COMMENT ON TABLE 同款纪律）。三件缺一不可：
//   - ENABLE：策略生效；
//   - FORCE：表主（通常就是应用角色）也被策略约束——没有它，RLS 对
//     owner 是装饰；
//   - POLICY：tenant_id 等于当前租户才可见、才可写（WITH CHECK 拦住
//     「把别家的 tenant_id 写进去」）。
//
// 迁移文件内要跨租户回填数据时，把本函数的输出放在回填语句之后（同一
// 文件内 DDL 顺序自控）。
func TenantPolicySQL(schema, table string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "-- 行级安全：tenant_id 隔离（appkit pgtx.TenantPolicySQL 生成）。漏写 WHERE 也查不到别家的行。\n")
	fmt.Fprintf(&b, "ALTER TABLE %s.%s ENABLE ROW LEVEL SECURITY;\n", quoteIdent(schema), quoteIdent(table))
	fmt.Fprintf(&b, "ALTER TABLE %s.%s FORCE ROW LEVEL SECURITY;\n", quoteIdent(schema), quoteIdent(table))
	fmt.Fprintf(&b, "CREATE POLICY tenant_isolation ON %s.%s\n", quoteIdent(schema), quoteIdent(table))
	fmt.Fprintf(&b, "    USING (tenant_id = %s.appkit_current_tenant())\n", quoteIdent(schema))
	fmt.Fprintf(&b, "    WITH CHECK (tenant_id = %s.appkit_current_tenant());\n", quoteIdent(schema))
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
//   - schema 里凡带 tenant_id 列的表都开了 RLS（ENABLE + FORCE）且有
//     策略——建表忘了挂策略、或策略被后来者删掉，启动即红并点名表；
//   - 连接角色不得是 superuser/BYPASSRLS——这两类角色绕过 RLS，隔离
//     对其静默失效（superuser 永远绕过，表主也要 FORCE 才被约束）。
//
// 基础设施表（outbox/idem/audit）没有 tenant_id 列，天然不在校验视野。
// 一个租户表都没有时是 no-op——刚生成还没写业务表的域不受角色约束打搅。
func VerifyTenantRLS(ctx context.Context, db DB, schema string) error {
	if !schemaRe.MatchString(schema) {
		return fmt.Errorf("pgtx: VerifyTenantRLS 的 schema 名 %q 不合法", schema)
	}
	const q = `
SELECT c.relname,
       c.relrowsecurity,
       c.relforcerowsecurity,
       EXISTS (SELECT 1 FROM pg_policies p
               WHERE p.schemaname = $1 AND p.tablename = c.relname)
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'tenant_id'
                   AND a.attnum > 0 AND NOT a.attisdropped
WHERE n.nspname = $1 AND c.relkind = 'r'
ORDER BY c.relname`
	rows, err := db.Query(ctx, q, schema)
	if err != nil {
		return fmt.Errorf("pgtx: 查询租户表的 RLS 状态: %w", err)
	}
	defer rows.Close()

	var problems []string
	n := 0
	for rows.Next() {
		var name string
		var enabled, forced, hasPolicy bool
		if err := rows.Scan(&name, &enabled, &forced, &hasPolicy); err != nil {
			return fmt.Errorf("pgtx: 读取租户表 RLS 状态: %w", err)
		}
		n++
		switch {
		case !enabled:
			problems = append(problems,
				fmt.Sprintf("%s：未 ENABLE ROW LEVEL SECURITY（建表迁移里用 pgtx.TenantPolicySQL 生成三件套）", name))
		case !forced:
			problems = append(problems,
				fmt.Sprintf("%s：未 FORCE ROW LEVEL SECURITY——应用角色通常是表主，不 FORCE 则 RLS 对它是装饰", name))
		case !hasPolicy:
			problems = append(problems,
				fmt.Sprintf("%s：开了 RLS 但没有策略——用 pgtx.TenantPolicySQL 重建", name))
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
