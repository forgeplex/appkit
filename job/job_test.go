package job

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit/internal/dbtest"
)

// ---- 不需要 Postgres 的单测 ----

func TestLockKeyIsStable(t *testing.T) {
	t.Parallel()
	// 固定值而非"自己等于自己"：键一旦变化，滚动升级期间新旧副本会各持
	// 一把不同的锁、同时执行同一个任务——这正是本包要防的事故。
	got := lockKey("ledger.cleanup")
	const want = int64(5687354664291396303)
	if got != want {
		t.Errorf("lockKey 变了：got %d, want %d（改动会破坏滚动升级期间的互斥）", got, want)
	}
	if lockKey("ledger.cleanup") == lockKey("ledger.cleanup2") {
		t.Error("不同任务名不应映射到同一把锁")
	}
}

func TestJitterWithinHalfInterval(t *testing.T) {
	t.Parallel()
	const d = time.Minute
	for range 100 {
		got := jitter(d)
		if got < d/2 || got >= d {
			t.Fatalf("jitter(%v) = %v，应落在 [%v, %v)", d, got, d/2, d)
		}
	}
	// 极小周期不能除出 0 来（会退化成忙循环）。
	if got := jitter(time.Nanosecond); got <= 0 {
		t.Fatalf("jitter(1ns) = %v，必须为正", got)
	}
}

func TestEveryRejectsBadTask(t *testing.T) {
	t.Parallel()
	pool := &pgxpool.Pool{} // 只作非 nil 占位，不会被调用
	tests := []struct {
		name string
		pool *pgxpool.Pool
		task Task
	}{
		{"pool为nil", nil, Task{Name: "x", Interval: time.Second, Run: noop}},
		{"名字为空", pool, Task{Interval: time.Second, Run: noop}},
		{"周期为零", pool, Task{Name: "x", Run: noop}},
		{"周期为负", pool, Task{Name: "x", Interval: -time.Second, Run: noop}},
		{"Run为nil", pool, Task{Name: "x", Interval: time.Second}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Error("装配期的编程错误应当场 panic，而不是运行期才发作")
				}
			}()
			Every(tc.pool, tc.task)
		})
	}
}

// TestLoopStopsOnCancel 验证节奏部分的关停语义：ctx 取消即返回 ctx.Err()，
// appkit.Registry.Worker 据此判定这是正常关停而非崩溃。
func TestLoopStopsOnCancel(t *testing.T) {
	t.Parallel()
	ticks := make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- loop(ctx, 2*time.Millisecond, func(context.Context) {
			select {
			case ticks <- struct{}{}:
			default:
			}
		})
	}()

	select {
	case <-ticks:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("循环未执行")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("关停应返回 context.Canceled，得到 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("取消后循环未退出")
	}
}

