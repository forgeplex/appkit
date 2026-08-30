// Package pgtx 是 tx.Transactor 的 pgx 实现：事务句柄经 tx.With 藏进 ctx，
// 业务代码只见 context.Context。本包由框架装配注入（组合根 / 模块 Setup），
// 业务包只 import 接口面 appkit/tx；数据层（internal/postgres）经 From 取
// DBTX——ctx 内有事务用事务，无则直接用连接池。
package pgtx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit/internal/metrics"
	"github.com/forgeplex/appkit/tx"
)

// PoolOption 配置连接池。
type PoolOption func(*pgxpool.Config)

// WithAfterConnect 注册连接建立后的回调（典型用途：注册 money 的 NUMERIC codec）。
// 多次调用按注册顺序依次执行，任一回调出错该连接即建立失败。
func WithAfterConnect(fn func(context.Context, *pgx.Conn) error) PoolOption {
	return func(c *pgxpool.Config) {
		prev := c.AfterConnect
		if prev == nil {
			c.AfterConnect = fn
			return
		}
		c.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			if err := prev(ctx, conn); err != nil {
				return err
			}
			return fn(ctx, conn)
		}
	}
}

// WithMaxConns 设置连接池上限。按 DESIGN §8，连接池按数据库共享而非按模块独占。
func WithMaxConns(n int32) PoolOption {
	return func(c *pgxpool.Config) { c.MaxConns = n }
}

// NewPool 解析 DSN、应用选项并建池，随即 Ping 一次——配置错误与数据库不可达
// 都在启动期暴露（fail-fast），而不是留到第一条查询。
func NewPool(ctx context.Context, dsn string, opts ...PoolOption) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pgtx: 解析 DSN: %w", err)
	}
	// 默认埋点在 opts 之前装：需要自己的 tracer（如 otelpgx）的用户用
	// PoolOption 覆盖掉即可。
	cfg.ConnConfig.Tracer = queryTracer{}
	for _, o := range opts {
		o(cfg)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgtx: 创建连接池: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgtx: ping: %w", err)
	}
	return pool, nil
}

// queryTracer 给每条 Query/QueryRow/Exec 记一条耗时与结果指标。
// 数据库是最常见的故障源，也是最容易"没埋点所以查不出来"的一层。
type queryTracer struct{}

type traceKey struct{}

// traceState 在 start 时算好 SQL 动词——TraceQueryEnd 拿不到 SQL，
// 而 CommandTag 在出错时是空的，恰好是最需要知道动词的时候。
type traceState struct {
	start time.Time
	op    string
}

func (queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, traceKey{}, traceState{start: time.Now(), op: metrics.Operation(data.SQL)})
}

func (queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	st, ok := ctx.Value(traceKey{}).(traceState)
	if !ok {
		return
	}
	metrics.DBQueryOp(ctx, st.op, data.Err, st.start)
}

// Transactor 实现 tx.Transactor。
type Transactor struct {
	pool *pgxpool.Pool
}

var _ tx.Transactor = (*Transactor)(nil)

// New 构造 Transactor。
func New(pool *pgxpool.Pool) *Transactor {
	return &Transactor{pool: pool}
}

// beginner 抽象 pool 与 pgx.Tx 共同的 Begin：后者的 Begin 即 savepoint，
// 嵌套 Do 因此天然获得部分回滚语义。
type beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Do 划定一个事务边界：ctx 无事务则在池上开新事务；已有事务（嵌套 Do）则在
// 其上开 savepoint。fn 返回错误或 panic 都会回滚（panic 回滚后继续抛出），
// 否则提交。fn 收到的 ctx 携带事务句柄，经 From 取到的 DBTX 即该事务。
func (t *Transactor) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	var b beginner = t.pool
	if h := tx.Value(ctx); h != nil {
		cur, ok := h.(pgx.Tx)
		if !ok {
			return fmt.Errorf("pgtx: ctx 携带的事务句柄是 %T 而非 pgx.Tx：混用了不同的 Transactor 实现", h)
		}
		b = cur
	}
	ptx, err := b.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgtx: 开启事务: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = ptx.Rollback(rollbackCtx(ctx))
			panic(p)
		}
	}()

	if err := fn(tx.With(ctx, ptx)); err != nil {
		if rbErr := ptx.Rollback(rollbackCtx(ctx)); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			return errors.Join(err, fmt.Errorf("pgtx: 回滚: %w", rbErr))
		}
		return err
	}
	if err := ptx.Commit(ctx); err != nil {
		return fmt.Errorf("pgtx: 提交: %w", err)
	}
	return nil
}

// rollbackCtx 剥离取消信号：fn 常因 ctx 取消而失败，若回滚也用已取消的 ctx，
// ROLLBACK 发不出去，连接会被整个废弃。回滚必须尽力完成以归还健康连接。
func rollbackCtx(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

// DB 与 sqlc 生成代码的 DBTX 接口签名一致：pgx.Tx 与 *pgxpool.Pool 都满足它，
// sqlc Querier 拿它即可透明地跑在事务内或池上。
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var (
	_ DB = (*pgxpool.Pool)(nil)
	_ DB = (pgx.Tx)(nil)
)

// From 返回当前应使用的 DBTX：ctx 携带事务句柄（Do 之内）返回该 pgx.Tx，
// 否则返回 pool。句柄不是 pgx.Tx 时 panic 而非静默落回 pool——静默落回会把
// 本应在事务内的写悄悄漏出事务边界。
func From(ctx context.Context, pool *pgxpool.Pool) DB {
	if h := tx.Value(ctx); h != nil {
		ptx, ok := h.(pgx.Tx)
		if !ok {
			panic(fmt.Sprintf("pgtx: ctx 携带的事务句柄是 %T 而非 pgx.Tx：混用了不同的 Transactor 实现", h))
		}
		return ptx
	}
	return pool
}
