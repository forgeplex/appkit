package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit"
)

// Relay 轮询参数默认值。
const (
	DefaultBatchSize   = 100
	DefaultInterval    = time.Second
	DefaultLease       = 30 * time.Second
	DefaultMaxAttempts = 10

	// maxBackoff 封顶失败退避（interval * 2^attempts），避免长故障后
	// 恢复等待无界拉长。
	maxBackoff = 5 * time.Minute
)

// RelayOption 配置 Relay。
type RelayOption func(*Relay)

// WithBatchSize 设置单批拉取上限。非正值忽略。
func WithBatchSize(n int) RelayOption {
	return func(r *Relay) {
		if n > 0 {
			r.batch = n
		}
	}
}

// WithInterval 设置空批/出错后的轮询间隔，兼作失败退避的基数。非正值忽略。
func WithInterval(d time.Duration) RelayOption {
	return func(r *Relay) {
		if d > 0 {
			r.interval = d
		}
	}
}

// WithLease 设置 claim 租约时长：投递进程在收尾前崩溃时，租约到期后事件才
// 会被本副本或其他副本接管重投。应大于单批投递的最坏耗时。非正值忽略。
func WithLease(d time.Duration) RelayOption {
	return func(r *Relay) {
		if d > 0 {
			r.lease = d
		}
	}
}

// WithMaxAttempts 设置单事件投递次数上限，达到即置 failed_at 进入死信。
// 非正值忽略。
func WithMaxAttempts(n int) RelayOption {
	return func(r *Relay) {
		if n > 0 {
			r.maxAttempts = n
		}
	}
}

// Relay 把 {schema}.outbox 中待投递的事件投递到 Bus 并标记 published_at，
// 采用 claim/lease 两段式（详见包注释）。claim 短事务内的 FOR UPDATE SKIP
// LOCKED 加 claimed_until 租约保证多副本（水平扩容或滚动发布重叠期）互不
// 重复投递；投递期间不持有连接池连接，同池的消费者事务不会与 relay 形成
// hold-and-wait 死锁。
//
// 典型装配（模块 Register 内）：
//
//	done := make(chan error, 1)
//	reg.OnStart(appkit.StageWorker, func(ctx context.Context) error {
//		go func() { done <- relay.Run(ctx) }()
//		return nil
//	})
//	reg.OnStop(func(ctx context.Context) error {
//		select {
//		case err := <-done:
//			return err
//		case <-ctx.Done(): // 关停预算耗尽，不再等 worker
//			return ctx.Err()
//		}
//	})
type Relay struct {
	pool        *pgxpool.Pool
	schema      string
	bus         Bus
	batch       int
	interval    time.Duration
	lease       time.Duration
	maxAttempts int
	log         *slog.Logger

	claimSelectSQL string
	claimMarkSQL   string
	publishedSQL   string
	retrySQL       string
	deadSQL        string
	releaseSQL     string
}

