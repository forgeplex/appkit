package pgtx_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit/internal/dbtest"
	"github.com/forgeplex/appkit/pgtx"
)

// ---- 不需要 Postgres ----

func TestVerifyTenantIsolationRejectsBadIdentifiers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// 标识符在碰数据库之前就被拦下，所以 db 可以是 nil。
	if err := pgtx.VerifyTenantIsolation(ctx, nil, `files"; DROP SCHEMA x`); err == nil ||
		!strings.Contains(err.Error(), "schema 名") {
		t.Fatalf("坏 schema 名应报错，实际 %v", err)
	}
	if err := pgtx.VerifyTenantIsolation(ctx, nil, "files", pgtx.TenantExempt("rates", "Rates")); err == nil ||
		!strings.Contains(err.Error(), "TenantExempt") || !strings.Contains(err.Error(), "Rates") {
		t.Fatalf("坏豁免表名应点名报错，实际 %v", err)
	}
}

// ---- 需要 Postgres（TEST_DATABASE_URL；make check 下自动 skip）----

// releasedV09Migration 是 v0.9.0–v0.9.9 的 TenantScopeSQL + TenantPolicySQL
// 输出原文（函数裸调用、不包子查询）。存量域的迁移里落的就是这份文本，
// 升级 appkit 不会重写它们——严格校验必须一直认它。这里是字面量而不是
// 调库函数：库函数的输出会随版本演进，本用例锁的正是「已发布的旧文本」。
func releasedV09Migration(schema string) string {
	s := pgx.Identifier{schema}.Sanitize()
	return fmt.Sprintf(`CREATE TABLE %[1]s.documents (id text PRIMARY KEY, tenant_id text NOT NULL, body text);
CREATE OR REPLACE FUNCTION %[1]s.appkit_read_all_tenants() RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT coalesce(current_setting('app.tenant_scope', true), '') = 'all'
$$;
CREATE OR REPLACE FUNCTION %[1]s.appkit_current_tenant() RETURNS text
LANGUAGE plpgsql STABLE AS $$
DECLARE
    v text := nullif(current_setting('app.tenant_id', true), '');
BEGIN
    IF v IS NULL THEN
        IF coalesce(current_setting('app.tenant_scope', true), '') = 'all' THEN
            RETURN NULL;
        END IF;
        RAISE EXCEPTION 'app.tenant_id 未设置——租户域的业务查询必须经 pgtx.NewTenant 的 Do，且 ctx 带租户身份（authn 从令牌 tid 焊入 callctx）'
            USING ERRCODE = '42501';
    END IF;
    RETURN v;
END;
$$;
ALTER TABLE %[1]s."documents" ENABLE ROW LEVEL SECURITY;
ALTER TABLE %[1]s."documents" FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON %[1]s."documents";
CREATE POLICY tenant_isolation ON %[1]s."documents"
    USING (tenant_id = %[1]s.appkit_current_tenant())
    WITH CHECK (tenant_id = %[1]s.appkit_current_tenant());
DROP POLICY IF EXISTS tenant_isolation_read_all ON %[1]s."documents";
CREATE POLICY tenant_isolation_read_all ON %[1]s."documents" FOR SELECT
    USING (%[1]s.appkit_read_all_tenants());
`, s)
}

// ddl 把含 %[1]s（带引号的 schema 名）的 SQL 模板变成按 schema 渲染的函数。
func ddl(format string) func(schema string) string {
	return func(schema string) string { return fmt.Sprintf(format, pgx.Identifier{schema}.Sanitize()) }
}

