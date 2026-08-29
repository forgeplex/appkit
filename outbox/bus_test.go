package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/outbox"
)

func TestDirectBusPublish(t *testing.T) {
	t.Parallel()
	errBoom := errors.New("boom")
	type sub struct {
		topic string
		err   error
	}
	tests := []struct {
		name      string
		subs      []sub
		topic     string
		wantCalls int
		wantErr   error
	}{
		{name: "无订阅者返回 nil", subs: nil, topic: "t", wantCalls: 0},
		{name: "单 handler 被调用", subs: []sub{{"t", nil}}, topic: "t", wantCalls: 1},
		{name: "同 topic 多 handler 全部调用", subs: []sub{{"t", nil}, {"t", nil}, {"t", nil}}, topic: "t", wantCalls: 3},
		{name: "一个失败其余仍被调用且返回错误", subs: []sub{{"t", errBoom}, {"t", nil}}, topic: "t", wantCalls: 2, wantErr: errBoom},
		{name: "topic 隔离", subs: []sub{{"t", nil}, {"u", nil}}, topic: "t", wantCalls: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bus := outbox.NewDirectBus()
			evt := appkit.Event{ID: "id-1", Topic: tc.topic, Payload: []byte(`{"n":1}`), Meta: map[string]string{"k": "v"}}
			var calls atomic.Int64
			for _, s := range tc.subs {
				subErr := s.err
				bus.Subscribe(s.topic, func(_ context.Context, got appkit.Event) error {
					calls.Add(1)
					if got.ID != evt.ID || got.Topic != evt.Topic {
						t.Errorf("handler 收到 %+v, want %+v", got, evt)
					}
					return subErr
				})
			}
			err := bus.Publish(context.Background(), evt)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("Publish = %v, want nil", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("Publish = %v, want 包含 %v", err, tc.wantErr)
			}
			if got := calls.Load(); got != int64(tc.wantCalls) {
				t.Fatalf("handler 调用次数 = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

// 并发 Subscribe/Publish 交错，靠 -race 检出数据竞争。
func TestDirectBusConcurrent(t *testing.T) {
	t.Parallel()
	bus := outbox.NewDirectBus()
	var handled atomic.Int64
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			topic := fmt.Sprintf("t%d", i%2)
			for range 50 {
				bus.Subscribe(topic, func(context.Context, appkit.Event) error {
					handled.Add(1)
					return nil
				})
				if err := bus.Publish(context.Background(), appkit.Event{ID: "x", Topic: topic}); err != nil {
					t.Errorf("Publish: %v", err)
				}
			}
		})
	}
	wg.Wait()
	if handled.Load() == 0 {
		t.Fatal("并发场景下应有 handler 被调用")
	}
}
