// Package pgtx 是 tx.Transactor 的 pgx 实现：事务句柄经 tx.With 藏进 ctx，
// 业务代码只见 context.Context。本包由框架装配注入（组合根 / 模块 Setup），
// 业务包只 import 接口面 appkit/tx；数据层（internal/postgres）经 From 取
// DBTX——ctx 内有事务用事务，无则直接用连接池。
package pgtx

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit/callctx"
	"github.com/forgeplex/appkit/internal/metrics"
	"github.com/forgeplex/appkit/tx"
)

// PoolOption 配置连接池。
type PoolOption func(*pgxpool.Config)

// WithAfterConnect 注册连接建立后的回调（典型用途：连接级初始化，如经
// otelpgx 装追踪、SET 本连接生效的 GUC）。
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
	// route 收在指针后面：函数不可比较，平铺进结构体会把 Transactor 变成
	// 不可比较类型（相对存量 tag 的 apidiff incompatible）。idem.mwConfig
	// 的 injectedOptions 是同款取舍。
	route *router
	// tenant 标记 NewTenant 构造：Do 开事务后把 callctx 里的租户身份落成
	// 事务级 GUC，供 RLS 策略读取（见 NewTenant 与 tenant.go）。
	tenant bool
}

// router 包一层路由函数以保持 Transactor 可比较（见 Transactor.route）。
type router struct {
	fn func(ctx context.Context) (string, error)
}

var _ tx.Transactor = (*Transactor)(nil)

// New 构造 Transactor。
func New(pool *pgxpool.Pool) *Transactor {
	return &Transactor{pool: pool}
}

// NewRouted 构造分区域 Transactor：每次 Do 在开启事务后调用 route 解析本次
// 落在哪个 schema，并 SET LOCAL search_path（事务结束自动还原，连接归还池
// 时是干净的）。route 通常从 callctx 的租户身份查组合根注入的分区映射。
//
// 返回错误即本次 Do 失败（事务回滚）——「查无分区」要让调用响亮地失败，
// 而不是静默落到默认 search_path 上把表不存在的错误甩给最里面那条查询。
// 返回空 schema 同样报错：想表达「不路由」就别用 NewRouted。
func NewRouted(pool *pgxpool.Pool, route func(ctx context.Context) (string, error)) *Transactor {
	if route == nil {
		panic("pgtx: NewRouted 的 route 为 nil——不需要路由时用 New")
	}
	return &Transactor{pool: pool, route: &router{fn: route}}
}

// NewTenant 构造租户隔离 Transactor（单 schema 域、按行隔离的形态）：
// 每次 Do 在开启事务后把 callctx 里的租户身份（authn 验签时从令牌 tid
// 焊入）落成事务级 GUC app.tenant_id——RLS 策略（TenantPolicySQL 生成）
// 据此过滤行，业务查询漏写 WHERE tenant_id 也查不到、写不进别家的行。
//
// 租户身份为空时不设 GUC：租户业务表的策略函数会对缺 GUC 的查询响亮
// 报错（见 TenantScopeSQL），而基础设施表（outbox/idem/audit——无
// tenant_id 列、无策略）的操作不受影响。跨租户批处理逐租户 callctx.With
// 后各开一次 Do；迁移文件内的跨租户回填在同一文件里先回填后挂策略。
//
// ctx 带 tx.WithReadAllTenants 标记时另落 GUC app.tenant_scope=all：
// SELECT 放开全部租户的行（tenant_isolation_read_all 策略），写入仍只能
// 落当前租户。标记须在最外层 Do 之前打——嵌套 Do 内切换模式报错，
// 因为 SET LOCAL 的作用域是整个事务而不是 savepoint。
func NewTenant(pool *pgxpool.Pool) *Transactor {
	return &Transactor{pool: pool, tenant: true}
}

// NewRoutedTenant 构造「分区 + 行级」双层隔离的 Transactor：每次 Do 先按
// route 把事务路由到分区 schema（NewRouted 的语义），再把租户身份落成
// 事务级 GUC（NewTenant 的语义）。这是「运营平台 + 多商户、每个平台一套
// 数据」的形态：分区键（callctx.Meta.Partition）决定落哪个 schema，
// 租户（callctx.Meta.TenantID）决定看哪些行——两个维度各走各的字段，
// route 读分区键、GUC 落租户，互不串。
//
// 分区内的迁移用无前缀 DDL（TenantScopeSQLBare / TenantPolicySQLBare），
// 与分区域域的基础迁移同一纪律。
func NewRoutedTenant(pool *pgxpool.Pool, route func(ctx context.Context) (string, error)) *Transactor {
	if route == nil {
		panic("pgtx: NewRoutedTenant 的 route 为 nil——不需要路由时用 NewTenant")
	}
	return &Transactor{pool: pool, route: &router{fn: route}, tenant: true}
}

// routeSchema 在事务开启后落位 search_path（嵌套幂等性见 beforeFn）。
func (t *Transactor) routeSchema(ctx context.Context, ptx pgx.Tx) error {
	if t.route == nil {
		return nil
	}
	schema, err := t.route.fn(ctx)
	if err != nil {
		return err
	}
	if !schemaRe.MatchString(schema) {
		return fmt.Errorf("pgtx: 路由返回的 schema 名 %q 不合法（须匹配 %s）", schema, schemaRe)
	}
	if _, err := ptx.Exec(ctx, "SET LOCAL search_path TO "+pgx.Identifier{schema}.Sanitize()); err != nil {
		return fmt.Errorf("pgtx: 设置 search_path: %w", err)
	}
	return nil
}