// plainRole 建一个不绕过 RLS 的角色，返回带引号的角色名。测试连接是
// superuser，校验要切成普通角色才能走到「全部通过」这条路。
func plainRole(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	rid := pgx.Identifier{fmt.Sprintf("pgtx_iso_%d", time.Now().UnixNano())}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE ROLE "+rid+" NOLOGIN NOBYPASSRLS"); err != nil {
		t.Fatalf("建角色: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.WithoutCancel(ctx), "DROP ROLE IF EXISTS "+rid) })
	return rid
}

// verifyIsolationAs 在一条连接上切到 role（空串 = 保持测试连接的 superuser）、
// 可选地设置 search_path，然后跑严格校验。
func verifyIsolationAs(t *testing.T, pool *pgxpool.Pool, role, searchPath, schema string, opts ...pgtx.IsolationOption) error {
	t.Helper()
	ctx := context.Background()
	var verr error
	err := pool.AcquireFunc(ctx, func(c *pgxpool.Conn) error {
		defer func() { _, _ = c.Exec(ctx, "RESET ROLE; RESET search_path") }()
		if searchPath != "" {
			if _, err := c.Exec(ctx, "SET search_path TO "+searchPath); err != nil {
				return err
			}
		}
		if role != "" {
			if _, err := c.Exec(ctx, "SET ROLE "+role); err != nil {
				return err
			}
		}
		verr = pgtx.VerifyTenantIsolation(ctx, c, schema, opts...)
		return nil
	})
	if err != nil {
		t.Fatalf("准备校验连接: %v", err)
	}
	return verr
}

func TestVerifyTenantIsolation(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	role := plainRole(t, pool)

	// %[1]s = 带引号的 schema 名。
	for _, tc := range []struct {
		name string
		// base 缺省为 tenantMigration：documents 表 + 库函数当前输出的函数与策略。
		base dbtest.Migration
		// alter 在 base 之后执行，模拟「后来者」对隔离的改动。
		alter func(schema string) string
		opts  []pgtx.IsolationOption
		want  []string // 报错必须包含；为空表示应通过
		// legacyBlind 标记 VerifyTenantRLS 看不见的改写：结构齐全、名字都对，
		// 只是定义被改松了。两个函数在同一个用例上的差别，就是严格校验存在的理由。
		legacyBlind bool
	}{
		{name: "生成物原样"},
		{name: "v0.9.x 的旧写法也认", base: releasedV09Migration},
		{
			name: "varchar 的 tenant_id",
			alter: func(schema string) string {
				return ddl(`CREATE TABLE %[1]s.legacy_docs (id text PRIMARY KEY, tenant_id varchar(64) NOT NULL);`)(schema) +
					pgtx.TenantPolicySQL(schema, "legacy_docs")
			},
		},
		{
			name:  "额外的 RESTRICTIVE 策略只会收紧",
			alter: ddl(`CREATE POLICY not_archived ON %[1]s.documents AS RESTRICTIVE USING (body IS NOT NULL);`),
		},
		{
			// 下游判例：凭据表不参与读全部模式，读全部策略写成恒假。
			name:  "读全部策略收紧成恒假",
			alter: ddl(`ALTER POLICY tenant_isolation_read_all ON %[1]s.documents USING (false);`),
		},
		{
			name:  "函数体只差空白",
			alter: ddl(`CREATE OR REPLACE FUNCTION %[1]s.appkit_read_all_tenants() RETURNS boolean LANGUAGE sql STABLE AS $$ SELECT coalesce(current_setting('app.tenant_scope', true), '') = 'all' $$;`),
		},
		{
			name:        "额外的 PERMISSIVE 策略",
			legacyBlind: true,
			alter:       ddl(`CREATE POLICY support_readonly ON %[1]s.documents FOR SELECT USING (true);`),
			want:        []string{"documents：多出一条 PERMISSIVE 策略 support_readonly"},
		},
		{
			name:        "同名策略改成恒真",
			legacyBlind: true,
			alter:       ddl(`ALTER POLICY tenant_isolation ON %[1]s.documents USING (true) WITH CHECK (true);`),
			want:        []string{"策略 tenant_isolation 的定义不是", "USING 是 true", "WITH CHECK 是 true"},
		},
		{
			name:  "隔离策略多了一个 OR 分支",
			alter: ddl(`ALTER POLICY tenant_isolation ON %[1]s.documents USING (tenant_id = %[1]s.appkit_current_tenant() OR body IS NULL);`),
			want:  []string{"策略 tenant_isolation 的定义不是", "USING 是 "},
		},
		{
			name:  "隔离策略读 GUC 而不经上下文函数",
			alter: ddl(`ALTER POLICY tenant_isolation ON %[1]s.documents USING (tenant_id = current_setting('app.tenant_id', true));`),
			want:  []string{"策略 tenant_isolation 的定义不是", "USING 是 (tenant_id = current_setting("},
		},
		{
			name:  "隔离策略限定了角色",
			alter: ddl(`ALTER POLICY tenant_isolation ON %[1]s.documents TO CURRENT_USER;`),
			want:  []string{"策略 tenant_isolation 的定义不是", "限定了角色"},
		},
		{
			name: "隔离策略是 RESTRICTIVE",
			alter: ddl(`DROP POLICY tenant_isolation ON %[1]s.documents;
CREATE POLICY tenant_isolation ON %[1]s.documents AS RESTRICTIVE
    USING (tenant_id = %[1]s.appkit_current_tenant()) WITH CHECK (tenant_id = %[1]s.appkit_current_tenant());`),
			want: []string{"策略 tenant_isolation 的定义不是", "不是 PERMISSIVE"},
		},
		{
			name:        "读全部策略改成恒真",
			legacyBlind: true,
			alter:       ddl(`ALTER POLICY tenant_isolation_read_all ON %[1]s.documents USING (true);`),
			want:        []string{"策略 tenant_isolation_read_all 的定义不是", "USING 是 true"},
		},
		{
			name:  "读全部策略取反",
			alter: ddl(`ALTER POLICY tenant_isolation_read_all ON %[1]s.documents USING (NOT %[1]s.appkit_read_all_tenants());`),
			want:  []string{"策略 tenant_isolation_read_all 的定义不是", "USING 是 "},
		},
		{
			name: "读全部策略放开了写",
			alter: ddl(`DROP POLICY tenant_isolation_read_all ON %[1]s.documents;
CREATE POLICY tenant_isolation_read_all ON %[1]s.documents USING (%[1]s.appkit_read_all_tenants());`),
			want: []string{"策略 tenant_isolation_read_all 的定义不是", "不是 FOR SELECT"},
		},
		{
			name:        "上下文函数改成恒真",
			legacyBlind: true,
			alter:       ddl(`CREATE OR REPLACE FUNCTION %[1]s.appkit_read_all_tenants() RETURNS boolean LANGUAGE sql STABLE AS $$ SELECT true $$;`),
			want:        []string{"上下文函数 appkit_read_all_tenants() 的定义不是", "函数体被改写"},
		},
		{
			name:  "上下文函数缺 GUC 时不再报错",
			alter: ddl(`CREATE OR REPLACE FUNCTION %[1]s.appkit_current_tenant() RETURNS text LANGUAGE sql STABLE AS $$ SELECT nullif(current_setting('app.tenant_id', true), '') $$;`),
			want:  []string{"上下文函数 appkit_current_tenant() 的定义不是", "语言是 sql", "函数体被改写"},
		},
		{
			name:  "上下文函数带函数级 SET",
			alter: ddl(`ALTER FUNCTION %[1]s.appkit_read_all_tenants() SET app.tenant_scope = 'all';`),
			want:  []string{"上下文函数 appkit_read_all_tenants() 的定义不是", "带函数级 SET 子句"},
		},
		{
			name:  "上下文函数被删（策略随 CASCADE 一起没了）",
			alter: ddl(`DROP FUNCTION %[1]s.appkit_current_tenant() CASCADE;`),
			want:  []string{"documents：开了 RLS 但没有 tenant_isolation 策略", "上下文函数 appkit_current_tenant() 不存在"},
		},
		{
			name:  "摘掉 FORCE",
			alter: ddl(`ALTER TABLE %[1]s.documents NO FORCE ROW LEVEL SECURITY;`),
			want:  []string{"documents：未 FORCE ROW LEVEL SECURITY"},
		},
		{
			name:  "隔离策略被删",
			alter: ddl(`DROP POLICY tenant_isolation ON %[1]s.documents;`),
			want:  []string{"documents：开了 RLS 但没有 tenant_isolation 策略"},
		},
		{
			name:  "读全部策略被删",
			alter: ddl(`DROP POLICY tenant_isolation_read_all ON %[1]s.documents;`),
			want:  []string{"documents：缺 tenant_isolation_read_all 策略"},
		},
		{
			name:  "没有 tenant_id 的表必须声明",
			alter: ddl(`CREATE TABLE %[1]s.rates (id text PRIMARY KEY);`),
			want:  []string{"rates：没有 tenant_id 列，也不在豁免清单里"},
		},
		{
			name:  "显式豁免",
			alter: ddl(`CREATE TABLE %[1]s.rates (id text PRIMARY KEY);`),
			opts:  []pgtx.IsolationOption{pgtx.TenantExempt("rates")},
		},
		{
			name:  "基础设施表不必声明",
			alter: ddl(`CREATE TABLE %[1]s.outbox (id int); CREATE TABLE %[1]s.inbox (id int); CREATE TABLE %[1]s.idempotency_keys (id int); CREATE TABLE %[1]s.audit_log (id int); CREATE TABLE %[1]s.schema_migrations (id int);`),
		},
		{
			name: "豁免清单过期",
			opts: []pgtx.IsolationOption{pgtx.TenantExempt("gone")},
			want: []string{"豁免清单里的 gone 在 schema 里不存在"},
		},
		{
			name: "带 tenant_id 的表不能豁免",
			opts: []pgtx.IsolationOption{pgtx.TenantExempt("documents")},
			want: []string{"豁免清单里的 documents 带 tenant_id 列"},
		},
		{
			name: "分区表的父表与分区都要挂策略",
			alter: ddl(`CREATE TABLE %[1]s.events (id bigint, tenant_id text NOT NULL, at date NOT NULL) PARTITION BY RANGE (at);
CREATE TABLE %[1]s.events_2026 PARTITION OF %[1]s.events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');`),
			want: []string{"events：未 ENABLE ROW LEVEL SECURITY", "events_2026：未 ENABLE ROW LEVEL SECURITY"},
		},
		{
			name:  "带 tenant_id 的物化视图",
			alter: ddl(`CREATE MATERIALIZED VIEW %[1]s.doc_stats AS SELECT tenant_id, count(*) AS n FROM %[1]s.documents GROUP BY tenant_id;`),
			want:  []string{"doc_stats：物化视图不支持行级安全"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := tc.base
			if base == nil {
				base = tenantMigration
			}
			schema := dbtest.Schema(t, pool, "pgtx_iso", base)
			if tc.alter != nil {
				if _, err := pool.Exec(ctx, tc.alter(schema)); err != nil {
					t.Fatalf("应用改动: %v", err)
				}
			}
			err := verifyIsolationAs(t, pool, role, "", schema, tc.opts...)
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("应通过，实际 %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应拒绝并包含 %q，实际通过", tc.want)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("报错应包含 %q，实际：\n%v", w, err)
				}
			}
			if tc.legacyBlind {
				legacy := pool.AcquireFunc(ctx, func(c *pgxpool.Conn) error {
					defer func() { _, _ = c.Exec(ctx, "RESET ROLE") }()
					if _, err := c.Exec(ctx, "SET ROLE "+role); err != nil {
						return err
					}
					return pgtx.VerifyTenantRLS(ctx, c, schema)
				})
				if legacy != nil {
					t.Errorf("VerifyTenantRLS 的行为不应改变（它只按名点检，看不见这类改写），实际 %v", legacy)
				}
			}
		})
	}
}

