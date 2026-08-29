package pgtx_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit/pgtx"
	"github.com/forgeplex/appkit/tx"
)

// ---- 不需要 Postgres 的单测 ----

func TestNewPoolBadDSN(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		dsn  string
	}{
		{name: "非法 URL", dsn: "://bad"},
		{name: "非法参数", dsn: "postgres://h/db?sslmode=nonsense"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := pgtx.NewPool(context.Background(), tc.dsn); err == nil {
				t.Fatalf("NewPool(%q) 应当报错", tc.dsn)
			}
		})
	}
}

// fakeTx 经接口内嵌满足 pgx.Tx（方法未实现也不会被调用），
// 用于在无 DB 的情况下验证 tx 标记与 From 的取用逻辑。
type fakeTx struct{ pgx.Tx }

func TestTxMarkers(t *testing.T) {
	t.Parallel()
	base := context.Background()
	tests := []struct {
		name  string
		ctx   context.Context
		hasTx bool
	}{
		{name: "裸 ctx 无事务", ctx: base, hasTx: false},
		{name: "With 后有事务", ctx: tx.With(base, fakeTx{}), hasTx: true},
		{name: "Strip 后无事务", ctx: tx.Strip(tx.With(base, fakeTx{})), hasTx: false},
		{name: "Strip 裸 ctx 仍无事务", ctx: tx.Strip(base), hasTx: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tx.HasTx(tc.ctx); got != tc.hasTx {
				t.Fatalf("HasTx = %v, want %v", got, tc.hasTx)
			}
		})
	}
}

func TestFromWithoutDB(t *testing.T) {
	t.Parallel()
	base := context.Background()
	fake := fakeTx{}
	tests := []struct {
		name string
		ctx  context.Context
		want pgtx.DB
	}{
		{name: "无事务返回 pool", ctx: base, want: (*pgxpool.Pool)(nil)},
		{name: "有事务返回该句柄", ctx: tx.With(base, fake), want: fake},
		{name: "Strip 后回到 pool", ctx: tx.Strip(tx.With(base, fake)), want: (*pgxpool.Pool)(nil)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pgtx.From(tc.ctx, nil); got != tc.want {
				t.Fatalf("From = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestFromForeignHandlePanics(t *testing.T) {
	t.Parallel()
	ctx := tx.With(context.Background(), "不是 pgx.Tx")
	defer func() {
		if recover() == nil {
			t.Fatal("From 遇到异质事务句柄应当 panic")
		}
	}()
	pgtx.From(ctx, nil)
}

func TestDoForeignHandleErrors(t *testing.T) {
	t.Parallel()
	ctx := tx.With(context.Background(), 42)
	err := pgtx.New(nil).Do(ctx, func(context.Context) error { return nil })
	if err == nil {
		t.Fatal("Do 遇到异质事务句柄应当报错")
	}
}

// ---- 需要 Postgres 的测试（TEST_DATABASE_URL）----

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过需要 Postgres 的测试")
	}
	pool, err := pgtx.NewPool(context.Background(), dsn)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

var tableSeq atomic.Int64

func testTable(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	name := fmt.Sprintf("pgtx_test_%d_%d", time.Now().UnixNano(), tableSeq.Add(1))
	if _, err := pool.Exec(context.Background(), "CREATE TABLE "+name+" (v text NOT NULL)"); err != nil {
		t.Fatalf("建表: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+name)
	})
	return name
}

func insert(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table, v string) error {
	t.Helper()
	_, err := pgtx.From(ctx, pool).Exec(ctx, "INSERT INTO "+table+" (v) VALUES ($1)", v)
	return err
}

func visible(t *testing.T, db pgtx.DB, table, v string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE v = $1", v).Scan(&n); err != nil {
		t.Fatalf("查询 %s: %v", table, err)
	}
	return n > 0
}

func TestNewPoolOptions(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过需要 Postgres 的测试")
	}
	var first, second atomic.Bool
	pool, err := pgtx.NewPool(context.Background(), dsn,
		pgtx.WithMaxConns(3),
		pgtx.WithAfterConnect(func(context.Context, *pgx.Conn) error { first.Store(true); return nil }),
		pgtx.WithAfterConnect(func(context.Context, *pgx.Conn) error { second.Store(true); return nil }),
	)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	if got := pool.Config().MaxConns; got != 3 {
		t.Errorf("MaxConns = %d, want 3", got)
	}
	// NewPool 内的 Ping 已建立过连接，AfterConnect 必然执行过。
	if !first.Load() || !second.Load() {
		t.Errorf("AfterConnect 链未全部执行: first=%v second=%v", first.Load(), second.Load())
	}
}

func TestDoCommitRollback(t *testing.T) {
	pool := testPool(t)
	tr := pgtx.New(pool)
	errBoom := errors.New("boom")

	tests := []struct {
		name        string
		fnErr       error
		wantVisible bool
	}{
		{name: "提交后可见", fnErr: nil, wantVisible: true},
		{name: "出错回滚不可见", fnErr: errBoom, wantVisible: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			table := testTable(t, pool)
			err := tr.Do(context.Background(), func(ctx context.Context) error {
				if err := insert(ctx, t, pool, table, "a"); err != nil {
					return err
				}
				return tc.fnErr
			})
			if !errors.Is(err, tc.fnErr) {
				t.Fatalf("Do = %v, want %v", err, tc.fnErr)
			}
			if got := visible(t, pool, table, "a"); got != tc.wantVisible {
				t.Fatalf("提交后可见性 = %v, want %v", got, tc.wantVisible)
			}
		})
	}
}

