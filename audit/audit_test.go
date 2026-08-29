package audit_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/audit"
	"github.com/forgeplex/appkit/pgtx"
	"github.com/forgeplex/appkit/tx"
)

// txMarked 只为走过 HasTx 守卫以测试字段校验，不触库。
func txMarked(ctx context.Context) context.Context { return tx.With(ctx, struct{}{}) }

func TestMigrationSQLRejectsBadSchema(t *testing.T) {
	for _, s := range []string{"", "Order", "a.b", `x"; DROP`, "1abc"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("schema %q 应被拒绝", s)
				}
			}()
			audit.MigrationSQL(s)
		}()
	}
}

func TestRecordOutsideTxRejected(t *testing.T) {
	r := audit.NewRecorder(nil, "demo")
	err := r.Record(context.Background(), audit.Entry{Actor: "a", Action: "b", Entity: "c"})
	if !apperr.Is(err, apperr.CodeTxBoundary) {
		t.Fatalf("事务外 Record 应返回 TX_BOUNDARY，实际 %v", err)
	}
}

func TestRecordValidation(t *testing.T) {
	r := audit.NewRecorder(nil, "demo")
	// 用带事务标记的 ctx 走到字段校验（不会触库）。
	ctx := txMarked(context.Background())
	err := r.Record(ctx, audit.Entry{Actor: "a"})
	if !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("缺必填字段应返回 INVALID_ARGUMENT，实际 %v", err)
	}
}

func TestRecordIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("未设置 TEST_DATABASE_URL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	schema := fmt.Sprintf("audit_t%08x", rand.Uint32())
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") })
	if _, err := pool.Exec(ctx, audit.MigrationSQL(schema)); err != nil {
		t.Fatal(err)
	}

	rec := audit.NewRecorder(pool, schema)
	transactor := pgtx.New(pool)

	// 提交路径：审计随事务可见。
	err = transactor.Do(ctx, func(ctx context.Context) error {
		return rec.Record(ctx, audit.Entry{
			Actor: "user-1", Action: "entry.post", Entity: "ledger_entry", EntityID: "e-1",
			Before: nil, After: map[string]any{"amount": "10.50"}, Meta: map[string]string{"ip": "127.0.0.1"},
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := count(t, pool, schema); got != 1 {
		t.Fatalf("提交后审计条数 = %d, want 1", got)
	}

	// 回滚路径：审计与业务同生共死。
	boom := fmt.Errorf("boom")
	err = transactor.Do(ctx, func(ctx context.Context) error {
		if err := rec.Record(ctx, audit.Entry{Actor: "u", Action: "x", Entity: "y", EntityID: "z"}); err != nil {
			return err
		}
		return boom
	})
	if err == nil {
		t.Fatal("应返回业务错误")
	}
	if got := count(t, pool, schema); got != 1 {
		t.Fatalf("回滚后审计条数 = %d, want 1", got)
	}

	// NULL 语义：Before/Meta 为 nil 时列为 NULL。
	_ = transactor.Do(ctx, func(ctx context.Context) error {
		return rec.Record(ctx, audit.Entry{Actor: "u", Action: "a", Entity: "e", EntityID: "1"})
	})
	var nullBefore int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM "`+schema+`".audit_log WHERE before IS NULL AND entity_id = '1'`).Scan(&nullBefore); err != nil {
		t.Fatal(err)
	}
	if nullBefore != 1 {
		t.Fatalf("nil Before 应落为 NULL，count = %d", nullBefore)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, schema string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM "`+schema+`".audit_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