// 函数的身份取自 pg_depend 而不是表达式文本：别的 schema 里一个同名函数
// 在 search_path 上可见时，pg_get_expr 省略 schema 限定，文本与正主一模一样。
func TestVerifyTenantIsolationFunctionIdentity(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	role := plainRole(t, pool)
	schema := dbtest.Schema(t, pool, "pgtx_iso_id", tenantMigration)
	evil := dbtest.Schema(t, pool, "pgtx_iso_evil", func(evil string) string {
		return fmt.Sprintf(`CREATE FUNCTION %[1]s.appkit_current_tenant() RETURNS text LANGUAGE sql STABLE AS $$ SELECT 't1'::text $$;`,
			pgx.Identifier{evil}.Sanitize())
	})
	sid, eid := pgx.Identifier{schema}.Sanitize(), pgx.Identifier{evil}.Sanitize()
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`ALTER POLICY tenant_isolation ON %[1]s.documents
    USING (tenant_id = %[2]s.appkit_current_tenant()) WITH CHECK (tenant_id = %[2]s.appkit_current_tenant());
GRANT USAGE ON SCHEMA %[2]s TO %[3]s;`, sid, eid, role)); err != nil {
		t.Fatalf("改写策略: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.WithoutCancel(ctx), "REVOKE ALL ON SCHEMA "+eid+" FROM "+role)
	})

	// 冒名函数所在的 schema 排在 search_path 最前：表达式文本不带限定。
	err := verifyIsolationAs(t, pool, role, eid, schema)
	if err == nil || !strings.Contains(err.Error(), "引用的函数不是本 schema 的 appkit_current_tenant()") {
		t.Fatalf("冒名函数应按 OID 被认出，实际 %v", err)
	}
	if strings.Contains(err.Error(), "USING 是") {
		t.Fatalf("本用例要证明的是「文本看不出差别、OID 看得出」，但文本已经不匹配了：\n%v", err)
	}
	// 不设 search_path 时文本带限定，同样被拒。
	if err := verifyIsolationAs(t, pool, role, "", schema); err == nil {
		t.Fatal("带限定的冒名函数也应被拒")
	}
}

