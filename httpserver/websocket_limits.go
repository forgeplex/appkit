package httpserver

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/forgeplex/appkit/apperr"
)

// WebSocketRateLimits 为 WebSocket 单方向的应用 data 消息设置速率与突发预算。
type WebSocketRateLimits struct {
	// FramesPerSecond 是每秒允许的应用 data 帧数。
	FramesPerSecond int64
	// FrameBurst 是该方向可立即发送或接收的最大 data 帧数。
	FrameBurst int64
	// BytesPerSecond 是每秒允许的 JSON payload 字节数，不含 wire envelope。
	BytesPerSecond int64
	// ByteBurst 是该方向可立即发送或接收的最大 JSON payload 字节数。
	ByteBurst int64
}

// WebSocketHubLimits 限制单个 Hub 的 pending+active 连接总数，并为每条连接
// 的入站和出站方向分别配置消息预算；它不是进程级或跨副本配额。
type WebSocketHubLimits struct {
	// MaxConnections 限制该 Hub 中 pending reservation 与 active connection 的合计。
	MaxConnections int
	// Inbound 限制客户端发往服务端的应用 data 消息。
	Inbound WebSocketRateLimits
	// Outbound 限制服务端发往客户端的应用 data 消息。
	Outbound WebSocketRateLimits
}

// NewWebSocketHubWithLimits 创建可接纳 WebSocket Upgrade 的 Hub。
// 限额必须由调用方显式提供；框架不会猜测通用容量默认值。
func NewWebSocketHubWithLimits(limits WebSocketHubLimits) (*WebSocketHub, error) {
	if err := validateWebSocketHubLimits(limits); err != nil {
		return nil, err
	}
	hub := NewWebSocketHub()
	hub.limits = limits
	hub.limitsConfigured = true
	return hub, nil
}

func validateWebSocketHubLimits(limits WebSocketHubLimits) error {
	if limits.MaxConnections <= 0 {
		return apperr.InvalidArgument("httpserver WebSocket hub limits: MaxConnections must be positive")
	}
	if err := validateWebSocketRateLimits(limits.Inbound); err != nil {
		return err
	}
	if err := validateWebSocketRateLimits(limits.Outbound); err != nil {
		return err
	}
	return nil
}

func validateWebSocketRateLimits(limits WebSocketRateLimits) error {
	if limits.FramesPerSecond <= 0 || limits.FrameBurst <= 0 || limits.BytesPerSecond <= 0 || limits.ByteBurst <= 0 {
		return apperr.InvalidArgument("httpserver WebSocket rate limits must be positive")
	}
	return nil
}

func (h *WebSocketHub) validateMessageLimit(maxMessageBytes int64) error {
	if h == nil || !h.limitsConfigured {
		return nil // The legacy constructor remains constructible but cannot admit upgrades.
	}
	if maxMessageBytes > h.limits.Inbound.ByteBurst || maxMessageBytes > h.limits.Outbound.ByteBurst {
		return apperr.InvalidArgument("httpserver WebSocket: MaxMessageBytes must fit inbound and outbound byte bursts")
	}
	return nil
}

type webSocketConnectionRateLimiter struct {
	inboundFrames  *webSocketTokenBucket
	inboundBytes   *webSocketTokenBucket
	outboundFrames *webSocketTokenBucket
	outboundBytes  *webSocketTokenBucket
}

func newWebSocketConnectionRateLimiter(limits WebSocketHubLimits) *webSocketConnectionRateLimiter {
	return &webSocketConnectionRateLimiter{
		inboundFrames:  newWebSocketTokenBucket(limits.Inbound.FramesPerSecond, limits.Inbound.FrameBurst),
		inboundBytes:   newWebSocketTokenBucket(limits.Inbound.BytesPerSecond, limits.Inbound.ByteBurst),
		outboundFrames: newWebSocketTokenBucket(limits.Outbound.FramesPerSecond, limits.Outbound.FrameBurst),
		outboundBytes:  newWebSocketTokenBucket(limits.Outbound.BytesPerSecond, limits.Outbound.ByteBurst),
	}
}

func (l *webSocketConnectionRateLimiter) waitInbound(ctx context.Context, idleTimeout time.Duration, payloadBytes int64) error {
	return waitWebSocketRateBudget(ctx, idleTimeout, l.inboundFrames, l.inboundBytes, payloadBytes)
}

func (l *webSocketConnectionRateLimiter) waitOutbound(ctx context.Context, idleTimeout time.Duration, payloadBytes int64) error {
	return waitWebSocketRateBudget(ctx, idleTimeout, l.outboundFrames, l.outboundBytes, payloadBytes)
}

var errWebSocketRateIdle = errors.New("websocket rate wait exceeded idle timeout")

func waitWebSocketRateBudget(
	ctx context.Context,
	idleTimeout time.Duration,
	frameBucket *webSocketTokenBucket,
	byteBucket *webSocketTokenBucket,
	payloadBytes int64,
) error {
	if ctx == nil {
		return apperr.InvalidArgument("httpserver WebSocket rate wait context must not be nil")
	}
	waitCtx := ctx
	cancel := func() {}
	if idleTimeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, idleTimeout)
	}
	defer cancel()
	if err := frameBucket.wait(waitCtx, 1); err != nil {
		return classifyWebSocketRateWaitError(ctx, waitCtx, err, idleTimeout)
	}
	if err := byteBucket.wait(waitCtx, payloadBytes); err != nil {
		return classifyWebSocketRateWaitError(ctx, waitCtx, err, idleTimeout)
	}
	return nil
}

func classifyWebSocketRateWaitError(parent, waitCtx context.Context, err error, idleTimeout time.Duration) error {
	if idleTimeout > 0 && errors.Is(err, context.DeadlineExceeded) && parent.Err() == nil && errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
		return errWebSocketRateIdle
	}
	return err
}

func webSocketRateWaitError(err error) *apperr.Error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errWebSocketRateIdle) {
		return apperr.Unavailable(err)
	}
	return apperr.From(err)
}

type webSocketTokenBucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func newWebSocketTokenBucket(rate, burst int64) *webSocketTokenBucket {
	capacity := float64(burst)
	return &webSocketTokenBucket{
		rate:   float64(rate),
		burst:  capacity,
		tokens: capacity,
		last:   time.Now(),
	}
}

func (b *webSocketTokenBucket) wait(ctx context.Context, count int64) error {
	if ctx == nil {
		return apperr.InvalidArgument("httpserver WebSocket rate wait context must not be nil")
	}
	if count < 0 || float64(count) > b.burst {
		return apperr.InvalidArgument("httpserver WebSocket message exceeds configured rate burst")
	}
	if count == 0 {
		return ctx.Err()
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		now := time.Now()
		b.mu.Lock()
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens = math.Min(b.burst, b.tokens+elapsed*b.rate)
			b.last = now
		}
		if b.tokens >= float64(count) {
			b.tokens -= float64(count)
			b.mu.Unlock()
			return nil
		}
		waitNanos := math.Ceil((float64(count) - b.tokens) / b.rate * float64(time.Second))
		var delay time.Duration
		if waitNanos >= float64(math.MaxInt64) {
			delay = time.Duration(math.MaxInt64)
		} else {
			delay = time.Duration(waitNanos)
		}
		if delay <= 0 {
			delay = time.Nanosecond
		}
		b.mu.Unlock()

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}
