// Package streamtest provides a reusable behavioral suite for contract stream
// adapters. It is intended for adapter tests, not production call paths.
package streamtest

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/callctx"
	"github.com/forgeplex/appkit/contract"
	"github.com/forgeplex/appkit/tx"
	"go.opentelemetry.io/otel/trace"
)

// OpenFunc adapts a transport's stream opener to the conformance suite. An
// implementation may start a local server or test fixture before returning its
// ClientStream; the supplied handler defines that fixture's server behavior.
type OpenFunc[Send, Receive any] func(
	context.Context,
	contract.StreamConfig,
	contract.StreamHandler[Send, Receive],
) (contract.ClientStream[Send, Receive], error)

// Verify runs the shared lifecycle, ordering, cancellation, backpressure,
// transaction, firewall, terminal-error, and close-budget checks against an
// adapter. It requires at least one input value and a deterministic echo mapper.
// The adapter is responsible for mapping the supplied handler to its test
// server; a transport that cannot support bidi may use focused assertions over
// the same public Stream contract instead.
func Verify[Send, Receive any](
	t *testing.T,
	open OpenFunc[Send, Receive],
	config contract.StreamConfig,
	inputs []Send,
	echo func(Send) Receive,
) {
	t.Helper()
	if open == nil {
		t.Fatal("streamtest: OpenFunc must not be nil")
	}
	if config.MaxDuration <= 0 || config.CloseTimeout <= 0 || config.QueueSize < 0 || config.IdleTimeout < 0 {
		t.Fatalf("streamtest: invalid base config: %+v", config)
	}
	if len(inputs) == 0 || echo == nil {
		t.Fatal("streamtest: at least one input and a non-nil echo mapper are required")
	}
	base := config
	if base.MaxDuration < time.Second {
		base.MaxDuration = time.Second
	}
	base.IdleTimeout = 0

	t.Run("values_order_and_stable_eof", func(t *testing.T) {
		want := make([]Receive, len(inputs))
		for i, input := range inputs {
			want[i] = echo(input)
		}
		s, _ := mustOpen(t, open, base, func(ctx context.Context, peer contract.Stream[Receive, Send]) error {
			var received []Send
			for {
				value, err := peer.Recv(ctx)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return err
				}
				received = append(received, value)
			}
			for _, value := range received {
				if err := peer.Send(ctx, echo(value)); err != nil {
					return err
				}
			}
			return nil
		})
		defer ignoreClose(s)
		for _, input := range inputs {
			if err := s.Send(context.Background(), input); err != nil {
				t.Fatalf("Send: %v", err)
			}
		}
		if err := s.CloseSend(context.Background()); err != nil {
			t.Fatalf("CloseSend: %v", err)
		}
		for i, expected := range want {
			got, err := s.Recv(context.Background())
			if err != nil {
				t.Fatalf("Recv[%d]: %v", i, err)
			}
			if !reflect.DeepEqual(got, expected) {
				t.Errorf("Recv[%d] = %#v, want %#v", i, got, expected)
			}
		}
		for i := 0; i < 2; i++ {
			if _, err := s.Recv(context.Background()); !errors.Is(err, io.EOF) {
				t.Fatalf("terminal Recv[%d] = %v, want stable io.EOF", i, err)
			}
		}
	})

	t.Run("operation_cancel_is_retryable", func(t *testing.T) {
		release := make(chan struct{})
		read := make(chan struct{})
		s, _ := mustOpen(t, open, base, func(ctx context.Context, peer contract.Stream[Receive, Send]) error {
			value, err := peer.Recv(ctx)
			if err != nil {
				return err
			}
			close(read)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			return peer.Send(ctx, echo(value))
		})
		defer ignoreClose(s)
		if err := s.Send(context.Background(), inputs[0]); err != nil {
			t.Fatalf("Send: %v", err)
		}
		select {
		case <-read:
		case <-time.After(time.Second):
			t.Fatal("handler did not receive input")
		}
		opCtx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
		_, firstErr := s.Recv(opCtx)
		cancel()
		if !apperr.Is(firstErr, apperr.CodeUnavailable) {
			t.Fatalf("cancelled operation error = %v, want UNAVAILABLE", firstErr)
		}
		close(release)
		got, err := s.Recv(context.Background())
		if err != nil || !reflect.DeepEqual(got, echo(inputs[0])) {
			t.Fatalf("retry Recv = (%#v, %v), want (%#v, nil)", got, err, echo(inputs[0]))
		}
	})

	t.Run("bounded_backpressure_does_not_drop", func(t *testing.T) {
		cfg := base
		cfg.QueueSize = 1
		if cfg.MaxDuration < time.Second {
			cfg.MaxDuration = time.Second
		}
		gate := make(chan struct{})
		consumed := make(chan struct{})
		s, _ := mustOpen(t, open, cfg, func(ctx context.Context, peer contract.Stream[Receive, Send]) error {
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
			first, err := peer.Recv(ctx)
			if err != nil {
				return err
			}
			close(consumed)
			if err := peer.Send(ctx, echo(first)); err != nil {
				return err
			}
			for {
				value, err := peer.Recv(ctx)
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				if err := peer.Send(ctx, echo(value)); err != nil {
					return err
				}
			}
		})
		defer ignoreClose(s)
		if err := s.Send(context.Background(), inputs[0]); err != nil {
			t.Fatalf("first Send: %v", err)
		}
		opCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		secondErr := s.Send(opCtx, inputs[0])
		cancel()
		if !apperr.Is(secondErr, apperr.CodeUnavailable) {
			t.Fatalf("full-queue Send error = %v, want operation UNAVAILABLE", secondErr)
		}
		close(gate)
		select {
		case <-consumed:
		case <-time.After(time.Second):
			t.Fatal("handler did not resume after backpressure")
		}
		if err := s.Send(context.Background(), inputs[0]); err != nil {
			t.Fatalf("retry Send: %v", err)
		}
		if err := s.CloseSend(context.Background()); err != nil {
			t.Fatalf("CloseSend: %v", err)
		}
		for i := 0; i < 2; i++ {
			got, err := s.Recv(context.Background())
			if err != nil || !reflect.DeepEqual(got, echo(inputs[0])) {
				t.Fatalf("Recv[%d] = (%#v, %v), want (%#v, nil)", i, got, err, echo(inputs[0]))
			}
		}
		if _, err := s.Recv(context.Background()); !errors.Is(err, io.EOF) {
			t.Fatalf("terminal Recv = %v, want io.EOF", err)
		}
	})

	t.Run("terminal_error_is_normalized_and_stable", func(t *testing.T) {
		want := apperr.Conflict("conformance terminal error")
		s, _ := mustOpen(t, open, base, func(context.Context, contract.Stream[Receive, Send]) error { return want })
		defer ignoreClose(s)
		_, first := s.Recv(context.Background())
		_, second := s.Recv(context.Background())
		if !apperr.Is(first, apperr.CodeConflict) {
			t.Fatalf("terminal error = %v, want CONFLICT", first)
		}
		if first != second {
			t.Fatalf("terminal error changed across Recv: first=%p second=%p", first, second)
		}
	})

	t.Run("unknown_terminal_error_is_internal_and_stable", func(t *testing.T) {
		cause := errors.New("private transport detail")
		s, _ := mustOpen(t, open, base, func(context.Context, contract.Stream[Receive, Send]) error { return cause })
		defer ignoreClose(s)
		_, first := s.Recv(context.Background())
		_, second := s.Recv(context.Background())
		if !apperr.Is(first, apperr.CodeInternal) || !errors.Is(first, cause) {
			t.Fatalf("unknown terminal error = %v, want INTERNAL with private cause in error chain", first)
		}
		if first != second {
			t.Fatalf("terminal error changed across Recv: first=%p second=%p", first, second)
		}
	})

	t.Run("transaction_guard_and_context_firewall", func(t *testing.T) {
		firewallConfig := base
		if firewallConfig.MaxDuration <= time.Second {
			firewallConfig.MaxDuration = 2 * time.Second
		}
		called := false
		ctx := tx.With(context.Background(), "test-tx")
		_, err := open(ctx, base, func(context.Context, contract.Stream[Receive, Send]) error {
			called = true
			return nil
		})
		if !apperr.Is(err, apperr.CodeTxBoundary) || called {
			t.Fatalf("Open under transaction = (%v, called=%v), want TX_BOUNDARY without handler", err, called)
		}

		type privateKey struct{}
		meta := callctx.Meta{RequestID: "streamtest", Partition: "region-a", TenantID: "test-tenant", Caller: "streamtest"}
		deadline := time.Now().Add(time.Second)
		outer, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		outer = callctx.With(context.WithValue(outer, privateKey{}, "private"), meta)
		var traceID trace.TraceID
		traceID[0] = 1
		var parentSpanID trace.SpanID
		parentSpanID[0] = 1
		parentSpan := trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: traceID, SpanID: parentSpanID, TraceFlags: trace.FlagsSampled,
		})
		outer = trace.ContextWithSpanContext(outer, parentSpan)
		seen := make(chan struct {
			meta        callctx.Meta
			value       any
			span        trace.SpanContext
			hasTx       bool
			deadline    time.Time
			hasDeadline bool
		}, 1)
		s, err := open(outer, firewallConfig, func(ctx context.Context, _ contract.Stream[Receive, Send]) error {
			gotDeadline, hasDeadline := ctx.Deadline()
			seen <- struct {
				meta        callctx.Meta
				value       any
				span        trace.SpanContext
				hasTx       bool
				deadline    time.Time
				hasDeadline bool
			}{callctx.From(ctx), ctx.Value(privateKey{}), trace.SpanFromContext(ctx).SpanContext(), tx.HasTx(ctx), gotDeadline, hasDeadline}
			return nil
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer ignoreClose(s)
		select {
		case got := <-seen:
			if got.meta != meta || got.value != nil || got.hasTx || !got.span.IsValid() || got.span.TraceID() != parentSpan.TraceID() || !got.hasDeadline || !got.deadline.Equal(deadline) {
				t.Fatalf("handler context leaked or lost contract data: %+v", got)
			}
		case <-time.After(time.Second):
			t.Fatal("handler did not start")
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close completed handler: %v", err)
		}
	})

	t.Run("root_cancel_close_and_forced_close", func(t *testing.T) {
		cfg := base
		cfg.CloseTimeout = time.Second
		cancelled := make(chan struct{})
		s, err := open(context.Background(), cfg, func(ctx context.Context, _ contract.Stream[Receive, Send]) error {
			<-ctx.Done()
			close(cancelled)
			return ctx.Err()
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close responsive handler: %v", err)
		}
		select {
		case <-cancelled:
		case <-time.After(time.Second):
			t.Fatal("Close did not cancel the root stream context")
		}
		if err1 := s.Close(); err1 != nil {
			t.Fatalf("repeated Close = %v, want nil", err1)
		}
		rootCtx, cancelRoot := context.WithCancel(context.Background())
		rootCancelled := make(chan struct{})
		rootStream, err := open(rootCtx, cfg, func(ctx context.Context, _ contract.Stream[Receive, Send]) error {
			<-ctx.Done()
			close(rootCancelled)
			return ctx.Err()
		})
		if err != nil {
			t.Fatalf("Open root-cancel stream: %v", err)
		}
		cancelRoot()
		select {
		case <-rootCancelled:
		case <-time.After(time.Second):
			t.Fatal("root context cancellation did not reach the handler")
		}
		if err := rootStream.Close(); err != nil {
			t.Fatalf("Close after root cancellation: %v", err)
		}

		cfg.CloseTimeout = 15 * time.Millisecond
		release := make(chan struct{})
		workerDone := make(chan struct{})
		slow, err := open(context.Background(), cfg, func(context.Context, contract.Stream[Receive, Send]) error {
			<-release // Deliberately non-cooperative, then released so the test leaves no goroutine behind.
			close(workerDone)
			return nil
		})
		if err != nil {
			t.Fatalf("Open slow handler: %v", err)
		}
		closeErr := slow.Close()
		if !apperr.Is(closeErr, apperr.CodeUnavailable) || !errors.Is(closeErr, context.DeadlineExceeded) {
			t.Fatalf("Close timeout = %v, want inspectable UNAVAILABLE", closeErr)
		}
		if second := slow.Close(); second != closeErr {
			t.Fatalf("Close result changed: first=%p second=%p", closeErr, second)
		}
		close(release)
		select {
		case <-workerDone:
		case <-time.After(time.Second):
			t.Fatal("released non-cooperative handler did not exit")
		}
	})

	t.Run("maximum_duration_is_independent_from_unary_timeout", func(t *testing.T) {
		cfg := base
		cfg.MaxDuration = 35 * time.Millisecond
		cfg.IdleTimeout = 0
		s, err := open(context.Background(), cfg, func(ctx context.Context, _ contract.Stream[Receive, Send]) error {
			<-ctx.Done()
			return ctx.Err()
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer ignoreClose(s)
		_, err = s.Recv(context.Background())
		if !apperr.Is(err, apperr.CodeUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("max-duration terminal = %v, want deadline UNAVAILABLE", err)
		}
	})

	t.Run("idle_timeout", func(t *testing.T) {
		cfg := base
		cfg.IdleTimeout = 35 * time.Millisecond
		s, err := open(context.Background(), cfg, func(ctx context.Context, _ contract.Stream[Receive, Send]) error {
			<-ctx.Done()
			return ctx.Err()
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer ignoreClose(s)
		_, err = s.Recv(context.Background())
		if !apperr.Is(err, apperr.CodeUnavailable) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("idle terminal = %v, want idle UNAVAILABLE", err)
		}
	})
}

func mustOpen[Send, Receive any](
	t *testing.T,
	open OpenFunc[Send, Receive],
	cfg contract.StreamConfig,
	handler contract.StreamHandler[Send, Receive],
) (contract.ClientStream[Send, Receive], error) {
	t.Helper()
	s, err := open(context.Background(), cfg, handler)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, err
}

func ignoreClose[Send, Receive any](s contract.ClientStream[Send, Receive]) {
	_ = s.Close()
}
