package outbox_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/internal/dbtest"
	"github.com/forgeplex/appkit/outbox"
)

// TestDeadLetterRetry 走完死信闭环的全程：消费方持续失败 → 死信 → List 看到
// 它 → 修好 bug 后 Retry 放回 → 正常投递成功。此前 failed_at 置位后事件
// 没有任何恢复路径，只能手写 SQL 改表——这个测试锁住运维闭环不再退化。
func TestDeadLetterRetry(t *testing.T) {
	pool := dbtest.Pool(t)
	schema := dbtest.Schema(t, pool, "outbox_test", outbox.MigrationSQL)
	ctx := context.Background()
	id := uuid.NewString()
	publishOne(t, pool, schema, appkit.Event{ID: id, Topic: "t.dead", Payload: []byte(`{"k":1}`)})

	var fail atomic.Bool
	fail.Store(true)
	bus := outbox.NewDirectBus()
	bus.Subscribe("t.dead", func(_ context.Context, _ appkit.Event) error {
		if fail.Load() {
			return errors.New("消费方 bug")
		}
		return nil
	})
	relay := outbox.NewRelay(pool, schema, bus,
		outbox.WithInterval(5*time.Millisecond), outbox.WithMaxAttempts(1))
	startRelay(t, relay)

	deadWhere := `SELECT count(*) FROM ` + schema + `.outbox WHERE failed_at IS NOT NULL`
	waitFor(t, 5*time.Second, "一次失败即死信", func() bool {
		return countRows(t, pool, deadWhere) == 1
	})

	// List：死信快照要够排查「为什么死」。
	dl := outbox.NewDeadLetters(pool, schema)
	letters, err := dl.List(ctx, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(letters) != 1 {
		t.Fatalf("死信数 = %d, want 1", len(letters))
	}
	got := letters[0]
	if got.ID != id || got.Topic != "t.dead" {
		t.Fatalf("死信 = %s/%s, want %s/t.dead", got.ID, got.Topic, id)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1（一次失败即达上限）", got.Attempts)
	}
	if !strings.Contains(got.LastError, "消费方 bug") {
		t.Errorf("last_error = %q, want 含失败原因", got.LastError)
	}
	if got.FailedAt.IsZero() {
		t.Error("failed_at 不应为零值")
	}

	// 修好 bug，放回：attempts 归零、立即到期，relay 重新投递并成功。
	fail.Store(false)
	n, err := dl.Retry(ctx, id)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if n != 1 {
		t.Fatalf("Retry 放回 = %d, want 1", n)
	}
	publishedWhere := `SELECT count(*) FROM ` + schema + `.outbox WHERE published_at IS NOT NULL`
	waitFor(t, 5*time.Second, "放回后投递成功", func() bool {
		return countRows(t, pool, publishedWhere) == 1
	})

	// 非死信态的行 Retry 不动：已发布 → 0。
	if n, err := dl.Retry(ctx, id); err != nil || n != 0 {
		t.Fatalf("已发布事件 Retry = %d（err=%v）, want 0", n, err)
	}
	// 合法但不存在的 uuid → 0，不是错误。
	if n, err := dl.Retry(ctx, uuid.NewString()); err != nil || n != 0 {
		t.Fatalf("不存在事件 Retry = %d（err=%v）, want 0", n, err)
	}
	// 非法 uuid → 明确报错（CLI 的用户输入在入口被挡）。
	if _, err := dl.Retry(ctx, "not-a-uuid"); err == nil {
		t.Fatal("非法 uuid 应报错")
	}
}