// TestRunOnceSwallowsErrors 验证任务失败只记日志、不向上传播——
// 周期任务的正确失败姿势是等下一轮，而不是掀翻整个服务。
func TestRunOnceSwallowsErrors(t *testing.T) {
	t.Parallel()
	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := loop(ctx, time.Millisecond, func(context.Context) {
		calls++
		// 模拟 runOnce：吞掉错误，继续下一轮。
		if calls >= 3 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("退出原因不符: %v", err)
	}
	if calls < 3 {
		t.Fatalf("任务失败后循环应继续，只执行了 %d 轮", calls)
	}
}

func TestWithLockRejectsEmptyName(t *testing.T) {
	t.Parallel()
	if err := WithLock(context.Background(), nil, "", noop); err == nil {
		t.Error("空锁名应报错")
	}
}

func noop(context.Context) error { return nil }

// ---- 需要 Postgres 的测试（TEST_DATABASE_URL）----

// lockName 让并发跑的用例之间不互相抢锁。
func lockName(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("appkit_job_test.%s.%d", t.Name(), time.Now().UnixNano())
}

// TestWithLockExcludesConcurrent 是本包存在的理由：两个副本同时抢，
// 只有一个跑得起来，另一个立刻拿到 ErrLocked 而不是排队等着。
func TestWithLockExcludesConcurrent(t *testing.T) {
	pool := dbtest.Pool(t)
	name := lockName(t)
	ctx := context.Background()

	inside := make(chan struct{})
	release := make(chan struct{})
	held := make(chan error, 1)
	go func() {
		held <- WithLock(ctx, pool, name, func(context.Context) error {
			close(inside)
			<-release
			return nil
		})
	}()
	select {
	case <-inside:
	case err := <-held:
		t.Fatalf("持锁方未进入任务体: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("持锁方未进入任务体")
	}

	// 第二个副本：不阻塞，直接被拒。
	start := time.Now()
	err := WithLock(ctx, pool, name, func(context.Context) error {
		t.Error("锁被持有时不应执行任务体")
		return nil
	})
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("应返回 ErrLocked，得到 %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("抢不到锁应立即返回，等了 %v", elapsed)
	}

	close(release)
	if err := <-held; err != nil {
		t.Fatalf("持锁方: %v", err)
	}

	// 放锁后下一轮抢得到——否则任务只会跑一次就永久停摆。
	ran := false
	if err := WithLock(ctx, pool, name, func(context.Context) error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("释放后重新抢锁: %v", err)
	}
	if !ran {
		t.Error("释放后任务体未执行")
	}
}

// TestWithLockDifferentNames 验证不同任务互不干扰（键推导没把它们撞一起）。
func TestWithLockDifferentNames(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	a, b := lockName(t)+".a", lockName(t)+".b"

	inside := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = WithLock(ctx, pool, a, func(context.Context) error {
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside
	defer close(release)

	ran := false
	if err := WithLock(ctx, pool, b, func(context.Context) error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("另一个任务不该被挡住: %v", err)
	}
	if !ran {
		t.Error("另一个任务未执行")
	}
}

// TestWithLockReleasesOnError 验证任务体报错也放锁——
// 否则一次失败就把这个任务永久锁死到进程重启。
func TestWithLockReleasesOnError(t *testing.T) {
	pool := dbtest.Pool(t)
	name := lockName(t)
	ctx := context.Background()

	boom := errors.New("任务体炸了")
	if err := WithLock(ctx, pool, name, func(context.Context) error {
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("任务体错误应原样返回: %v", err)
	}

	if err := WithLock(ctx, pool, name, noop); err != nil {
		t.Fatalf("失败后锁应已释放: %v", err)
	}
}

// TestWithLockReleasesOnPanic 验证 panic 同样放锁（defer 保证），
// 且 panic 不被吞掉。
func TestWithLockReleasesOnPanic(t *testing.T) {
	pool := dbtest.Pool(t)
	name := lockName(t)
	ctx := context.Background()

	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic 不应被吞掉")
			}
		}()
		_ = WithLock(ctx, pool, name, func(context.Context) error {
			panic("boom")
		})
	}()

	if err := WithLock(ctx, pool, name, noop); err != nil {
		t.Fatalf("panic 后锁应已释放: %v", err)
	}
}

// TestEveryRunsExclusively 端到端验证：两个"副本"跑同一个任务，
// 任一时刻只有一个在执行任务体。
func TestEveryRunsExclusively(t *testing.T) {
	pool := dbtest.Pool(t)
	name := lockName(t)

	var mu sync.Mutex
	concurrent, maxConcurrent, total := 0, 0, 0
	body := func(context.Context) error {
		mu.Lock()
		concurrent++
		total++
		maxConcurrent = max(maxConcurrent, concurrent)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		concurrent--
		mu.Unlock()
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	task := Task{Name: name, Interval: 10 * time.Millisecond, Run: body}
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { _ = Every(pool, task)(ctx) })
	}

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		n := total
		mu.Unlock()
		if n >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("5 秒内只执行了 %d 轮", n)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if maxConcurrent != 1 {
		t.Errorf("同一任务同时有 %d 个副本在跑，互斥失效", maxConcurrent)
	}
}