func TestVerifyTenantIsolationRoleAndEmpty(t *testing.T) {
	pool := dbtest.Pool(t)
	// 没有任何表：放行，superuser 连接也不打搅（与 VerifyTenantRLS 一致）。
	empty := dbtest.Schema(t, pool, "pgtx_iso_empty", nil)
	if err := verifyIsolationAs(t, pool, "", "", empty); err != nil {
		t.Fatalf("空 schema 应放行: %v", err)
	}
	// 策略齐全但连接角色绕过 RLS：只报角色。
	full := dbtest.Schema(t, pool, "pgtx_iso_role", tenantMigration)
	err := verifyIsolationAs(t, pool, "", "", full)
	if err == nil || !strings.Contains(err.Error(), "BYPASSRLS") {
		t.Fatalf("superuser 应报绕过，实际 %v", err)
	}
	if strings.Contains(err.Error(), "documents") {
		t.Fatalf("策略齐全不应点名表，实际 %v", err)
	}
}

// 「分区 + 行级」形态：函数与策略都无前缀，落位在分区 schema 里。校验时
// 分区 schema 在不在 search_path 上，决定表达式文本带不带限定——两种都认。
func TestVerifyTenantIsolationBare(t *testing.T) {
	pool := dbtest.Pool(t)
	role := plainRole(t, pool)
	schema := bareTenantSchema(t, pool, "pgtx_iso_bare")
	if err := verifyIsolationAs(t, pool, role, "", schema); err != nil {
		t.Fatalf("无前缀形态应通过（文本带限定）: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		"GRANT USAGE ON SCHEMA "+pgx.Identifier{schema}.Sanitize()+" TO "+role); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "REVOKE ALL ON SCHEMA "+pgx.Identifier{schema}.Sanitize()+" FROM "+role)
	})
	if err := verifyIsolationAs(t, pool, role, pgx.Identifier{schema}.Sanitize(), schema); err != nil {
		t.Fatalf("无前缀形态应通过（文本不带限定）: %v", err)
	}
}
