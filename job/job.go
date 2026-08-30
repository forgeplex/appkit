// Package job 提供跨副本互斥的周期任务。
//
// 服务一旦跑到两个副本，"每小时清理一次过期数据"这类任务就会同时跑两遍。
// 多数时候只是浪费，偶尔是数据损坏（对账重复入账、通知重复发送）。
// 正确做法是给任务加一把集群级的锁，本包用 Postgres 的 advisory lock 实现：
// 不建表、不留垃圾，持锁连接一断 Postgres 就自动放锁——副本被 kill -9
// 也不会留下永久锁死的任务。
//
// 典型用法是与 appkit.Registry.Worker 组合，生命周期交给框架：
//
//	reg.Worker("cleanup", job.Every(pool, job.Task{
//		Name:     "ledger.cleanup",
//		Interval: time.Hour,
//		Run:      svc.CleanupExpired,
//	}))
//
// 抢不到锁的副本跳过本轮，不阻塞、不排队——周期任务迟一轮无所谓，
// 排队积压才是事故。
package job

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit/internal/metrics"
)

// ErrLocked 表示锁正被其它副本持有，本轮跳过。
var ErrLocked = errors.New("job: 任务锁被其它副本持有")

// Task 描述一个周期任务。
type Task struct {
	// Name 是集群内的任务标识，同名任务互斥。跨域可能重名，
	// 建议带域前缀（"ledger.cleanup"）。必填。
	Name string
	// Interval 是执行周期，必须为正。
	Interval time.Duration
	// Run 是任务体。返回错误只记日志、不中断循环——一次清理失败不该
	// 掀翻整个服务（这点与 outbox relay 不同：relay 挂了事件就停摆了）。
	Run func(context.Context) error
	// Logger 可选，默认 slog.Default()。
	Logger *slog.Logger
}

// Every 返回一个长驻任务函数，交给 appkit.Registry.Worker 运行：
// 每 Interval 尝试执行一次 t.Run，同一时刻全集群只有一个副本在执行。
//
// 首次执行等一个（带抖动的）周期而非立即执行：滚动更新时全部副本几乎同时
// 起来，立即执行等于所有副本同时抢同一把锁，抢不到的白跑一趟。
// ctx 取消时返回 ctx.Err()（框架据此判定正常关停）。
// pool 为 nil、Name 为空或 Interval 非正时 panic——都是装配期的编程错误。
func Every(pool *pgxpool.Pool, t Task) func(context.Context) error {
	switch {
	case pool == nil:
		panic("job: pool 不能为 nil")
	case t.Name == "":
		panic("job: Task.Name 不能为空")
	case t.Interval <= 0:
		panic(fmt.Sprintf("job: Task.Interval 必须为正，得到 %v", t.Interval))
	case t.Run == nil:
		panic("job: Task.Run 不能为 nil")
	}
	log := t.Logger
	if log == nil {
		log = slog.Default()
	}

	return func(ctx context.Context) error {
		return loop(ctx, t.Interval, func(ctx context.Context) {
			runOnce(ctx, pool, t, log)
		})
	}
}

// loop 是不含锁与数据库的纯节奏部分：等一轮（首轮带抖动）、执行、再等。
func loop(ctx context.Context, interval time.Duration, tick func(context.Context)) error {
	timer := time.NewTimer(jitter(interval))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		tick(ctx)
		timer.Reset(interval)
	}
}

// runOnce 执行一轮：抢锁、跑、记日志。任何失败都不向上传播——
// 周期任务的正确失败姿势是等下一轮重试。
func runOnce(ctx context.Context, pool *pgxpool.Pool, t Task, log *slog.Logger) {
	start := time.Now()
	err := WithLock(ctx, pool, t.Name, t.Run)
	outcome := metrics.OutcomeError
	switch {
	case errors.Is(err, ErrLocked):
		outcome = metrics.OutcomeSkipped
		log.Debug("job: 本轮由其它副本执行，跳过", "job", t.Name)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// 关停途中，不当作故障，也不计入指标——否则每次滚动更新都拉高错误率。
		return
	case err != nil:
		log.Error("job: 执行失败，等下一轮重试", "job", t.Name,
			"interval", t.Interval, "err", err)
	default:
		outcome = metrics.OutcomeOK
		log.Info("job: 执行完成", "job", t.Name, "duration", time.Since(start))
	}
	// 指标带任务名：任务悄悄停了（曲线归零）比任务报错更难发现。
	metrics.JobRun(ctx, t.Name, outcome, start)
}

// WithLock 在持有 name 的集群锁期间执行 fn。锁被其它副本持有时立即返回
// ErrLocked（不阻塞等待）。fn 的错误原样返回。
//
// 锁是 Postgres 的 session 级 advisory lock，绑定在一条从池里借出的连接上：
// fn 执行期间这条连接不可用于其它查询（fn 内部的查询照常从池里另借），
// 因此 fn 里别做无限期阻塞的事。进程崩溃时连接断开，Postgres 自动放锁。
func WithLock(ctx context.Context, pool *pgxpool.Pool, name string, fn func(context.Context) error) error {
	if name == "" {
		return errors.New("job: 锁名不能为空")
	}
	key := lockKey(name)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("job: 取连接（任务 %q）: %w", name, err)
	}
	defer conn.Release()

	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&got); err != nil {
		return fmt.Errorf("job: 抢锁（任务 %q）: %w", name, err)
	}
	if !got {
		return ErrLocked
	}
	defer func() {
		// 放锁必须用未取消的 ctx：关停时 ctx 已取消，语句发不出去就只能
		// 等连接断开才放锁，下一轮平白多等一次。
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(uctx, "SELECT pg_advisory_unlock($1)", key); err != nil {
			// 放不掉也不致命：连接归还前 Postgres 会随会话结束释放。
			slog.Default().Warn("job: 释放任务锁失败（连接断开时会自动释放）",
				"job", name, "err", err)
		}
	}()

	return fn(ctx)
}

// lockKey 把任务名映射为 advisory lock 的 bigint 键：取 sha256 前 8 字节。
// 在 Go 侧算而不是用 Postgres 的 hashtext()——后者的算法不在兼容性承诺内，
// 跨大版本升级可能变，那会让升级期间新旧副本各持一把"不同"的锁。
func lockKey(name string) int64 {
	sum := sha256.Sum256([]byte(name))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// jitter 返回 [d/2, d) 内的随机时长，打散多副本的首次唤醒。
func jitter(d time.Duration) time.Duration {
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + rand.N(half)
}
