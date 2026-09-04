package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/internal/dbtest"
	"github.com/forgeplex/appkit/outbox"
	"github.com/forgeplex/appkit/pgtx"
	"github.com/forgeplex/appkit/tx"
)

// ---- 需要 Postgres 的集成测试（TEST_DATABASE_URL）----

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("查询 %q: %v", sql, err)
	}
	return n
}

// publishOne 在独立事务里发布单个事件；多次调用之间 created_at 严格递增，
// 便于构造确定的队列顺序。
func publishOne(t *testing.T, pool *pgxpool.Pool, schema string, evt appkit.Event) {
	t.Helper()
	err := pgtx.New(pool).Do(context.Background(), func(ctx context.Context) error {
		return outbox.Publish(ctx, pgtx.From(ctx, pool), schema, evt)
	})
	if err != nil {
		t.Fatalf("发布事件 %s: %v", evt.ID, err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

func TestPublishIntegration(t *testing.T) {
	pool := dbtest.Pool(t)
	tr := pgtx.New(pool)
	errBoom := errors.New("boom")

	tests := []struct {
		name     string
		evt      appkit.Event
		fnErr    error
		wantRows int
	}{
		{
			name: "提交后落表",
			evt: appkit.Event{
				ID:      uuid.NewString(),
				Topic:   "ledger.entry_posted",
				Payload: []byte(`{"amount":"10.00"}`),
				Meta:    map[string]string{"trace": "abc"},
			},
			wantRows: 1,
		},
		{
			name:     "空 ID 空 payload 自动补全",
			evt:      appkit.Event{Topic: "ledger.entry_posted"},
			wantRows: 1,
		},
		{
			name:     "业务回滚事件不落表",
			evt:      appkit.Event{Topic: "ledger.entry_posted", Payload: []byte(`{}`)},
			fnErr:    errBoom,
			wantRows: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			schema := dbtest.Schema(t, pool, "outbox_test", outbox.MigrationSQL)
			err := tr.Do(context.Background(), func(ctx context.Context) error {
				if err := outbox.Publish(ctx, pgtx.From(ctx, pool), schema, tc.evt); err != nil {
					return err
				}
				return tc.fnErr
			})
			if !errors.Is(err, tc.fnErr) {
				t.Fatalf("Do = %v, want %v", err, tc.fnErr)
			}
			got := countRows(t, pool, `SELECT count(*) FROM `+schema+`.outbox`)
			if got != tc.wantRows {
				t.Fatalf("outbox 行数 = %d, want %d", got, tc.wantRows)
			}
			if tc.wantRows == 0 {
				return
			}
			var (
				id, topic string
				published *time.Time
			)
			row := pool.QueryRow(context.Background(), `SELECT id, topic, published_at FROM `+schema+`.outbox`)
			if err := row.Scan(&id, &topic, &published); err != nil {
				t.Fatalf("读回事件: %v", err)
			}
			if _, err := uuid.Parse(id); err != nil {
				t.Errorf("id %q 不是 uuid: %v", id, err)
			}
			if tc.evt.ID != "" && id != tc.evt.ID {
				t.Errorf("id = %q, want %q", id, tc.evt.ID)
			}
			if topic != tc.evt.Topic {
				t.Errorf("topic = %q, want %q", topic, tc.evt.Topic)
			}
			if published != nil {
				t.Errorf("新事件的 published_at 应为 NULL，得到 %v", published)
			}
		})
	}
}

// startRelay 起 relay goroutine，Cleanup 时取消并断言正常退出（返回 nil）。
func startRelay(t *testing.T, relay *outbox.Relay) {
	t.Helper()
	rctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- relay.Run(rctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run 退出应返回 nil，得到 %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("relay 在 ctx 取消后未退出")
		}
	})
}

