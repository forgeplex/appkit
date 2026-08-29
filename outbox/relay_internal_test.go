package outbox

import (
	"testing"
	"time"
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
