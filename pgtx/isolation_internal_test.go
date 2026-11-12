package pgtx

import (
	"strings"
	"testing"
)

// 样本取自 PostgreSQL 17 的 pg_get_expr 真实输出（qualified / 经 search_path
// 可见而省略限定 / 保留字 schema 带引号 / varchar 列多一层 ::text）。
// 形态匹配只管「是不是 tenant_id = f()」，函数的身份由 OID 校验负责。
func TestIsolationExprShape(t *testing.T) {
	t.Parallel()
	for _, expr := range []string{
		`(tenant_id = dp.appkit_current_tenant())`,
		`(tenant_id = appkit_current_tenant())`,
		`(tenant_id = "order".appkit_current_tenant())`,
		`((tenant_id)::text = dp.appkit_current_tenant())`,
		`(tenant_id = ( SELECT dp.appkit_current_tenant() AS appkit_current_tenant))`,
		`(tenant_id = ( SELECT appkit_current_tenant() AS appkit_current_tenant))`,
		`((tenant_id)::text = ( SELECT "order".appkit_current_tenant() AS appkit_current_tenant))`,
		`tenant_id = (SELECT dp.appkit_current_tenant())`, // 不带别名、不带外层括号
	} {
		if !isolationExprRe.MatchString(expr) {
			t.Errorf("应接受生成物形态：%s", expr)
		}
	}
	for _, expr := range []string{
		``,
		`true`,
		`(tenant_id = current_setting('app.tenant_id'::text, true))`,
		`((tenant_id = dp.appkit_current_tenant()) OR true)`,
		`((tenant_id = dp.appkit_current_tenant()) OR (body IS NULL))`,
		`(tenant_id <> dp.appkit_current_tenant())`,
		`(NOT (tenant_id = dp.appkit_current_tenant()))`,
		`(owner_id = dp.appkit_current_tenant())`,
		`(tenant_id = dp.appkit_current_tenant('x'::text))`,
		`(tenant_id = dp.other_fn())`,
		`(tenant_id = COALESCE(dp.appkit_current_tenant(), tenant_id))`,
		`(tenant_id = ( SELECT documents.tenant_id))`,
		`(tenant_id = ( SELECT dp.appkit_current_tenant() AS appkit_current_tenant UNION ALL SELECT 'x'::text))`,
		`(tenant_id = a.b.appkit_current_tenant())`,
		`(tenant_id = ( SELECTdp.appkit_current_tenant()))`,
	} {
		if isolationExprRe.MatchString(expr) {
			t.Errorf("不应接受：%s", expr)
		}
	}
}

func TestReadAllExprShape(t *testing.T) {
	t.Parallel()
	for _, expr := range []string{
		`dp.appkit_read_all_tenants()`,
		`appkit_read_all_tenants()`,
		`"order".appkit_read_all_tenants()`,
		`( SELECT dp.appkit_read_all_tenants() AS appkit_read_all_tenants)`,
		`(SELECT appkit_read_all_tenants())`,
	} {
		if !readAllExprRe.MatchString(expr) {
			t.Errorf("应接受生成物形态：%s", expr)
		}
	}
	for _, expr := range []string{
		``,
		`true`,
		// 抹掉空白再匹配会把这条认成「带 schema 限定的调用」：NOT dp.f() → NOTdp.f()。
		// 它的语义是「不在读全部模式时放开 SELECT」，恰好是最坏的那种改写。
		`(NOT dp.appkit_read_all_tenants())`,
		`(NOT appkit_read_all_tenants())`,
		`(dp.appkit_read_all_tenants() OR true)`,
		`(dp.appkit_read_all_tenants() IS NOT TRUE)`,
		`dp.appkit_current_tenant()`,
		`(tenant_id = dp.appkit_current_tenant())`,
		`( SELECT true AS appkit_read_all_tenants)`,
	} {
		if readAllExprRe.MatchString(expr) {
			t.Errorf("不应接受：%s", expr)
		}
	}
}

// 生成器写出去的函数体必须在校验认可的列表里；只差空白算同一份，
// 多一个字符都不算。
func TestScopeFunctionBodiesAccepted(t *testing.T) {
	t.Parallel()
	generated := tenantScopeSQL("")
	for name, bodies := range map[string][]string{
		fnCurrentTenant:  acceptedCurrentTenantBodies,
		fnReadAllTenants: acceptedReadAllTenantsBodies,
	} {
		for _, body := range bodies {
			if strings.TrimSpace(body) == "" {
				t.Errorf("%s 的认可列表里有空函数体", name)
			}
		}
		latest := bodies[len(bodies)-1]
		if !strings.Contains(generated, "AS $$"+latest+"$$;") {
			t.Errorf("TenantScopeSQL 写出的 %s 函数体不是认可列表的最后一项——改函数体要往列表里追加", name)
		}
	}
	if !sameBody(currentTenantBody, strings.ReplaceAll(currentTenantBody, "\n", "\n\t  ")) {
		t.Error("只差缩进与换行应视为同一份函数体")
	}
	if sameBody(readAllTenantsBody, strings.Replace(readAllTenantsBody, "= 'all'", "<> 'all'", 1)) {
		t.Error("改了运算符不应视为同一份函数体")
	}
}

func TestShowExpr(t *testing.T) {
	t.Parallel()
	if got := showExpr(""); got != "空" {
		t.Errorf("空表达式应写明，实际 %q", got)
	}
	long := strings.Repeat("租", 200)
	if got := showExpr(long); len([]rune(got)) != 121 || !strings.HasSuffix(got, "…") {
		t.Errorf("过长的表达式应按字符截断并加省略号，实际 %d 个字符", len([]rune(got)))
	}
}
