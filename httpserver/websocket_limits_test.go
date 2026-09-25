package httpserver

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/forgeplex/appkit/apperr"
)

func testWebSocketHubLimits(maxConnections int) WebSocketHubLimits {
	rate := WebSocketRateLimits{
		FramesPerSecond: 10,
		FrameBurst:      2,
		BytesPerSecond:  1024,
		ByteBurst:       256,
	}
	return WebSocketHubLimits{MaxConnections: maxConnections, Inbound: rate, Outbound: rate}
}

func TestGuideWebSocketExampleUsesConfiguredHubLimits(t *testing.T) {
	guide, err := os.ReadFile("../docs/GUIDE.md")
	if err != nil {
		t.Fatal(err)
	}
	guideText := string(guide)
	sectionStart := strings.Index(guideText, "### 远程双向 Stream：WSS")
	if sectionStart < 0 {
		t.Fatal("GUIDE.md is missing the remote bidirectional WebSocket section")
	}
	section := guideText[sectionStart:]
	codeStart := strings.Index(section, "```go\n")
	if codeStart < 0 {
		t.Fatal("GUIDE.md WebSocket section is missing its Go example")
	}
	codeStart += len("```go\n")
	codeEnd := strings.Index(section[codeStart:], "\n```")
	if codeEnd < 0 {
		t.Fatal("GUIDE.md WebSocket Go example is not closed")
	}
	example := section[codeStart : codeStart+codeEnd]
	for _, want := range []string{
		"NewWebSocketHubWithLimits(",
		"MaxConnections:",
		"Inbound: httpserver.WebSocketRateLimits{",
		"Outbound: httpserver.WebSocketRateLimits{",
		"FramesPerSecond:",
		"FrameBurst:",
		"BytesPerSecond:",
		"ByteBurst:",
		"MaxMessageBytes: 1 << 20",
		"if err != nil",
	} {
		if !strings.Contains(example, want) {
			t.Errorf("GUIDE.md WebSocket example is missing %q", want)
		}
	}
	if strings.Contains(example, "NewWebSocketHub()") {
		t.Error("GUIDE.md WebSocket example must not construct an unbounded Hub")
	}
	byteBurstCount := 0
	for _, line := range strings.Split(example, "\n") {
		if strings.Contains(line, "ByteBurst:") && strings.Contains(line, "1 << 20") {
			byteBurstCount++
		}
	}
	if byteBurstCount != 2 {
		t.Errorf("GUIDE.md WebSocket example has %d 1 MiB byte bursts, want one for each direction", byteBurstCount)
	}
}

func TestNewWebSocketHubWithLimitsRejectsUnboundedOrInvalidBudgets(t *testing.T) {
	valid := testWebSocketHubLimits(2)
	cases := []struct {
		name   string
		mutate func(*WebSocketHubLimits)
	}{
		{name: "connections", mutate: func(l *WebSocketHubLimits) { l.MaxConnections = 0 }},
		{name: "inbound rate", mutate: func(l *WebSocketHubLimits) { l.Inbound.FramesPerSecond = 0 }},
		{name: "inbound burst", mutate: func(l *WebSocketHubLimits) { l.Inbound.ByteBurst = 0 }},
		{name: "outbound rate", mutate: func(l *WebSocketHubLimits) { l.Outbound.BytesPerSecond = -1 }},
		{name: "outbound burst", mutate: func(l *WebSocketHubLimits) { l.Outbound.FrameBurst = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limits := valid
			tc.mutate(&limits)
			hub, err := NewWebSocketHubWithLimits(limits)
			if hub != nil || !apperr.Is(err, apperr.CodeInvalidArgument) {
				t.Fatalf("NewWebSocketHubWithLimits() = (%v, %v), want nil hub and INVALID_ARGUMENT", hub, err)
			}
		})
	}

	legacy := NewWebSocketHub()
	if err := legacy.Start(context.Background()); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("unbounded legacy Hub.Start() = %v, want INVALID_ARGUMENT", err)
	}
	if err := legacy.Ready(context.Background()); !apperr.Is(err, apperr.CodeUnavailable) {
		t.Fatalf("unbounded legacy Hub.Ready() = %v, want UNAVAILABLE", err)
	}
}

func TestWebSocketHubCountsPendingAndActiveReservationsTogether(t *testing.T) {
	hub, err := NewWebSocketHubWithLimits(testWebSocketHubLimits(2))
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	pending, err := hub.reserve()
	if err != nil {
		t.Fatal(err)
	}
	activeReservation, err := hub.reserve()
	if err != nil {
		t.Fatal(err)
	}
	active := &managedWebSocket{}
	if !activeReservation.attach(active) {
		t.Fatal("reservation did not attach")
	}
	if _, err := hub.reserve(); !apperr.Is(err, apperr.CodeUnavailable) {
		t.Fatalf("reservation with one pending and one active = %v, want UNAVAILABLE", err)
	}

	pending.release()
	available, err := hub.reserve()
	if err != nil {
		t.Fatalf("reservation after pending release: %v", err)
	}
	available.release()
	hub.remove(active)
	if _, err := hub.reserve(); err != nil {
		t.Fatalf("reservation after active release: %v", err)
	}
}

func TestWebSocketHubConcurrentReservationsRespectCapacity(t *testing.T) {
	const capacity = 3
	hub, err := NewWebSocketHubWithLimits(testWebSocketHubLimits(capacity))
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	const callers = 32
	reservations := make(chan *webSocketReservation, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reservation, err := hub.reserve()
			if err == nil {
				reservations <- reservation
			} else if !apperr.Is(err, apperr.CodeUnavailable) {
				t.Errorf("reserve() error = %v, want UNAVAILABLE or success", err)
			}
		}()
	}
	wg.Wait()
	close(reservations)

	var accepted []*webSocketReservation
	for reservation := range reservations {
		accepted = append(accepted, reservation)
	}
	if len(accepted) != capacity {
		t.Fatalf("accepted reservations = %d, want exactly %d", len(accepted), capacity)
	}
	for _, reservation := range accepted {
		reservation.release()
	}
}

func TestWebSocketRateBudgetWaitIsCancellableAndIdleBounded(t *testing.T) {
	rate := WebSocketRateLimits{FramesPerSecond: 1, FrameBurst: 1, BytesPerSecond: 100, ByteBurst: 100}
	limiter := newWebSocketConnectionRateLimiter(WebSocketHubLimits{Inbound: rate, Outbound: rate})
	if err := limiter.waitInbound(context.Background(), 0, 10); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := limiter.waitInbound(context.Background(), 25*time.Millisecond, 10); !errors.Is(err, errWebSocketRateIdle) {
		t.Fatalf("idle-bounded wait error = %v, want idle timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("idle-bounded wait took %s, want prompt cancellation", elapsed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	limiter = newWebSocketConnectionRateLimiter(WebSocketHubLimits{Inbound: rate, Outbound: rate})
	if err := limiter.waitInbound(context.Background(), 0, 10); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if err := limiter.waitInbound(ctx, 0, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait error = %v, want context.Canceled", err)
	}
}
