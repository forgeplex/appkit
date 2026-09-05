package pgtx_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit/callctx"
	"github.com/forgeplex/appkit/internal/dbtest"
	"github.com/forgeplex/appkit/pgtx"
	"github.com/forgeplex/appkit/tx"
)

// ---- 不需要 Postgres ----

func TestTenantScopeSQLGolden(t *testing.T) {
	t.Parallel()
	got := pgtx.TenantScopeSQL("files")
	for _, want := range []string{
		`CREATE OR REPLACE FUNCTION "files".appkit_read_all_tenants() RETURNS boolean`,
		"current_setting('app.tenant_scope', true)",
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
	// 无前缀形态：函数名不带 schema，落位交给迁移期的 search_path。
	bare := pgtx.TenantScopeSQLBare()
	for _, want := range []string{
		"CREATE OR REPLACE FUNCTION appkit_read_all_tenants() RETURNS boolean",
		"CREATE OR REPLACE FUNCTION appkit_current_tenant() RETURNS text",
	} {
		if !strings.Contains(bare, want) {
			t.Errorf("TenantScopeSQLBare 缺少 %q：\n%s", want, bare)
		}
	}
	if strings.Contains(bare, `"`) {
		t.Errorf("TenantScopeSQLBare 不应出现任何限定名：\n%s", bare)
	}
}

func TestTenantPolicySQLGolden(t *testing.T) {
	t.Parallel()
	got := pgtx.TenantPolicySQL("files", "documents")
	for _, want := range []string{
		`ALTER TABLE "files"."documents" ENABLE ROW LEVEL SECURITY;`,
		`ALTER TABLE "files"."documents" FORCE ROW LEVEL SECURITY;`,
		`DROP POLICY IF EXISTS tenant_isolation ON "files"."documents";`,
		`CREATE POLICY tenant_isolation ON "files"."documents"`,
		`USING (tenant_id = "files".appkit_current_tenant())`,
		`WITH CHECK (tenant_id = "files".appkit_current_tenant())`,
		`DROP POLICY IF EXISTS tenant_isolation_read_all ON "files"."documents";`,
		`CREATE POLICY tenant_isolation_read_all ON "files"."documents" FOR SELECT`,
		`USING ("files".appkit_read_all_tenants())`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("TenantPolicySQL 缺少 %q：\n%s", want, got)
		}
	}
	bare := pgtx.TenantPolicySQLBare("documents")
	for _, want := range []string{
		`ALTER TABLE "documents" FORCE ROW LEVEL SECURITY;`,
		`CREATE POLICY tenant_isolation ON "documents"`,
		`USING (tenant_id = appkit_current_tenant())`,
		`CREATE POLICY tenant_isolation_read_all ON "documents" FOR SELECT`,
		`USING (appkit_read_all_tenants())`,
	} {
		if !strings.Contains(bare, want) {
			t.Errorf("TenantPolicySQLBare 缺少 %q：\n%s", want, bare)
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
		{"bare table 带分号注入", func() { pgtx.TenantPolicySQLBare(`t; DROP TABLE x`) }, "DROP TABLE"},
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
	setRole := nonBypassRole(t, pool, schema)
	// 预置别家（t2）的数据：superuser 绕过 RLS，直接插。
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO %s.documents VALUES ('d2', 't2', '别家的行')",
		pgx.Identifier{schema}.Sanitize())); err != nil {
		t.Fatalf("预置数据: %v", err)
	}

	txr := pgtx.NewTenant(pool)
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
	// 升级前的旧形态：只有 tenant_isolation、没有读全部策略。
	old := dbtest.Schema(t, pool, "pgtx_vold", func(schema string) string {
		s := pgx.Identifier{schema}.Sanitize()
		return fmt.Sprintf(`CREATE TABLE %s.documents (id text PRIMARY KEY, tenant_id text NOT NULL);
ALTER TABLE %s.documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE %s.documents FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON %s.documents USING (tenant_id = current_setting('app.tenant_id', true));`, s, s, s, s)
	})

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
	// 旧形态：点名缺的那条策略并指出修法。
	err = pgtx.VerifyTenantRLS(ctx, pool, old)
	if err == nil || !strings.Contains(err.Error(), "tenant_isolation_read_all") {
		t.Fatalf("旧形态应点名缺读全部策略，实际 %v", err)
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

// nonBypassRole 建一个非绕过角色并授到 schemas 上，返回切进去用的
// SET LOCAL ROLE 语句。测试连接是 superuser、永远绕过 RLS，要验隔离
// 必须切成普通角色。
func nonBypassRole(t *testing.T, pool *pgxpool.Pool, schemas ...string) string {
	t.Helper()
	ctx := context.Background()
	role := fmt.Sprintf("pgtx_role_%d_%d", time.Now().UnixNano(), len(schemas))
	rid := pgx.Identifier{role}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE ROLE "+rid+" NOLOGIN NOBYPASSRLS"); err != nil {
		t.Fatalf("建角色: %v", err)
	}
	t.Cleanup(func() {
		for _, s := range schemas {
			_, _ = pool.Exec(context.WithoutCancel(ctx),
				"REVOKE ALL ON ALL TABLES IN SCHEMA "+pgx.Identifier{s}.Sanitize()+" FROM "+rid+
					"; REVOKE ALL ON SCHEMA "+pgx.Identifier{s}.Sanitize()+" FROM "+rid)
		}
		_, _ = pool.Exec(context.WithoutCancel(ctx), "DROP ROLE IF EXISTS "+rid)
	})
	for _, s := range schemas {
		sid := pgx.Identifier{s}.Sanitize()
		for _, stmt := range []string{
			"GRANT USAGE ON SCHEMA " + sid + " TO " + rid,
			"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA " + sid + " TO " + rid,
		} {
			if _, err := pool.Exec(ctx, stmt); err != nil {
				t.Fatalf("授权: %v", err)
			}
		}
	}
	return "SET LOCAL ROLE " + rid
}

// TestTenantReadAll 锁住读全部模式的边界：SELECT 放开全部租户，写仍只能
// 落当前租户；无租户的读全部合法（系统级批处理），无租户的写被拒。
func TestTenantReadAll(t *testing.T) {
	pool := dbtest.Pool(t)
	schema := dbtest.Schema(t, pool, "pgtx_readall", tenantMigration)
	ctx := context.Background()
	setRole := nonBypassRole(t, pool, schema)
	q := func(sql string) string { return fmt.Sprintf(sql, pgx.Identifier{schema}.Sanitize()) }
	if _, err := pool.Exec(ctx, q("INSERT INTO %s.documents VALUES ('d1', 't1', 'one'), ('d2', 't2', 'two')")); err != nil {
		t.Fatalf("预置数据: %v", err)
	}
	txr := pgtx.NewTenant(pool)
	count := func(ctx context.Context, db pgtx.DB) (int, error) {
		var n int
		err := db.QueryRow(ctx, q("SELECT count(*) FROM %s.documents")).Scan(&n)
		return n, err
	}

	// 运营（t1）读全部：两家的行都可见；写自己的行成功；冒名写别家仍被拒；
	// UPDATE/DELETE 只触及自己的行（SELECT 放开不等于写放开）。
	err := txr.Do(tx.WithReadAllTenants(withTenant(ctx, "t1")), func(ctx context.Context) error {
		db := pgtx.From(ctx, pool)
		if _, err := db.Exec(ctx, setRole); err != nil {
			return err
		}
		if n, err := count(ctx, db); err != nil || n != 2 {
			return fmt.Errorf("读全部应见两家的行：count=%d err=%v", n, err)
		}
		if _, err := db.Exec(ctx, q("INSERT INTO %s.documents VALUES ('d3', 't1', 'mine')")); err != nil {
			return fmt.Errorf("读全部模式下写自己的行应成功: %w", err)
		}
		tag, err := db.Exec(ctx, q("UPDATE %s.documents SET body = 'touched'"))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 2 {
			return fmt.Errorf("不带 WHERE 的 UPDATE 只该触及本租户的 2 行，实际 %d", tag.RowsAffected())
		}
		tag, err = db.Exec(ctx, q("DELETE FROM %s.documents WHERE id = 'd2'"))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			return fmt.Errorf("别家的行对 DELETE 不可见，实际删了 %d 行", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("读全部路径: %v", err)
	}
	err = txr.Do(tx.WithReadAllTenants(withTenant(ctx, "t1")), func(ctx context.Context) error {
		db := pgtx.From(ctx, pool)
		if _, err := db.Exec(ctx, setRole); err != nil {
			return err
		}
		_, err := db.Exec(ctx, q("INSERT INTO %s.documents VALUES ('d4', 't2', 'forged')"))
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("读全部模式下冒名写别家仍应被 WITH CHECK 拒，实际 %v", err)
	}

	// 无租户 + 读全部：系统级批处理的合法形态——读得到全部，写被拒。
	err = txr.Do(tx.WithReadAllTenants(ctx), func(ctx context.Context) error {
		db := pgtx.From(ctx, pool)
		if _, err := db.Exec(ctx, setRole); err != nil {
			return err
		}
		n, err := count(ctx, db)
		if err != nil || n != 3 {
			return fmt.Errorf("无租户读全部应见 3 行：count=%d err=%v", n, err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("无租户读全部: %v", err)
	}
	err = txr.Do(tx.WithReadAllTenants(ctx), func(ctx context.Context) error {
		db := pgtx.From(ctx, pool)
		if _, err := db.Exec(ctx, setRole); err != nil {
			return err
		}
		_, err := db.Exec(ctx, q("INSERT INTO %s.documents VALUES ('d5', 't1', 'no tenant')"))
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("无租户的写应被拒，实际 %v", err)
	}

	// 不带标记：一切如旧，只见本租户。
	err = txr.Do(withTenant(ctx, "t1"), func(ctx context.Context) error {
		db := pgtx.From(ctx, pool)
		if _, err := db.Exec(ctx, setRole); err != nil {
			return err
		}
		if n, err := count(ctx, db); err != nil || n != 2 {
			return fmt.Errorf("普通模式只见本租户：count=%d err=%v", n, err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("普通路径: %v", err)
	}
}

// TestTenantReadAllNestedMismatch 锁住「标记须在最外层 Do 之前打」：
// savepoint 里切模式会延续到外层事务结束，实现方直接拒绝。
func TestTenantReadAllNestedMismatch(t *testing.T) {
	pool := dbtest.Pool(t)
	dbtest.Schema(t, pool, "pgtx_nested", tenantMigration)
	ctx := withTenant(context.Background(), "t1")
	txr := pgtx.NewTenant(pool)

	err := txr.Do(ctx, func(ctx context.Context) error {
		return txr.Do(tx.WithReadAllTenants(ctx), func(context.Context) error { return nil })
	})
	if err == nil || !strings.Contains(err.Error(), "嵌套事务内切换") {
		t.Fatalf("嵌套内切成读全部应报错，实际 %v", err)
	}
	// 外层已是读全部，嵌套沿用同一 ctx：合法且幂等。
	ran := false
	err = txr.Do(tx.WithReadAllTenants(ctx), func(ctx context.Context) error {
		return txr.Do(ctx, func(context.Context) error { ran = true; return nil })
	})
	if err != nil || !ran {
		t.Fatalf("外层读全部、嵌套沿用应放行: err=%v ran=%v", err, ran)
	}
}

// TestTenantNestedScopeMismatch locks the whole transaction scope, not merely
// read-all mode. SET LOCAL survives releasing a savepoint, so changing the
// tenant in a nested Do would otherwise redirect the outer transaction.
func TestTenantNestedScopeMismatch(t *testing.T) {
	pool := dbtest.Pool(t)
	dbtest.Schema(t, pool, "pgtx_nested_scope", tenantMigration)
	outer := withTenant(context.Background(), "t1")
	tenant := pgtx.NewTenant(pool)

	for name, derive := range map[string]func(context.Context) context.Context{
		"tenant": func(ctx context.Context) context.Context {
			return callctx.With(ctx, callctx.Meta{TenantID: "t2"})
		},
		"read_all": tx.WithReadAllTenants,
	} {
		t.Run(name, func(t *testing.T) {
			ran := false
			err := tenant.Do(outer, func(ctx context.Context) error {
				innerErr := tenant.Do(derive(ctx), func(context.Context) error {
					ran = true
					return nil
				})
				if innerErr == nil || !strings.Contains(innerErr.Error(), "嵌套事务内切换作用域") {
					return fmt.Errorf("nested scope rebind should fail, got %v", innerErr)
				}
				var got string
				if err := pgtx.From(ctx, pool).QueryRow(ctx, "SELECT current_setting('app.tenant_id')").Scan(&got); err != nil {
					return err
				}
				if got != "t1" {
					return fmt.Errorf("outer tenant GUC changed to %q", got)
				}
				return nil
			})
			if err != nil || ran {
				t.Fatalf("nested scope rebind must not run callback or alter outer scope: err=%v ran=%v", err, ran)
			}
		})
	}

	// Independent adapters may share a transaction only when their derived
	// scope is identical. This preserves normal composition-root wiring.
	other := pgtx.NewTenant(pool)
	ran := false
	err := tenant.Do(outer, func(ctx context.Context) error {
		return other.Do(ctx, func(context.Context) error { ran = true; return nil })
	})
	if err != nil || !ran {
		t.Fatalf("same tenant scope across transactors should use a savepoint: err=%v ran=%v", err, ran)
	}

	// A plain outer transaction has no tenant GUC. Adding tenant isolation in a
	// nested savepoint is also a scope change, not an implicit upgrade.
	plain := pgtx.New(pool)
	err = plain.Do(context.Background(), func(ctx context.Context) error {
		return tenant.Do(withTenant(ctx, "t1"), func(context.Context) error { return nil })
	})
	if err == nil || !strings.Contains(err.Error(), "嵌套事务内切换作用域") {
		t.Fatalf("plain to tenant nested upgrade should fail, got %v", err)
	}
	err = tenant.Do(outer, func(ctx context.Context) error {
		return plain.Do(ctx, func(context.Context) error { return nil })
	})
	if err == nil || !strings.Contains(err.Error(), "嵌套事务内切换作用域") {
		t.Fatalf("tenant to plain nested downgrade should fail, got %v", err)
	}

	// A raw pgx transaction marker without pgtx's scope marker is not trusted:
	// otherwise a caller could erase the marker and rebind transaction-local GUCs.
	err = tenant.Do(outer, func(ctx context.Context) error {
		return tenant.Do(tx.With(context.Background(), pgtx.From(ctx, pool)), func(context.Context) error { return nil })
	})
	if err == nil || !strings.Contains(err.Error(), "无法验证嵌套事务作用域") {
		t.Fatalf("unscoped existing transaction should fail, got %v", err)
	}

	// Retaining a valid marker while replacing tx.Value with another raw pgx.Tx
	// must not authorize a savepoint on that different transaction.
	err = tenant.Do(outer, func(ctx context.Context) error {
		borrowed, err := pool.Begin(context.Background())
		if err != nil {
			return err
		}
		defer func() { _ = borrowed.Rollback(context.WithoutCancel(context.Background())) }()
		ran := false
		innerErr := tenant.Do(tx.With(ctx, borrowed), func(context.Context) error {
			ran = true
			return nil
		})
		if innerErr == nil || !strings.Contains(innerErr.Error(), "事务句柄") {
			return fmt.Errorf("borrowed transaction should fail, got %v", innerErr)
		}
		if ran {
			return errors.New("borrowed transaction callback ran")
		}
		var got string
		if err := pgtx.From(ctx, pool).QueryRow(ctx, "SELECT current_setting('app.tenant_id')").Scan(&got); err != nil {
			return err
		}
		if got != "t1" {
			return fmt.Errorf("outer tenant GUC changed to %q", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("outer transaction should continue after borrowed transaction rejection: %v", err)
	}
}

func TestNewRoutedTenantNilRoutePanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("NewRoutedTenant(pool, nil) 应当 panic")
		}
	}()
	pgtx.NewRoutedTenant(nil, nil)
}

// bareTenantSchema 建一个分区 schema 并以无前缀 DDL 应用租户表与策略——
// 与 pgmigrate 对分区域域的做法一致：事务内 SET LOCAL search_path 再跑
// 无前缀文件。
func bareTenantSchema(t *testing.T, pool *pgxpool.Pool, prefix string) string {
	t.Helper()
	schema := dbtest.Schema(t, pool, prefix, nil)
	ctx := context.Background()
	ptx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ptx.Rollback(context.WithoutCancel(ctx)) }()
	ddl := "SET LOCAL search_path TO " + pgx.Identifier{schema}.Sanitize() + ";\n" +
		"CREATE TABLE documents (id text PRIMARY KEY, tenant_id text NOT NULL, body text);\n" +
		pgtx.TenantScopeSQLBare() + pgtx.TenantPolicySQLBare("documents")
	if _, err := ptx.Exec(ctx, ddl); err != nil {
		t.Fatalf("应用无前缀迁移: %v", err)
	}
	if err := ptx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return schema
}

// TestNewRoutedTenant 验「分区 + 行级」双层：分区键选 schema，租户选行，
// 读全部只放开本分区内的全部租户——跨分区的行连表都碰不到。
func TestNewRoutedTenant(t *testing.T) {
	pool := dbtest.Pool(t)
	a := bareTenantSchema(t, pool, "pgtx_rt_a")
	b := bareTenantSchema(t, pool, "pgtx_rt_b")
	setRole := nonBypassRole(t, pool, a, b)
	schemas := map[string]string{"a": a, "b": b}
	txr := pgtx.NewRoutedTenant(pool, func(ctx context.Context) (string, error) {
		s, ok := schemas[callctx.From(ctx).Partition]
		if !ok {
			return "", fmt.Errorf("未知分区 %q", callctx.From(ctx).Partition)
		}
		return s, nil
	})
	at := func(partition, tenant string) context.Context {
		return callctx.With(context.Background(), callctx.Meta{Partition: partition, TenantID: tenant})
	}
	insert := func(ctx context.Context, id string) error {
		return txr.Do(ctx, func(ctx context.Context) error {
			db := pgtx.From(ctx, pool)
			if _, err := db.Exec(ctx, setRole); err != nil {
				return err
			}
			_, err := db.Exec(ctx, "INSERT INTO documents VALUES ($1, current_setting('app.tenant_id'), '')", id)
			return err
		})
	}
	count := func(ctx context.Context) int {
		var n int
		err := txr.Do(ctx, func(ctx context.Context) error {
			db := pgtx.From(ctx, pool)
			if _, err := db.Exec(ctx, setRole); err != nil {
				return err
			}
			return db.QueryRow(ctx, "SELECT count(*) FROM documents").Scan(&n)
		})
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	for _, in := range []struct{ p, t, id string }{
		{"a", "a1", "x1"}, {"a", "a1", "x2"}, {"a", "a2", "x3"}, {"b", "b1", "y1"},
	} {
		if err := insert(at(in.p, in.t), in.id); err != nil {
			t.Fatalf("写 %s/%s: %v", in.p, in.t, err)
		}
	}
	// 商户只见自己：a1 两行、a2 一行、b1 一行。
	if n := count(at("a", "a1")); n != 2 {
		t.Errorf("a/a1 应见 2 行，实际 %d", n)
	}
	if n := count(at("a", "a2")); n != 1 {
		t.Errorf("a/a2 应见 1 行，实际 %d", n)
	}
	// 运营读全部：a 的全部 = 3，b 的全部 = 1——两个分区各看各的。
	if n := count(tx.WithReadAllTenants(at("a", "platform"))); n != 3 {
		t.Errorf("a 分区读全部应见 3 行，实际 %d", n)
	}
	if n := count(tx.WithReadAllTenants(at("b", "platform"))); n != 1 {
		t.Errorf("b 分区读全部应见 1 行，实际 %d", n)
	}
	// 未知分区响亮失败。
	if err := insert(at("c", "c1"), "z"); err == nil || !strings.Contains(err.Error(), "未知分区") {
		t.Errorf("未知分区应报错，实际 %v", err)
	}
	// 每个分区各自过校验（普通角色下）。
	role := strings.TrimPrefix(setRole, "SET LOCAL ROLE ")
	for _, s := range []string{a, b} {
		err := pool.AcquireFunc(context.Background(), func(c *pgxpool.Conn) error {
			defer func() { _, _ = c.Exec(context.Background(), "RESET ROLE") }()
			if _, err := c.Exec(context.Background(), "SET ROLE "+role); err != nil {
				return err
			}
			return pgtx.VerifyTenantRLS(context.Background(), c, s)
		})
		if err != nil {
			t.Errorf("分区 %s 的校验应放行: %v", s, err)
		}
	}
}
