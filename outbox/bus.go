package outbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/forgeplex/appkit"
)

// Bus 是 relay 与消息基础设施之间的接缝：单体单库用 DirectBus 直投进程内
// 消费者，拆分部署换 NATS/Kafka 适配——同一接口，业务代码不变（DESIGN §8）。
type Bus interface {
	Publish(ctx context.Context, evt appkit.Event) error
}

// DirectBus 是进程内直投 Bus：Publish 同步调用 topic 下的全部 handler。
// 并发安全；零值可用，但含锁，只能以指针传递。
type DirectBus struct {
	mu   sync.RWMutex
	subs map[string][]appkit.EventHandler
}

var _ Bus = (*DirectBus)(nil)

// NewDirectBus 构造 DirectBus。
func NewDirectBus() *DirectBus { return &DirectBus{} }

// Subscribe 追加 topic 的一个 handler。handler 必须幂等——通常用 Inbox 包装
// （部分 handler 失败导致 relay 重投时，已成功的 handler 靠 inbox 去重跳过）。
func (b *DirectBus) Subscribe(topic string, h appkit.EventHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs == nil {
		b.subs = make(map[string][]appkit.EventHandler)
	}
	b.subs[topic] = append(b.subs[topic], h)
}

// Publish 逐个调用 evt.Topic 下的全部 handler。任一失败即返回（聚合）错误，
// relay 因此不标记 published_at、稍后整体重投；失败者之外的 handler 本轮
// 仍会被调用，避免一个坏消费者饿死其他消费者。无订阅者时静默返回 nil。
func (b *DirectBus) Publish(ctx context.Context, evt appkit.Event) error {
	b.mu.RLock()
	handlers := slices.Clone(b.subs[evt.Topic])
	b.mu.RUnlock()

	var errs []error
	for _, h := range handlers {
		if err := h(ctx, evt); err != nil {
			errs = append(errs, fmt.Errorf("outbox: topic %q 的 handler 处理事件 %s 失败: %w", evt.Topic, evt.ID, err))
		}
	}
	return errors.Join(errs...)
}