func TestRelayIntegration(t *testing.T) {
	pool := dbtest.Pool(t)
	tr := pgtx.New(pool)

	tests := []struct {
		name      string
		failFirst bool
		wantCalls int64 // handler 总调用次数下限
	}{
		{name: "直接投递成功", failFirst: false, wantCalls: 2},
		{name: "首投失败后重投成功", failFirst: true, wantCalls: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			schema := dbtest.Schema(t, pool, "outbox_test", outbox.MigrationSQL)
			const topic = "ledger.entry_posted"

			wantPayload := map[string]map[string]any{}
			var evts []appkit.Event
			for i := range 2 {
				id := uuid.NewString()
				evts = append(evts, appkit.Event{
					ID:      id,
					Topic:   topic,
					Payload: fmt.Appendf(nil, `{"n":%d}`, i),
					Meta:    map[string]string{"seq": fmt.Sprint(i)},
				})
				wantPayload[id] = map[string]any{"n": float64(i)}
			}
			err := tr.Do(context.Background(), func(ctx context.Context) error {
				for _, evt := range evts {
					if err := outbox.Publish(ctx, pgtx.From(ctx, pool), schema, evt); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Fatalf("发布: %v", err)
			}

			var (
				calls atomic.Int64
				mu    sync.Mutex
				got   = map[string]appkit.Event{}
			)
			bus := outbox.NewDirectBus()
			bus.Subscribe(topic, func(_ context.Context, evt appkit.Event) error {
				if calls.Add(1) == 1 && tc.failFirst {
					return errors.New("transient")
				}
				mu.Lock()
				got[evt.ID] = evt
				mu.Unlock()
				return nil
			})

			relay := outbox.NewRelay(pool, schema, bus,
				outbox.WithInterval(10*time.Millisecond), outbox.WithBatchSize(10))
			startRelay(t, relay)

			waitFor(t, 5*time.Second, "两条事件都被投递", func() bool {
				mu.Lock()
				defer mu.Unlock()
				return len(got) == 2
			})
			waitFor(t, 5*time.Second, "published_at 全部置位", func() bool {
				return countRows(t, pool, `SELECT count(*) FROM `+schema+`.outbox WHERE published_at IS NULL`) == 0
			})

			mu.Lock()
			defer mu.Unlock()
			for _, want := range evts {
				g, ok := got[want.ID]
				if !ok {
					t.Fatalf("事件 %s 未投递", want.ID)
				}
				if g.Topic != want.Topic {
					t.Errorf("topic = %q, want %q", g.Topic, want.Topic)
				}
				// jsonb 会规范化 JSON 文本，按语义比较。
				var p map[string]any
				if err := json.Unmarshal(g.Payload, &p); err != nil {
					t.Fatalf("payload 反序列化: %v", err)
				}
				if fmt.Sprint(p) != fmt.Sprint(wantPayload[want.ID]) {
					t.Errorf("payload = %v, want %v", p, wantPayload[want.ID])
				}
				if g.Meta["seq"] != want.Meta["seq"] {
					t.Errorf("meta = %v, want %v", g.Meta, want.Meta)
				}
			}
			if n := calls.Load(); n < tc.wantCalls {
				t.Errorf("handler 调用次数 = %d, want >= %d", n, tc.wantCalls)
			}
		})
	}
}

func TestRelayNoSubscriberIsNotPublished(t *testing.T) {
	pool := dbtest.Pool(t)
	schema := dbtest.Schema(t, pool, "outbox_test", outbox.MigrationSQL)
	evt := appkit.Event{ID: uuid.NewString(), Topic: "ledger.unhandled"}
	publishOne(t, pool, schema, evt)

	relay := outbox.NewRelay(pool, schema, outbox.NewDirectBus(),
		outbox.WithInterval(10*time.Millisecond), outbox.WithMaxAttempts(2))
	startRelay(t, relay)

	waitFor(t, 5*time.Second, "无订阅事件进入死信", func() bool {
		return countRows(t, pool,
			`SELECT count(*) FROM `+schema+`.outbox WHERE id = $1 AND failed_at IS NOT NULL`, evt.ID) == 1
	})
	var (
		publishedAt *time.Time
		attempts    int
		lastError   string
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT published_at, attempts, last_error FROM `+schema+`.outbox WHERE id = $1`, evt.ID).
		Scan(&publishedAt, &attempts, &lastError); err != nil {
		t.Fatalf("读取无订阅事件状态: %v", err)
	}
	if publishedAt != nil {
		t.Fatalf("无订阅事件不应标记 published_at，实际 %v", publishedAt)
	}
	if attempts != 2 || !strings.Contains(lastError, outbox.ErrNoSubscriber.Error()) {
		t.Fatalf("失败状态不符: attempts=%d last_error=%q", attempts, lastError)
	}
}

type durableAckFailBus struct{ err error }

func (b durableAckFailBus) Publish(context.Context, appkit.Event) error { return b.err }

func TestRelayDurableAckFailureIsNotPublished(t *testing.T) {
	pool := dbtest.Pool(t)
	schema := dbtest.Schema(t, pool, "outbox_test", outbox.MigrationSQL)
	evt := appkit.Event{ID: uuid.NewString(), Topic: "payment.created"}
	publishOne(t, pool, schema, evt)

	ackErr := errors.New("broker durable ack timeout")
	relay := outbox.NewRelay(pool, schema, durableAckFailBus{err: ackErr},
		outbox.WithInterval(10*time.Millisecond), outbox.WithMaxAttempts(1))
	startRelay(t, relay)
	waitFor(t, 5*time.Second, "durable ack 失败进入死信", func() bool {
		return countRows(t, pool,
			`SELECT count(*) FROM `+schema+`.outbox WHERE id = $1 AND failed_at IS NOT NULL`, evt.ID) == 1
	})
	if countRows(t, pool,
		`SELECT count(*) FROM `+schema+`.outbox WHERE id = $1 AND published_at IS NOT NULL`, evt.ID) != 0 {
		t.Fatal("durable ack 失败不应标记 published_at")
	}
}

func TestInboxIntegration(t *testing.T) {
	pool := dbtest.Pool(t)
	errBoom := errors.New("boom")

	tests := []struct {
		name      string
		failFirst bool
		wantCalls int64
	}{
		{name: "重复投递被去重", failFirst: false, wantCalls: 1},
		{name: "失败回滚后重投重试成功", failFirst: true, wantCalls: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			schema := dbtest.Schema(t, pool, "outbox_test", outbox.MigrationSQL)
			ctx := context.Background()
			// 业务侧表：验证 next 的写与 inbox 记录同事务提交/回滚。
			if _, err := pool.Exec(ctx, `CREATE TABLE `+schema+`.side (v text NOT NULL)`); err != nil {
				t.Fatalf("建业务表: %v", err)
			}

			var calls atomic.Int64
			next := func(hctx context.Context, evt appkit.Event) error {
				if !tx.HasTx(hctx) {
					return errors.New("next 内应处于事务中")
				}
				if _, err := pgtx.From(hctx, pool).Exec(hctx,
					`INSERT INTO `+schema+`.side (v) VALUES ($1)`, evt.ID); err != nil {
					return err
				}
				if calls.Add(1) == 1 && tc.failFirst {
					return errBoom
				}
				return nil
			}
			h := outbox.Inbox(pool, schema, "ledger", next)
			evt := appkit.Event{ID: uuid.NewString(), Topic: "ledger.entry_posted"}

			err1 := h(ctx, evt)
			if tc.failFirst {
				if !errors.Is(err1, errBoom) {
					t.Fatalf("首投应失败于 %v, 得到 %v", errBoom, err1)
				}
				// 同事务回滚：inbox 记录与业务写都不可见。
				if n := countRows(t, pool, `SELECT count(*) FROM `+schema+`.inbox`); n != 0 {
					t.Fatalf("失败后 inbox 行数 = %d, want 0", n)
				}
				if n := countRows(t, pool, `SELECT count(*) FROM `+schema+`.side`); n != 0 {
					t.Fatalf("失败后业务行数 = %d, want 0", n)
				}
			} else if err1 != nil {
				t.Fatalf("首投: %v", err1)
			}

			for range 2 { // 重投一次 + 再次重复投递
				if err := h(ctx, evt); err != nil {
					t.Fatalf("重投: %v", err)
				}
			}
			if n := calls.Load(); n != tc.wantCalls {
				t.Errorf("next 调用次数 = %d, want %d", n, tc.wantCalls)
			}
			if n := countRows(t, pool, `SELECT count(*) FROM `+schema+`.inbox WHERE consumer = $1 AND event_id = $2 AND topic = $3`, "ledger", evt.ID, evt.Topic); n != 1 {
				t.Errorf("inbox 行数 = %d, want 1", n)
			}
			if n := countRows(t, pool, `SELECT count(*) FROM `+schema+`.side`); n != 1 {
				t.Errorf("业务行数 = %d, want 1", n)
			}
		})
	}
}

// 端到端：事务发布 → relay → DirectBus → Inbox 去重消费。
func TestOutboxEndToEnd(t *testing.T) {
	pool := dbtest.Pool(t)
	tr := pgtx.New(pool)
	schema := dbtest.Schema(t, pool, "outbox_test", outbox.MigrationSQL)
	const topic = "ledger.entry_posted"

	var calls atomic.Int64
	bus := outbox.NewDirectBus()
	bus.Subscribe(topic, outbox.Inbox(pool, schema, "ledger", func(context.Context, appkit.Event) error {
		calls.Add(1)
		return nil
	}))
	relay := outbox.NewRelay(pool, schema, bus, outbox.WithInterval(10*time.Millisecond))
	startRelay(t, relay)

	evt := appkit.Event{ID: uuid.NewString(), Topic: topic, Payload: []byte(`{"n":1}`)}
	err := tr.Do(context.Background(), func(ctx context.Context) error {
		return outbox.Publish(ctx, pgtx.From(ctx, pool), schema, evt)
	})
	if err != nil {
		t.Fatalf("发布: %v", err)
	}

	waitFor(t, 5*time.Second, "事件被消费并标记", func() bool {
		return calls.Load() == 1 &&
			countRows(t, pool, `SELECT count(*) FROM `+schema+`.outbox WHERE published_at IS NULL`) == 0
	})
	if n := countRows(t, pool, `SELECT count(*) FROM `+schema+`.inbox WHERE event_id = $1`, evt.ID); n != 1 {
		t.Fatalf("inbox 行数 = %d, want 1", n)
	}
}

// 旧架构 relayOnce 开着事务（占住连接）同步调 bus.Publish，Inbox 消费者再向
// 同池 Begin 即 hold-and-wait 死锁——MaxConns=1 时必现。claim/lease 两段式
// 在投递期间不持连接，端到端必须正常完成。
func TestRelaySingleConnPoolNoDeadlock(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过需要 Postgres 的测试")
	}
	pool, err := pgtx.NewPool(context.Background(), dsn, pgtx.WithMaxConns(1))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	schema := dbtest.Schema(t, pool, "outbox_test", outbox.MigrationSQL)
	const topic = "ledger.entry_posted"

	var calls atomic.Int64
	bus := outbox.NewDirectBus()
	bus.Subscribe(topic, outbox.Inbox(pool, schema, "ledger", func(context.Context, appkit.Event) error {
		calls.Add(1)
		return nil
	}))
	relay := outbox.NewRelay(pool, schema, bus, outbox.WithInterval(10*time.Millisecond))
	startRelay(t, relay)

	evt := appkit.Event{ID: uuid.NewString(), Topic: topic, Payload: []byte(`{"n":1}`)}
	publishOne(t, pool, schema, evt)

	waitFor(t, 5*time.Second, "MaxConns=1 池上事件被端到端消费（旧架构此处死锁）", func() bool {
		return calls.Load() == 1 &&
			countRows(t, pool, `SELECT count(*) FROM `+schema+`.outbox WHERE published_at IS NULL`) == 0
	})
	if n := countRows(t, pool, `SELECT count(*) FROM `+schema+`.inbox WHERE event_id = $1`, evt.ID); n != 1 {
		t.Fatalf("inbox 行数 = %d, want 1", n)
	}
}

// 队头毒消息：持续失败的事件按退避让位、attempts 递增、达上限进死信，
// 不阻塞后续事件，死信之后 relay 继续正常工作。
func TestRelayPoisonMessageBackoffAndDeadLetter(t *testing.T) {
	pool := dbtest.Pool(t)
	schema := dbtest.Schema(t, pool, "outbox_test", outbox.MigrationSQL)
	const topic = "ledger.entry_posted"

	poison := appkit.Event{ID: uuid.NewString(), Topic: topic, Payload: []byte(`{"poison":true}`)}
	good := appkit.Event{ID: uuid.NewString(), Topic: topic, Payload: []byte(`{"n":1}`)}
	publishOne(t, pool, schema, poison) // 先落表，占据队头
	publishOne(t, pool, schema, good)

	var (
		poisonCalls atomic.Int64
		mu          sync.Mutex
		got         []string
	)
	bus := outbox.NewDirectBus()
	bus.Subscribe(topic, func(_ context.Context, evt appkit.Event) error {
		if evt.ID == poison.ID {
			poisonCalls.Add(1)
			return errors.New("poison boom")
		}
		mu.Lock()
		got = append(got, evt.ID)
		mu.Unlock()
		return nil
	})
	relay := outbox.NewRelay(pool, schema, bus,
		outbox.WithInterval(10*time.Millisecond), outbox.WithMaxAttempts(3))
	startRelay(t, relay)

	// 毒消息不再永久阻塞队头：其后的事件正常投递。
	waitFor(t, 5*time.Second, "毒消息之后的事件被投递", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(got, good.ID)
	})
	// 失败达上限后进入死信：failed_at 置位、attempts 与 last_error 记录在案。
	waitFor(t, 5*time.Second, "毒消息进入死信", func() bool {
		return countRows(t, pool,
			`SELECT count(*) FROM `+schema+`.outbox WHERE id = $1 AND failed_at IS NOT NULL`, poison.ID) == 1
	})
	var (
		attempts  int
		lastError string
	)
	err := pool.QueryRow(context.Background(),
		`SELECT attempts, last_error FROM `+schema+`.outbox WHERE id = $1`, poison.ID).Scan(&attempts, &lastError)
	if err != nil {
		t.Fatalf("读毒消息状态: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	if !strings.Contains(lastError, "poison boom") {
		t.Errorf("last_error = %q, 应包含底层错误", lastError)
	}
	if n := poisonCalls.Load(); n != 3 {
		t.Errorf("毒消息投递次数 = %d, want 3", n)
	}
	if countRows(t, pool,
		`SELECT count(*) FROM `+schema+`.outbox WHERE id = $1 AND published_at IS NOT NULL`, poison.ID) != 0 {
		t.Error("毒消息不应被标记 published_at")
	}

	// 死信移出热路径后，relay 对新事件照常投递。
	after := appkit.Event{ID: uuid.NewString(), Topic: topic, Payload: []byte(`{"n":2}`)}
	publishOne(t, pool, schema, after)
	waitFor(t, 5*time.Second, "死信之后的新事件仍被投递", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(got, after.ID)
	})
}

// handler panic 必须被恢复并按失败退避重试：消费路径的 panic 不能崩掉进程——
// 崩溃重启后按序重取同一事件只会形成崩溃循环。startRelay 的 Cleanup 断言
// Run 正常返回，证明 relay 未被 panic 打死。
func TestRelayHandlerPanicRecovered(t *testing.T) {
	pool := dbtest.Pool(t)
	schema := dbtest.Schema(t, pool, "outbox_test", outbox.MigrationSQL)
	const topic = "ledger.entry_posted"

	var calls atomic.Int64
	bus := outbox.NewDirectBus()
	bus.Subscribe(topic, func(context.Context, appkit.Event) error {
		if calls.Add(1) == 1 {
			panic("kaboom")
		}
		return nil
	})
	relay := outbox.NewRelay(pool, schema, bus, outbox.WithInterval(10*time.Millisecond))
	startRelay(t, relay)

	evt := appkit.Event{ID: uuid.NewString(), Topic: topic, Payload: []byte(`{"n":1}`)}
	publishOne(t, pool, schema, evt)

	waitFor(t, 5*time.Second, "panic 后事件经退避重试成功", func() bool {
		return countRows(t, pool,
			`SELECT count(*) FROM `+schema+`.outbox WHERE id = $1 AND published_at IS NOT NULL`, evt.ID) == 1
	})
	if n := calls.Load(); n != 2 {
		t.Errorf("handler 调用次数 = %d, want 2（panic 一次 + 重试成功一次）", n)
	}
	var (
		attempts  int
		lastError string
	)
	err := pool.QueryRow(context.Background(),
		`SELECT attempts, last_error FROM `+schema+`.outbox WHERE id = $1`, evt.ID).Scan(&attempts, &lastError)
	if err != nil {
		t.Fatalf("读事件状态: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if !strings.Contains(lastError, "kaboom") {
		t.Errorf("last_error = %q, 应包含 panic 信息", lastError)
	}
}

// 去重键是 (consumer, event_id)：同 topic 的两个消费者各自处理一次；
// 旧架构（event_id 单键）第二个消费者会被静默跳过。重复投递时各自去重。
func TestInboxMultiConsumers(t *testing.T) {
	pool := dbtest.Pool(t)
	schema := dbtest.Schema(t, pool, "outbox_test", outbox.MigrationSQL)
	const topic = "ledger.entry_posted"

	var emailCalls, auditCalls atomic.Int64
	bus := outbox.NewDirectBus()
	bus.Subscribe(topic, outbox.Inbox(pool, schema, "email", func(context.Context, appkit.Event) error {
		emailCalls.Add(1)
		return nil
	}))
	bus.Subscribe(topic, outbox.Inbox(pool, schema, "audit", func(context.Context, appkit.Event) error {
		auditCalls.Add(1)
		return nil
	}))

	evt := appkit.Event{ID: uuid.NewString(), Topic: topic}
	// 投递两次：第一次两个消费者都处理，第二次都被去重跳过。
	for range 2 {
		if err := bus.Publish(context.Background(), evt); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	if n := emailCalls.Load(); n != 1 {
		t.Errorf("email 消费次数 = %d, want 1", n)
	}
	if n := auditCalls.Load(); n != 1 {
		t.Errorf("audit 消费次数 = %d, want 1", n)
	}
	for _, consumer := range []string{"email", "audit"} {
		if n := countRows(t, pool,
			`SELECT count(*) FROM `+schema+`.inbox WHERE consumer = $1 AND event_id = $2`, consumer, evt.ID); n != 1 {
			t.Errorf("consumer %s 的 inbox 行数 = %d, want 1", consumer, n)
		}
	}
}

// 模拟「claim 后崩溃」：relay1 的 handler 卡死（事件被 claim、永远不会收尾），
// 短租约到期后 relay2 必须接管重投；且接管不得早于租约到期。
func TestRelayLeaseTakeover(t *testing.T) {
	pool := dbtest.Pool(t)
	schema := dbtest.Schema(t, pool, "outbox_test", outbox.MigrationSQL)
	const topic = "ledger.entry_posted"

	block := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(block) }) }

	stuckBus := outbox.NewDirectBus()
	stuckBus.Subscribe(topic, func(context.Context, appkit.Event) error {
		<-block
		// 解除阻塞只发生在测试收尾：返回错误即可，避免与 relay2 的成功标记混淆。
		return errors.New("stuck handler released")
	})
	relay1 := outbox.NewRelay(pool, schema, stuckBus,
		outbox.WithInterval(10*time.Millisecond), outbox.WithLease(300*time.Millisecond))
	startRelay(t, relay1)
	// 注册在 startRelay 之后：Cleanup 按 LIFO 先解除阻塞，relay1 才能退出。
	t.Cleanup(unblock)

	evt := appkit.Event{ID: uuid.NewString(), Topic: topic}
	publishOne(t, pool, schema, evt)

	// 等 relay1 claim 到事件，并记下租约到期时刻（数据库时钟）。
	var claimedUntil time.Time
	waitFor(t, 5*time.Second, "relay1 claim 事件", func() bool {
		return pool.QueryRow(context.Background(),
			`SELECT claimed_until FROM `+schema+`.outbox WHERE id = $1 AND claimed_until IS NOT NULL`,
			evt.ID).Scan(&claimedUntil) == nil
	})

	// relay2 扮演接管副本。
	var calls atomic.Int64
	takeBus := outbox.NewDirectBus()
	takeBus.Subscribe(topic, func(context.Context, appkit.Event) error {
		calls.Add(1)
		return nil
	})
	relay2 := outbox.NewRelay(pool, schema, takeBus, outbox.WithInterval(10*time.Millisecond))
	startRelay(t, relay2)

	waitFor(t, 5*time.Second, "租约到期后 relay2 接管投递", func() bool {
		return calls.Load() == 1 &&
			countRows(t, pool,
				`SELECT count(*) FROM `+schema+`.outbox WHERE id = $1 AND published_at IS NOT NULL`, evt.ID) == 1
	})

	// 两个时刻都取数据库时钟，比较不受本地时钟偏差影响。
	var publishedAt time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT published_at FROM `+schema+`.outbox WHERE id = $1`, evt.ID).Scan(&publishedAt); err != nil {
		t.Fatalf("读 published_at: %v", err)
	}
	if publishedAt.Before(claimedUntil) {
		t.Errorf("接管早于租约到期：published_at = %v, claimed_until = %v", publishedAt, claimedUntil)
	}
}

// Publisher：业务层只依赖单方法接口即可发事件；事务外被守卫拦下，
// 事务内随业务事务一起提交/回滚。
func TestPublisherIntegration(t *testing.T) {
	pool := dbtest.Pool(t)
	tr := pgtx.New(pool)
	schema := dbtest.Schema(t, pool, "outbox_test", outbox.MigrationSQL)
	pub := outbox.NewPublisher(pool, schema)
	errBoom := errors.New("boom")

	// 事务外：TX_BOUNDARY 守卫，不落表。
	err := pub.Publish(context.Background(), appkit.Event{Topic: "ledger.entry_posted"})
	if !apperr.Is(err, apperr.CodeTxBoundary) {
		t.Fatalf("事务外 Publish 应返回 TX_BOUNDARY，得到 %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM `+schema+`.outbox`); n != 0 {
		t.Fatalf("事务外 Publish 后 outbox 行数 = %d, want 0", n)
	}

	// 事务内：随业务事务提交落表。
	evt := appkit.Event{ID: uuid.NewString(), Topic: "ledger.entry_posted", Payload: []byte(`{"n":1}`)}
	if err := tr.Do(context.Background(), func(ctx context.Context) error {
		return pub.Publish(ctx, evt)
	}); err != nil {
		t.Fatalf("事务内 Publish: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM `+schema+`.outbox WHERE id = $1`, evt.ID); n != 1 {
		t.Fatalf("事务内 Publish 后 outbox 行数 = %d, want 1", n)
	}

	// 业务回滚时事件一并回滚。
	err = tr.Do(context.Background(), func(ctx context.Context) error {
		if err := pub.Publish(ctx, appkit.Event{Topic: "ledger.entry_posted"}); err != nil {
			return err
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("Do = %v, want %v", err, errBoom)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM `+schema+`.outbox`); n != 1 {
		t.Fatalf("回滚后 outbox 行数 = %d, want 1", n)
	}
}
