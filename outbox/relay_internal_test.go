package outbox

import (
	"context"
	"testing"
	"time"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/callctx"
)

func TestRelayOptions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		opts         []RelayOption
		wantBatch    int
		wantInterval time.Duration
		wantLease    time.Duration
		wantAttempts int
	}{
		{
			name: "默认值", opts: nil,
			wantBatch: DefaultBatchSize, wantInterval: DefaultInterval,
			wantLease: DefaultLease, wantAttempts: DefaultMaxAttempts,
		},
		{
			name: "自定义",
			opts: []RelayOption{
				WithBatchSize(5), WithInterval(10 * time.Millisecond),
				WithLease(time.Second), WithMaxAttempts(3),
			},
			wantBatch: 5, wantInterval: 10 * time.Millisecond,
			wantLease: time.Second, wantAttempts: 3,
		},
		{
			name: "非正值忽略",
			opts: []RelayOption{
				WithBatchSize(0), WithBatchSize(-1),
				WithInterval(0), WithInterval(-time.Second),
				WithLease(0), WithLease(-time.Second),
				WithMaxAttempts(0), WithMaxAttempts(-1),
			},
			wantBatch: DefaultBatchSize, wantInterval: DefaultInterval,
			wantLease: DefaultLease, wantAttempts: DefaultMaxAttempts,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := NewRelay(nil, "s", nil, tc.opts...)
			if r.batch != tc.wantBatch {
				t.Errorf("batch = %d, want %d", r.batch, tc.wantBatch)
			}
			if r.interval != tc.wantInterval {
				t.Errorf("interval = %v, want %v", r.interval, tc.wantInterval)
			}
			if r.lease != tc.wantLease {
				t.Errorf("lease = %v, want %v", r.lease, tc.wantLease)
			}
			if r.maxAttempts != tc.wantAttempts {
				t.Errorf("maxAttempts = %d, want %d", r.maxAttempts, tc.wantAttempts)
			}
		})
	}
}

func TestBackoff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		base     time.Duration
		attempts int
		want     time.Duration
	}{
		{name: "首败即基数", base: time.Second, attempts: 0, want: time.Second},
		{name: "指数翻倍", base: time.Second, attempts: 3, want: 8 * time.Second},
		{name: "封顶五分钟", base: time.Second, attempts: 10, want: maxBackoff},
		{name: "移位溢出封顶", base: time.Hour, attempts: 100, want: maxBackoff},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := backoff(tc.base, tc.attempts); got != tc.want {
				t.Errorf("backoff(%v, %d) = %v, want %v", tc.base, tc.attempts, got, tc.want)
			}
		})
	}
}

// busFunc 把函数适配为 Bus。
type busFunc func(context.Context, appkit.Event) error

func (f busFunc) Publish(ctx context.Context, evt appkit.Event) error { return f(ctx, evt) }

// TestDeliverRestoresCallMeta 验证投递前把事件 meta 里的 callctx 白名单还原进
// ctx：消费者的日志与它再发出的事件由此接回原请求的 request id，异步链路不断。
func TestDeliverRestoresCallMeta(t *testing.T) {
	t.Parallel()
	var got callctx.Meta
	r := NewRelay(nil, "ledger", busFunc(func(ctx context.Context, _ appkit.Event) error {
		got = callctx.From(ctx)
		return nil
	}))
	err := r.deliver(context.Background(), claimedEvent{
		id: "e1", topic: "t", payload: []byte("{}"),
		meta: []byte(`{"appkit.request_id":"r1","appkit.tenant_id":"acme","source":"csv"}`),
	})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	want := callctx.Meta{RequestID: "r1", TenantID: "acme"}
	if got != want {
		t.Fatalf("消费侧拿到的元数据不符: %+v", got)
	}
}
