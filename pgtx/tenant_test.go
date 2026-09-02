package pgtx_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit/callctx"
	"github.com/forgeplex/appkit/internal/dbtest"
	"github.com/forgeplex/appkit/pgtx"
)

// ---- 不需要 Postgres ----

func TestTenantScopeSQLGolden(t *testing.T) {
	t.Parallel()
	got := pgtx.TenantScopeSQL("files")
	for _, want := range []string{
		`CREATE OR REPLACE FUNCTION "files".appkit_current_tenant() RETURNS text`,
		"LANGUAGE plpgsql STABLE AS $$",
		"current_setting('app.tenant_id', true)",
		"app.tenant_id 未设置",
		"USING ERRCODE = '42501'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("TenantScopeSQL 缺少 %q：\n%s", want, got)
		}
	}
}

func TestTenantPolicySQLGolden(t *testing.T) {
	t.Parallel()
	got := pgtx.TenantPolicySQL("files", "documents")
	for _, want := range []string{
		`ALTER TABLE "files"."documents" ENABLE ROW LEVEL SECURITY;`,
		`ALTER TABLE "files"."documents" FORCE ROW LEVEL SECURITY;`,
		`CREATE POLICY tenant_isolation ON "files"."documents"`,
		`USING (tenant_id = "files".appkit_current_tenant())`,
		`WITH CHECK (tenant_id = "files".appkit_current_tenant())`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("TenantPolicySQL 缺少 %q：\n%s", want, got)
		}
	}
}

func TestTenantDLIBadIdentifiers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		call  func()
		badIn string
	}{
		{"schema 大写", func() { pgtx.TenantScopeSQL("Files") }, "Files"},
		{"schema 带引号注入", func() { pgtx.TenantScopeSQL(`f"; DROP SCHEMA x`) }, "DROP SCHEMA"},
		{"table 带分号注入", func() { pgtx.TenantPolicySQL("files", `t; DROP TABLE x`) }, "DROP TABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("坏标识符应 panic")
				}
				if !strings.Contains(fmt.Sprint(r), tc.badIn) {
					t.Fatalf("报错应指出坏标识符，实际 %v", r)
				}
			}()
			tc.call()
		})
	}
}

// ---- 需要 Postgres（TEST_DATABASE_URL；make check 下自动 skip）----

// tenantMigration 建一张带 tenant_id 的业务表并挂全 RLS 三件套——
// 库函数生成的 DDL 在测试里真跑一遍，golden 只锁文本、这里锁行为。
func tenantMigration(schema string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s.documents (id text PRIMARY KEY, tenant_id text NOT NULL, body text);\n",
		pgx.Identifier{schema}.Sanitize())
	b.WriteString(pgtx.TenantScopeSQL(schema))
	b.WriteString(pgtx.TenantPolicySQL(schema, "documents"))
	return b.String()
}

func withTenant(ctx context.Context, tenant string) context.Context {
	return callctx.With(ctx, callctx.Meta{TenantID: tenant})
}

func TestNewTenantSetsGUC(t *testing.T) {
	pool := dbtest.Pool(t)
	schema := dbtest.Schema(t, pool, "pgtx_guc", tenantMigration)
	txr := pgtx.NewTenant(pool)
	_ = schema

	// 有租户：事务内读到事务级 GUC（callctx → set_config 的整条线）。
	err := txr.Do(withTenant(context.Background(), "t1"), func(ctx context.Context) error {
		var v string
		return pgtx.From(ctx, pool).QueryRow(ctx,
			"SELECT current_setting('app.tenant_id')").Scan(&v)
	})
	if err != nil {
		t.Fatalf("有租户的 Do: %v", err)
	}

	// 无租户：不设 GUC（缺 GUC 的报错来自策略函数，不在这里拦）。
	err = txr.Do(context.Background(), func(ctx context.Context) error {
		var v *string // NULL = GUC 未设
		return pgtx.From(ctx, pool).QueryRow(ctx,
			"SELECT nullif(current_setting('app.tenant_id', true), '')").Scan(&v)
	})
	if err != nil {
		t.Fatalf("无租户的 Do: %v", err)
	}
}