func TestDoNestedSavepoint(t *testing.T) {
	pool := testPool(t)
	tr := pgtx.New(pool)
	errInner := errors.New("inner")
	errOuter := errors.New("outer")

	tests := []struct {
		name     string
		innerErr error
		outerErr error
		wantA    bool
		wantB    bool
	}{
		{name: "内外全部提交", wantA: true, wantB: true},
		{name: "内层回滚外层提交", innerErr: errInner, wantA: true, wantB: false},
		{name: "内层提交外层回滚", outerErr: errOuter, wantA: false, wantB: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			table := testTable(t, pool)
			err := tr.Do(context.Background(), func(ctx context.Context) error {
				if err := insert(ctx, t, pool, table, "a"); err != nil {
					return err
				}
				nestedErr := tr.Do(ctx, func(ctx context.Context) error {
					if err := insert(ctx, t, pool, table, "b"); err != nil {
						return err
					}
					return tc.innerErr
				})
				if !errors.Is(nestedErr, tc.innerErr) {
					return fmt.Errorf("嵌套 Do = %v, want %v", nestedErr, tc.innerErr)
				}
				// 内层错误被吞掉：模拟「子操作失败但整体继续」的部分回滚场景。
				return tc.outerErr
			})
			if !errors.Is(err, tc.outerErr) {
				t.Fatalf("Do = %v, want %v", err, tc.outerErr)
			}
			if gotA := visible(t, pool, table, "a"); gotA != tc.wantA {
				t.Errorf("a 可见性 = %v, want %v", gotA, tc.wantA)
			}
			if gotB := visible(t, pool, table, "b"); gotB != tc.wantB {
				t.Errorf("b 可见性 = %v, want %v", gotB, tc.wantB)
			}
		})
	}
}

func TestDoPanicRollsBackAndRethrows(t *testing.T) {
	pool := testPool(t)
	tr := pgtx.New(pool)
	table := testTable(t, pool)

	func() {
		defer func() {
			if p := recover(); p != "boom" {
				t.Fatalf("panic 值 = %v, want %q", p, "boom")
			}
		}()
		_ = tr.Do(context.Background(), func(ctx context.Context) error {
			if err := insert(ctx, t, pool, table, "a"); err != nil {
				return err
			}
			panic("boom")
		})
	}()

	if visible(t, pool, table, "a") {
		t.Fatal("panic 后写入不应可见")
	}
}

func TestFromInsideAndOutsideTx(t *testing.T) {
	pool := testPool(t)
	tr := pgtx.New(pool)
	table := testTable(t, pool)

	// 事务外：返回的就是 pool 本身。
	if got, ok := pgtx.From(context.Background(), pool).(*pgxpool.Pool); !ok || got != pool {
		t.Fatalf("事务外 From 应返回 pool 本身，得到 %T", pgtx.From(context.Background(), pool))
	}

	err := tr.Do(context.Background(), func(ctx context.Context) error {
		if !tx.HasTx(ctx) {
			return errors.New("Do 内 HasTx 应为 true")
		}
		inTx, ok := pgtx.From(ctx, pool).(pgx.Tx)
		if !ok {
			return fmt.Errorf("Do 内 From 应返回 pgx.Tx，得到 %T", pgtx.From(ctx, pool))
		}
		if err := insert(ctx, t, pool, table, "a"); err != nil {
			return err
		}
		// 未提交的写：事务内可见，事务外（pool 直查）不可见。
		if !visible(t, inTx, table, "a") {
			return errors.New("事务内应看到未提交的写")
		}
		if visible(t, pool, table, "a") {
			return errors.New("事务外不应看到未提交的写")
		}
		// Strip 后的 ctx 视同无事务：From 落回 pool。
		stripped := tx.Strip(ctx)
		if tx.HasTx(stripped) {
			return errors.New("Strip 后 HasTx 应为 false")
		}
		if _, ok := pgtx.From(stripped, pool).(*pgxpool.Pool); !ok {
			return errors.New("Strip 后 From 应落回 pool")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if tx.HasTx(context.Background()) {
		t.Fatal("Do 之外的 ctx 不应带事务标记")
	}
	if !visible(t, pool, table, "a") {
		t.Fatal("提交后应可见")
	}
}
