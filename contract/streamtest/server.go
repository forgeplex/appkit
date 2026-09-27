package streamtest

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/contract"
	"github.com/forgeplex/appkit/tx"
)

// ServerOpenFunc adapts a receive-only stream to the shared lifecycle suite.
// produce represents the remote producer, and send emits one response. Fixtures
// must establish the response independently from the first application event
// (for example, SSE can flush a comment). No client Send capability is assumed.
type ServerOpenFunc[T any] func(context.Context, contract.StreamConfig,
	func(context.Context, func(context.Context, T) error) error) (contract.ServerStream[T], error)

// VerifyServer checks the common receive-only capabilities of Local, SSE and
// WebSocket adapters. Verify separately covers bidirectional capabilities;
// this suite does not imply SSE supports Send or CloseSend. Protocol parsing,
// credential expiry and per-frame limits remain adapter-specific tests.
func VerifyServer[T any](t *testing.T, open ServerOpenFunc[T], config contract.StreamConfig, values []T) {
	t.Helper()
	if open == nil || len(values) == 0 || config.MaxDuration <= 0 || config.CloseTimeout <= 0 || config.QueueSize < 0 || config.IdleTimeout < 0 {
		t.Fatal("streamtest: valid opener, stream config and nonempty values are required")
	}
	base := config
	if base.MaxDuration < 3*time.Second {
		base.MaxDuration = 3 * time.Second
	}
	base.IdleTimeout = 0
	start := func(t *testing.T, ctx context.Context, cfg contract.StreamConfig, produce func(context.Context, func(context.Context, T) error) error) contract.ServerStream[T] {
		t.Helper()
		s, err := open(ctx, cfg, produce)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}

	t.Run("ordered_delivery_and_stable_eof", func(t *testing.T) {
		s := start(t, context.Background(), base, func(ctx context.Context, send func(context.Context, T) error) error {
			for _, value := range values {
				if err := send(ctx, value); err != nil {
					return err
				}
			}
			return nil
		})
		for i, want := range values {
			got, err := s.Recv(context.Background())
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("Recv[%d] = (%#v, %v), want %#v", i, got, err, want)
			}
		}
		for range 2 {
			if _, err := s.Recv(context.Background()); !errors.Is(err, io.EOF) {
				t.Fatalf("terminal = %v, want EOF", err)
			}
		}
	})

	t.Run("operation_cancel_preserves_stream", func(t *testing.T) {
		release := make(chan struct{})
		s := start(t, context.Background(), base, func(ctx context.Context, send func(context.Context, T) error) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
				return send(ctx, values[0])
			}
		})
		op, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := s.Recv(op); !apperr.Is(err, apperr.CodeUnavailable) {
			t.Fatalf("cancelled operation = %v, want UNAVAILABLE", err)
		}
		close(release)
		if got, err := s.Recv(context.Background()); err != nil || !reflect.DeepEqual(got, values[0]) {
			t.Fatalf("retry = (%#v, %v), want first value", got, err)
		}
	})

	t.Run("post_commit_error_is_stable", func(t *testing.T) {
		s := start(t, context.Background(), base, func(ctx context.Context, send func(context.Context, T) error) error {
			if err := send(ctx, values[0]); err != nil {
				return err
			}
			return apperr.Conflict("producer stopped")
		})
		if _, err := s.Recv(context.Background()); err != nil {
			t.Fatalf("first event: %v", err)
		}
		_, first := s.Recv(context.Background())
		_, second := s.Recv(context.Background())
		if !apperr.Is(first, apperr.CodeConflict) || first != second {
			t.Fatalf("terminal = (%v, %v), want stable CONFLICT", first, second)
		}
	})

	t.Run("transaction_and_precancel_guard", func(t *testing.T) {
		var called atomic.Bool
		produce := func(context.Context, func(context.Context, T) error) error { called.Store(true); return nil }
		if s, err := open(tx.With(context.Background(), "test-tx"), base, produce); !apperr.Is(err, apperr.CodeTxBoundary) || s != nil || called.Load() {
			t.Fatalf("transaction Open = (%v, %v), handler called=%v", s, err, called.Load())
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if s, err := open(ctx, base, produce); !apperr.Is(err, apperr.CodeUnavailable) || s != nil || called.Load() {
			t.Fatalf("precancelled Open = (%v, %v), handler called=%v", s, err, called.Load())
		}
	})

	for _, mode := range []string{"close", "root_cancel", "max_duration", "idle_timeout"} {
		t.Run(mode, func(t *testing.T) {
			cfg := base
			if mode == "max_duration" {
				cfg.MaxDuration = 250 * time.Millisecond
			}
			if mode == "idle_timeout" {
				cfg.IdleTimeout = 250 * time.Millisecond
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			s := start(t, ctx, cfg, func(ctx context.Context, _ func(context.Context, T) error) error {
				defer close(done)
				<-ctx.Done()
				return ctx.Err()
			})
			switch mode {
			case "close":
				if err := s.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
			case "root_cancel":
				cancel()
			}
			if _, err := s.Recv(context.Background()); !apperr.Is(err, apperr.CodeUnavailable) {
				t.Fatalf("terminal = %v, want UNAVAILABLE", err)
			}
			waitForHandlerExit(t, done, mode)
			if err := s.Close(); err != nil {
				t.Fatalf("idempotent Close: %v", err)
			}
		})
	}
}
