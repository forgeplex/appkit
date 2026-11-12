package scaffold

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/internal/dbtest"
	"github.com/forgeplex/appkit/pgmigrate"
	"github.com/forgeplex/appkit/pgtx"
)

// 脚手架声明的 sqlc NUMERIC override 必须真能过 pgx。
//
// 这类错编译期完全无感：override 指向 money.Money 时生成仓库照常编译，
// 一碰金额列才在运行期炸出 "cannot scan numeric (OID 1700) in binary
// format into *money.Money"（money.Money 不实现任何 pgx/database-sql
// 接口，单个 NUMERIC 列也还原不了币种）。编译测试在这里结构性失明，
// 所以机检落在真库读写上：把渲染后 sqlc.yaml 里声明的类型原样
// 写进 NUMERIC、读出来，标度都不能丢。
//
// 无 TEST_DATABASE_URL 时在 roundTripDecimal 里经 dbtest.Pool skip
// （与 idem/audit 的集成测试同门槛，`make test-db` 才会跑到）；
// 生成仓库与读 yaml 不需要库，skip 前照常执行。
func TestDomainNumericOverrideRoundTrips(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	if err := Domain(Options{Name: "ledger", Dir: dir}, nil); err != nil {
		t.Fatalf("Domain: %v", err)
	}
	goType := numericOverrideGoType(t, readFile(t, dir, "sqlc.yaml"))

	// 白名单：本测试能替 pgx 验证过的 override 目标类型。换新类型时先
	// 在这里登记一个 case（并顺手加真库断言）——没登记就改模板，
	// 等于把「编译得过」又当成了「跑得起」。
	switch goType {
	case "github.com/shopspring/decimal.Decimal":
		roundTripDecimal(t)
	default:
		t.Fatalf("sqlc.yaml 把 NUMERIC override 指向 %s，未登记进本测试的类型表。\n"+
			"新金额类型必须先在这里过真库读写（money.Money 就是栽在这：\n"+
			"不实现 Valuer/Scanner，扫描 OID 1700 直接失败）", goType)
	}
}

// numericOverrideGoType 从渲染后的 sqlc.yaml 取 NUMERIC override 的 go_type。
// 模板形状固定，按行扫描即可，不值得为它引 yaml 依赖。
func numericOverrideGoType(t *testing.T, yml string) string {
	t.Helper()
	lines := strings.Split(yml, "\n")
	for i, l := range lines {
		if !strings.Contains(l, `db_type: "pg_catalog.numeric"`) {
			continue
		}
		for _, next := range lines[i+1:] {
			rest, ok := strings.CutPrefix(strings.TrimSpace(next), `go_type: "`)
			if !ok {
				continue
			}
			return strings.TrimSuffix(rest, `"`)
		}
	}
	t.Fatal("渲染后的 sqlc.yaml 未找到 pg_catalog.numeric override（脚手架违约：金额列类型没人管了）")
	return ""
}

// roundTripDecimal 用超过任何 float 精度的标度往返一次：值与标度都得原样回来。
func roundTripDecimal(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Pool(t)
	schema := dbtest.Schema(t, pool, "scaffold_t", func(schema string) string {
		return `CREATE TABLE "` + schema + `".t (amount numeric NOT NULL, maybe numeric)`
	})

	// 27 位小数：float64 只有 ~15-17 位有效数字，等价性即证明没走过 float。
	in := decimal.RequireFromString("12.345678901234567890123456789")
	if _, err := pool.Exec(ctx,
		`INSERT INTO "`+schema+`".t (amount, maybe) VALUES ($1, $2)`, in, nil); err != nil {
		t.Fatalf("写入 decimal.Decimal 参数: %v", err)
	}

	var out decimal.Decimal
	var maybe *decimal.Decimal
	if err := pool.QueryRow(ctx,
		`SELECT amount, maybe FROM "`+schema+`".t`).Scan(&out, &maybe); err != nil {
		t.Fatalf("扫描 numeric → decimal.Decimal: %v", err)
	}
	if out.String() != in.String() {
		t.Fatalf("往返失真: 写入 %s（exp=%d），读回 %s（exp=%d）",
			in, in.Exponent(), out, out.Exponent())
	}
	if maybe != nil {
		t.Fatalf("NULL 应扫描为 nil *decimal.Decimal，实际 %v", *maybe)
	}
}

// 租户形态的生成物必须自身通过严格隔离校验（pgtx.VerifyTenantIsolation）。
//
// 这条用例把三件各自独立演进的东西锁在一起，任何一件单独改动都会让它变红：
// 库函数写出去的策略与函数文本（校验要认）、基础迁移建的基础设施表（校验的
// 免声明清单要覆盖）、样例业务表（不能是一张没声明的无租户表）。golden 只锁
// 文本，这里把生成的迁移真应用到库里再校验。
func TestDomainTenantMigrationsPassStrictIsolation(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	role := pgx.Identifier{fmt.Sprintf("scaffold_iso_%d", time.Now().UnixNano())}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE ROLE "+role+" NOLOGIN NOBYPASSRLS"); err != nil {
		t.Fatalf("建角色: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.WithoutCancel(ctx), "DROP ROLE IF EXISTS "+role) })

	for _, mode := range []struct {
		name        string
		partitioned bool
	}{
		{name: "租户域"},
		{name: "分区加行级", partitioned: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			// 域名即 schema 名（带前缀形态的迁移里写死了它），取一个不会撞的。
			domain := fmt.Sprintf("iso%d", time.Now().UnixNano())
			schema := domain
			if mode.partitioned {
				schema = domain + "_p1" // 分区 schema 由组合根映射决定，与域名无关
			}
			dir := filepath.Join(t.TempDir(), domain)
			if err := Domain(Options{Name: domain, Dir: dir, Tenant: true, Partitioned: mode.partitioned}, nil); err != nil {
				t.Fatalf("Domain: %v", err)
			}
			t.Cleanup(func() {
				_, _ = pool.Exec(context.WithoutCancel(ctx),
					"DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			})
			if err := pgmigrate.Runner(pool)(ctx, []appkit.MigrationSet{{
				Schema: schema, FS: os.DirFS(filepath.Join(dir, "db", "migrations")), Module: domain,
			}}); err != nil {
				t.Fatalf("应用生成的迁移: %v", err)
			}
			err := pool.AcquireFunc(ctx, func(c *pgxpool.Conn) error {
				defer func() { _, _ = c.Exec(ctx, "RESET ROLE") }()
				if _, err := c.Exec(ctx, "SET ROLE "+role); err != nil {
					return err
				}
				return pgtx.VerifyTenantIsolation(ctx, c, schema)
			})
			if err != nil {
				t.Fatalf("生成物应通过严格隔离校验，实际：\n%v", err)
			}
		})
	}
}