func TestTenantRLSEnforcement(t *testing.T) {
	pool := dbtest.Pool(t)
	schema := dbtest.Schema(t, pool, "pgtx_rls", tenantMigration)
	ctx := context.Background()

	// 非绕过角色：RLS 对它真实生效。测试连接是 superuser、永远绕过 RLS，
	// 所以用 SET LOCAL ROLE 切进去验——这正是 VerifyTenantRLS 必须查角色的原因。
	role := fmt.Sprintf("pgtx_rls_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE ROLE %s NOLOGIN NOBYPASSRLS", pgx.Identifier{role}.Sanitize())); err != nil {
		t.Fatalf("建角色: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.WithoutCancel(ctx), fmt.Sprintf(
			"REVOKE ALL ON SCHEMA %s FROM %s; DROP ROLE IF EXISTS %s",
			pgx.Identifier{schema}.Sanitize(), pgx.Identifier{role}.Sanitize(), pgx.Identifier{role}.Sanitize()))
	})
	for _, stmt := range []string{
		fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s", pgx.Identifier{schema}.Sanitize(), pgx.Identifier{role}.Sanitize()),
		fmt.Sprintf("GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %s TO %s",
			pgx.Identifier{schema}.Sanitize(), pgx.Identifier{role}.Sanitize()),
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("授权: %v", err)
		}
	}
	// 预置别家（t2）的数据：superuser 绕过 RLS，直接插。
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO %s.documents VALUES ('d2', 't2', '别家的行')",
		pgx.Identifier{schema}.Sanitize())); err != nil {
		t.Fatalf("预置数据: %v", err)
	}

	txr := pgtx.NewTenant(pool)
	setRole := fmt.Sprintf("SET LOCAL ROLE %s", pgx.Identifier{role}.Sanitize())
	q := func(sql string) string {
		return fmt.Sprintf(sql, pgx.Identifier{schema}.Sanitize())
	}

	// 持本租户身份：写自己的行成功；不带 WHERE 的 SELECT 也只见本租户——
	// 漏写 WHERE 不再是泄漏，而是查不到别家。
	err := txr.Do(withTenant(ctx, "t1"), func(ctx context.Context) error {
		db := pgtx.From(ctx, pool)
		if _, err := db.Exec(ctx, setRole); err != nil {
			return fmt.Errorf("切角色: %w", err)
		}
		if _, err := db.Exec(ctx, q(`INSERT INTO %s.documents VALUES ('d1', 't1', '自己的行')`)); err != nil {
			return fmt.Errorf("插本租户: %w", err)
		}
		var n int
		if err := db.QueryRow(ctx, q("SELECT count(*) FROM %s.documents")).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("不带 WHERE 也只见本租户：count=%d want 1（别家的行漏过来了）", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("本租户路径: %v", err)
	}

	// 冒名写别家：WITH CHECK 当场拒绝。失败语句会把事务打脏（aborted），
	// 把错误交回 Do 走回滚，外层断言错误身份。
	err = txr.Do(withTenant(ctx, "t1"), func(ctx context.Context) error {
		db := pgtx.From(ctx, pool)
		if _, err := db.Exec(ctx, setRole); err != nil {
			return err
		}
		_, err := db.Exec(ctx, q(`INSERT INTO %s.documents VALUES ('d3', 't2', '冒名别家')`))
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("冒名写别家应被 WITH CHECK 拒，实际 %v", err)
	}

	// 缺租户 GUC（普通事务、非 NewTenant）：策略函数响亮报错而非静默空结果。
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("开事务: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, setRole); err != nil {
		t.Fatalf("切角色: %v", err)
	}
	var body string
	err = tx.QueryRow(ctx, q("SELECT body FROM %s.documents LIMIT 1")).Scan(&body)
	if err == nil || !strings.Contains(err.Error(), "app.tenant_id 未设置") {
		t.Fatalf("缺 GUC 应响亮报错，实际 %v", err)
	}
}

func TestVerifyTenantRLS(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	full := dbtest.Schema(t, pool, "pgtx_vfull", tenantMigration)
	// 只有表、没挂策略的 schema。
	bare := dbtest.Schema(t, pool, "pgtx_vbare", func(schema string) string {
		return fmt.Sprintf("CREATE TABLE %s.documents (id text PRIMARY KEY, tenant_id text NOT NULL)",
			pgx.Identifier{schema}.Sanitize())
	})
	empty := dbtest.Schema(t, pool, "pgtx_vempty", nil)

	// 无租户表：no-op（superuser 连接也不打搅）。
	if err := pgtx.VerifyTenantRLS(ctx, pool, empty); err != nil {
		t.Fatalf("空 schema 应放行: %v", err)
	}
	// 缺策略：点名表；连接角色是 superuser，一并报全。
	err := pgtx.VerifyTenantRLS(ctx, pool, bare)
	if err == nil || !strings.Contains(err.Error(), "documents") {
		t.Fatalf("缺策略应点名表，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "BYPASSRLS") {
		t.Fatalf("superuser 豁免应一并报出，实际 %v", err)
	}
	// 策略齐全但连接角色绕过：只报角色，不报结构问题。
	err = pgtx.VerifyTenantRLS(ctx, pool, full)
	if err == nil || !strings.Contains(err.Error(), "BYPASSRLS") {
		t.Fatalf("superuser 应报绕过，实际 %v", err)
	}
	if strings.Contains(err.Error(), "未 ENABLE") {
		t.Fatalf("策略齐全不应报结构问题，实际 %v", err)
	}
	// 换成普通角色：齐备即绿（Verify 走 pgxpool.Conn 以保持会话角色）。
	role := fmt.Sprintf("pgtx_v_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, fmt.Sprintf("CREATE ROLE %s NOLOGIN NOBYPASSRLS",
		pgx.Identifier{role}.Sanitize())); err != nil {
		t.Fatalf("建角色: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.WithoutCancel(ctx),
			fmt.Sprintf("DROP ROLE IF EXISTS %s", pgx.Identifier{role}.Sanitize()))
	})
	err = pool.AcquireFunc(ctx, func(c *pgxpool.Conn) error {
		defer func() { _, _ = c.Exec(ctx, "RESET ROLE") }()
		if _, err := c.Exec(ctx, fmt.Sprintf("SET ROLE %s", pgx.Identifier{role}.Sanitize())); err != nil {
			return err
		}
		return pgtx.VerifyTenantRLS(ctx, c, full)
	})
	if err != nil {
		t.Fatalf("普通角色 + 齐备策略应放行: %v", err)
	}
}