// beforeFn 落事务前置状态：分区的 search_path 与租户 GUC（各自按构造
// 形态启用）。嵌套 Do（savepoint）会再次走到这里：route 纯函数、租户
// 取自同一 ctx，重复执行解析出同样的值，幂等无害。
func (t *Transactor) beforeFn(ctx context.Context, ptx pgx.Tx) error {
	if err := t.routeSchema(ctx, ptx); err != nil {
		return err
	}
	return t.setTenantGUC(ctx, ptx)
}

// scopeKey 记录本事务已落位的租户可见范围（true = 读全部），供嵌套 Do
// 校验模式没有中途切换。
type scopeKey struct{}

// setTenantGUC 把 callctx 里的租户身份落成事务级 GUC（set_config 第三参
// true：事务结束自动还原，连接归还池时不带走）。空租户不设——语义见
// NewTenant。ctx 带读全部标记时另落 app.tenant_scope=all。
//
// 嵌套 Do 内的模式必须与外层一致：SET LOCAL 的作用域是整个事务，
// savepoint 里切成读全部、释放后外层剩下的查询也全放开了——这是静默
// 扩权，直接拒绝。
func (t *Transactor) setTenantGUC(ctx context.Context, ptx pgx.Tx) error {
	if !t.tenant {
		return nil
	}
	readAll := tx.ReadsAllTenants(ctx)
	if applied, nested := ctx.Value(scopeKey{}).(bool); nested && applied != readAll {
		return errors.New("pgtx: 嵌套事务内切换读全部租户模式——tx.WithReadAllTenants 必须在最外层 Do 之前打" +
			"（SET LOCAL 延续到外层事务结束，savepoint 内切换等于静默扩权）")
	}
	if tenant := callctx.From(ctx).TenantID; tenant != "" {
		if _, err := ptx.Exec(ctx, "SELECT set_config('"+tenantGUC+"', $1, true)", tenant); err != nil {
			return fmt.Errorf("pgtx: 设置租户 GUC: %w", err)
		}
	}
	if readAll {
		if _, err := ptx.Exec(ctx, "SELECT set_config('"+tenantScopeGUC+"', 'all', true)"); err != nil {
			return fmt.Errorf("pgtx: 设置读全部租户 GUC: %w", err)
		}
	}
	return nil
}

// schemaRe 与 pgmigrate/outbox 的约束一致：标识符（schema 名、表名）会
// 拼进 SQL，白名单防注入。
var schemaRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// beginner 抽象 pool 与 pgx.Tx 共同的 Begin：后者的 Begin 即 savepoint，
// 嵌套 Do 因此天然获得部分回滚语义。
type beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Do 划定一个事务边界：ctx 无事务则在池上开新事务；已有事务（嵌套 Do）则在
// 其上开 savepoint。fn 返回错误或 panic 都会回滚（panic 回滚后继续抛出），
// 否则提交。fn 收到的 ctx 携带事务句柄，经 From 取到的 DBTX 即该事务。
//
// 收尾靠无条件 defer 而非 recover：recover 兜不住 runtime.Goexit（testing
// 的 t.Fatal/FailNow 即是），Goexit 下事务既不提交也不回滚 = 连接泄漏到
// 进程退出。defer 在返回、panic、Goexit 三种退出路径下都执行，连接必归还。
func (t *Transactor) Do(ctx context.Context, fn func(ctx context.Context) error) (err error) {
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

	committed := false
	defer func() {
		if !committed {
			// 已提交/已收尾的事务再 Rollback 得 ErrTxClosed，忽略；其余回滚
			// 失败并进返回错误（panic/Goexit 路径 err 无人消费，并进去无害）。
			if rbErr := ptx.Rollback(rollbackCtx(ctx)); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				err = errors.Join(err, fmt.Errorf("pgtx: 回滚: %w", rbErr))
			}
		}
	}()

	if err := t.beforeFn(ctx, ptx); err != nil {
		return err
	}
	fnCtx := tx.With(ctx, ptx)
	if t.tenant {
		// 记下本事务已落位的可见范围：嵌套 Do 据此拒绝中途切换读全部模式。
		fnCtx = context.WithValue(fnCtx, scopeKey{}, tx.ReadsAllTenants(ctx))
	}
	if err := fn(fnCtx); err != nil {
		return err
	}
	if err := ptx.Commit(ctx); err != nil {
		return fmt.Errorf("pgtx: 提交: %w", err)
	}
	committed = true
	return nil
}

// rollbackCtx 剥离取消信号：fn 常因 ctx 取消而失败，若回滚也用已取消的 ctx，
// ROLLBACK 发不出去，连接会被整个废弃。回滚必须尽力完成以归还健康连接。
func rollbackCtx(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

// DB 与 sqlc 生成代码的 DBTX 接口签名一致：pgx.Tx 与 *pgxpool.Pool 都满足它，
// sqlc Querier 拿它即可透明地跑在事务内或池上。含 SendBatch——sqlc 只要在
// 任意一条查询上见到批处理注解（:batchexec/:batchmany/:copyfrom 等），就会
// 给该包共享的 DBTX 整体加上 SendBatch，缺它则 From 的返回值赋给生成的
// Querier 时编译不过。
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

var (
	_ DB = (*pgxpool.Pool)(nil)
	_ DB = (pgx.Tx)(nil)
)

// From 返回当前应使用的 DBTX：ctx 携带事务句柄（Do 之内）返回该 pgx.Tx，
// 否则返回 pool。句柄不是 pgx.Tx 时 panic 而非静默落回 pool——静默落回会把
// 本应在事务内的写悄悄漏出事务边界。
//
// 分区域域（NewRouted）的查询必须经 Do：search_path 是事务级的，From 直查
// 落在池连接的默认 search_path 上，表不存在即报错——失败响亮，不会静默
// 读写错误的分区，但分区域的业务查询一律包进事务是硬纪律。
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