// NewRelay 构造 Relay。schema 不合法时 panic。
func NewRelay(pool *pgxpool.Pool, schema string, bus Bus, opts ...RelayOption) *Relay {
	mustSchema(schema)
	r := &Relay{
		pool:        pool,
		schema:      schema,
		bus:         bus,
		batch:       DefaultBatchSize,
		interval:    DefaultInterval,
		lease:       DefaultLease,
		maxAttempts: DefaultMaxAttempts,
		log:         slog.Default(),
		// 时长参数一律以毫秒整数传入，避免依赖驱动的 interval 编码。
		claimSelectSQL: fmt.Sprintf(
			`SELECT id, topic, payload, meta, attempts FROM %s.outbox
			 WHERE published_at IS NULL AND failed_at IS NULL
			   AND next_attempt_at <= now()
			   AND (claimed_until IS NULL OR claimed_until < now())
			 ORDER BY created_at, id LIMIT $1 FOR UPDATE SKIP LOCKED`,
			ident(schema)),
		claimMarkSQL: fmt.Sprintf(
			`UPDATE %s.outbox SET claimed_until = now() + $2 * interval '1 millisecond' WHERE id = ANY($1)`,
			ident(schema)),
		publishedSQL: fmt.Sprintf(
			`UPDATE %s.outbox SET published_at = now(), claimed_until = NULL WHERE id = ANY($1)`,
			ident(schema)),
		retrySQL: fmt.Sprintf(
			`UPDATE %s.outbox SET attempts = attempts + 1,
			        next_attempt_at = now() + $2 * interval '1 millisecond',
			        last_error = $3, claimed_until = NULL
			 WHERE id = $1`,
			ident(schema)),
		deadSQL: fmt.Sprintf(
			`UPDATE %s.outbox SET attempts = attempts + 1, failed_at = now(),
			        last_error = $2, claimed_until = NULL
			 WHERE id = $1`,
			ident(schema)),
		releaseSQL: fmt.Sprintf(
			`UPDATE %s.outbox SET claimed_until = NULL WHERE id = ANY($1)`,
			ident(schema)),
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Run 阻塞运行轮询循环直到 ctx 取消（正常退出，返回 nil）。
// 单轮失败（数据库抖动、某 handler 出错甚至 panic）不终止循环：失败的事件
// 按退避重试、成功的照常推进，这正是 outbox 的至少一次语义；错误仅记日志。
func (r *Relay) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		n, err := r.relayOnce(ctx)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				r.log.Warn("outbox: relay 本轮投递失败，等待下轮重试", "schema", r.schema, "err", err)
			}
		case n >= r.batch:
			// 满批说明可能仍有积压，立即拉下一批。
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.interval):
		}
	}
}

// claimedEvent 是 claim 阶段选中的一行。attempts 供失败退避决策；meta 延迟
// 到投递前才反序列化——坏 meta 按该条投递失败退避处理，不再阻塞整批。
type claimedEvent struct {
	id       string
	topic    string
	payload  []byte
	meta     []byte
	attempts int
}

// relayOnce 以 claim/lease 两段式处理一批。
//
// ① claim（短事务）：FOR UPDATE SKIP LOCKED 选中到期待投递事件、写
// claimed_until 租约后立即提交——投递期间连接已归还，同池消费者（如 Inbox）
// 再 Begin 不会与 relay 形成 hold-and-wait 死锁。
// ② 投递（不持连接）：逐条 bus.Publish，panic 恢复为错误；单条失败即停止
// 本批后续投递（批内尽力保序），未投递者立即释放租约由下轮重拣。
// ③ 收尾（短语句）：成功者置 published_at；失败者记 attempts 与指数退避
// next_attempt_at、写 last_error，达重试上限则置 failed_at 进入死信。
//
// 崩溃于 ②③ 之间时租约到期后重投——至少一次语义不变，重复由 inbox 去重吸收。
// 返回本轮 claim 的事件数（供 Run 判断是否仍有积压）。
func (r *Relay) relayOnce(ctx context.Context) (n int, err error) {
	claimed, err := r.claim(ctx)
	if err != nil || len(claimed) == 0 {
		return 0, err
	}

	var (
		delivered []string // 已成功投递的事件 id
		rest      []string // 失败后未尝试投递的事件 id
		failedIdx = -1
		pubErr    error
	)
	for i := range claimed {
		if derr := r.deliver(ctx, claimed[i]); derr != nil {
			failedIdx = i
			pubErr = fmt.Errorf("outbox: 投递事件 %s（topic %q）: %w", claimed[i].id, claimed[i].topic, derr)
			for _, later := range claimed[i+1:] {
				rest = append(rest, later.id)
			}
			break
		}
		delivered = append(delivered, claimed[i].id)
	}

	// 收尾必须尽力完成：ctx 已取消也要把已投递的结果落库，否则重复投递被放大。
	finCtx := context.WithoutCancel(ctx)
	var finErrs []error
	if len(delivered) > 0 {
		if _, err := r.pool.Exec(finCtx, r.publishedSQL, delivered); err != nil {
			finErrs = append(finErrs, fmt.Errorf("outbox: 标记 published_at: %w", err))
		}
	}
	if failedIdx >= 0 {
		if err := r.recordFailure(finCtx, claimed[failedIdx], pubErr); err != nil {
			finErrs = append(finErrs, err)
		}
	}
	if len(rest) > 0 {
		if _, err := r.pool.Exec(finCtx, r.releaseSQL, rest); err != nil {
			finErrs = append(finErrs, fmt.Errorf("outbox: 释放未投递事件的租约: %w", err))
		}
	}
	return len(claimed), errors.Join(append(finErrs, pubErr)...)
}

// claim 短事务：选中到期待投递事件并写租约，随即提交归还连接。
func (r *Relay) claim(ctx context.Context) ([]claimedEvent, error) {
	ptx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("outbox: 开启 claim 事务: %w", err)
	}
	// 提交成功后的 Rollback 是空操作；用免取消 ctx 保证失败路径能归还健康连接。
	defer func() { _ = ptx.Rollback(context.WithoutCancel(ctx)) }()

	rows, err := ptx.Query(ctx, r.claimSelectSQL, r.batch)
	if err != nil {
		return nil, fmt.Errorf("outbox: 拉取待投递事件: %w", err)
	}
	var (
		claimed []claimedEvent
		ids     []string
	)
	for rows.Next() {
		var ce claimedEvent
		if err := rows.Scan(&ce.id, &ce.topic, &ce.payload, &ce.meta, &ce.attempts); err != nil {
			rows.Close()
			return nil, fmt.Errorf("outbox: 读取事件行: %w", err)
		}
		claimed = append(claimed, ce)
		ids = append(ids, ce.id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("outbox: 遍历事件行: %w", err)
	}
	if len(claimed) == 0 {
		return nil, nil
	}
	if _, err := ptx.Exec(ctx, r.claimMarkSQL, ids, r.lease.Milliseconds()); err != nil {
		return nil, fmt.Errorf("outbox: 写入 claim 租约: %w", err)
	}
	if err := ptx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("outbox: 提交 claim 事务: %w", err)
	}
	return claimed, nil
}

// deliver 反序列化 meta 并投递单条事件。bus.Publish（含其中全部 handler）的
// panic 在此恢复为错误：消费路径的 panic 不能崩掉进程——重启后按序重取同一
// 事件只会形成崩溃循环，转入退避重试才是正解。
func (r *Relay) deliver(ctx context.Context, ce claimedEvent) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("handler panic: %v", p)
			r.log.Error("outbox: 投递时 handler panic，已恢复并转入退避重试",
				"schema", r.schema, "event_id", ce.id, "topic", ce.topic,
				"panic", p, "stack", string(debug.Stack()))
		}
	}()
	var m map[string]string
	if len(ce.meta) > 0 {
		if err := json.Unmarshal(ce.meta, &m); err != nil {
			return fmt.Errorf("meta 反序列化: %w", err)
		}
	}
	return r.bus.Publish(ctx, appkit.Event{ID: ce.id, Topic: ce.topic, Payload: ce.payload, Meta: m})
}

// recordFailure 给失败事件记退避；达重试上限则置 failed_at 进入死信，
// 移出投递热路径（不再阻塞队头），等待人工介入。
func (r *Relay) recordFailure(ctx context.Context, ce claimedEvent, cause error) error {
	attempts := ce.attempts + 1
	if attempts >= r.maxAttempts {
		if _, err := r.pool.Exec(ctx, r.deadSQL, ce.id, cause.Error()); err != nil {
			return fmt.Errorf("outbox: 标记死信: %w", err)
		}
		r.log.Error("outbox: 事件投递重试达上限，转入死信（failed_at），需人工处理",
			"schema", r.schema, "event_id", ce.id, "topic", ce.topic,
			"attempts", attempts, "err", cause)
		return nil
	}
	delay := backoff(r.interval, ce.attempts)
	if _, err := r.pool.Exec(ctx, r.retrySQL, ce.id, delay.Milliseconds(), cause.Error()); err != nil {
		return fmt.Errorf("outbox: 记录失败退避: %w", err)
	}
	return nil
}

// backoff 计算第 attempts 次失败后的退避：base * 2^attempts，封顶 maxBackoff。
func backoff(base time.Duration, attempts int) time.Duration {
	d := base
	for range attempts {
		d *= 2
		if d >= maxBackoff || d <= 0 { // d <= 0 即移位溢出
			return maxBackoff
		}
	}
	return min(d, maxBackoff)
}
