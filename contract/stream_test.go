package contract_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/callctx"
	"github.com/forgeplex/appkit/contract"
	"github.com/forgeplex/appkit/contract/streamtest"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

type streamRequest struct{ Sequence int }
type streamResponse struct{ Sequence int }

func openLocalStream(
	ctx context.Context,
	cfg contract.StreamConfig,
	handler contract.StreamHandler[streamRequest, streamResponse],
) (contract.ClientStream[streamRequest, streamResponse], error) {
	return contract.OpenLocal(ctx, "test", "Stream", cfg, handler)
}

func TestLocalStreamConformance(t *testing.T) {
	streamtest.Verify(t, openLocalStream,
		contract.StreamConfig{
			MaxDuration:  2 * time.Second,
			CloseTimeout: time.Second,
			QueueSize:    2,
		},
		[]streamRequest{{Sequence: 1}, {Sequence: 2}, {Sequence: 3}},
		func(request streamRequest) streamResponse { return streamResponse{Sequence: request.Sequence} },
	)
}

func TestLocalStreamSpanEndsAtTerminal(t *testing.T) {
	previous := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(context.Background())
	})

	started := make(chan trace.SpanContext, 1)
	s, err := openLocalStream(context.Background(), contract.StreamConfig{
		MaxDuration: time.Second, CloseTimeout: time.Second,
	}, func(ctx context.Context, _ contract.Stream[streamResponse, streamRequest]) error {
		started <- trace.SpanFromContext(ctx).SpanContext()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	streamSpan := <-started
	if !streamSpan.IsValid() {
		t.Fatal("handler context did not contain the stream span")
	}
	spans := recorder.Ended()
	if len(spans) != 1 || spans[0].Name() != "test.Stream" {
		t.Fatalf("ended spans = %v, want one test.Stream span", spans)
	}
	if got := spans[0].SpanContext(); got.TraceID() != streamSpan.TraceID() || got.SpanID() != streamSpan.SpanID() {
		t.Fatalf("recorded stream span context = %v, handler saw %v", got, streamSpan)
	}
	if spans[0].EndTime().Before(spans[0].StartTime()) {
		t.Fatalf("stream span did not cover an interval: start=%v end=%v", spans[0].StartTime(), spans[0].EndTime())
	}
}

func TestLocalStreamAllowsSendRecvOverlap(t *testing.T) {
	cfg := contract.StreamConfig{MaxDuration: time.Second, CloseTimeout: time.Second}
	s, err := openLocalStream(context.Background(), cfg, func(ctx context.Context, peer contract.Stream[streamResponse, streamRequest]) error {
		request, err := peer.Recv(ctx)
		if err != nil {
			return err
		}
		return peer.Send(ctx, streamResponse{Sequence: request.Sequence})
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	sendResult := make(chan error, 1)
	go func() { sendResult <- s.Send(context.Background(), streamRequest{Sequence: 7}) }()
	got, err := s.Recv(context.Background())
	if err != nil {
		t.Fatalf("Recv overlapping Send: %v", err)
	}
	if got.Sequence != 7 {
		t.Fatalf("response = %+v, want sequence 7", got)
	}
	if err := <-sendResult; err != nil {
		t.Fatalf("Send overlapping Recv: %v", err)
	}
	if _, err := s.Recv(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal Recv = %v, want io.EOF", err)
	}
}

func TestLocalStreamRejectsConcurrentSendAndRecv(t *testing.T) {
	cfg := contract.StreamConfig{MaxDuration: 2 * time.Second, CloseTimeout: time.Second}
	release := make(chan struct{})
	s, err := openLocalStream(context.Background(), cfg, func(ctx context.Context, peer contract.Stream[streamResponse, streamRequest]) error {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		request, err := peer.Recv(ctx)
		if err != nil {
			return err
		}
		return peer.Send(ctx, streamResponse{Sequence: request.Sequence})
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	start := make(chan struct{})
	sendResults := make(chan error, 2)
	for i := 1; i <= 2; i++ {
		go func(sequence int) {
			<-start
			sendResults <- s.Send(context.Background(), streamRequest{Sequence: sequence})
		}(i)
	}
	close(start)
	select {
	case err := <-sendResults:
		if !apperr.Is(err, apperr.CodeConflict) {
			t.Fatalf("overlapping Send error = %v, want CONFLICT", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second Send was neither rejected nor completed")
	}
	close(release)
	var accepted int
	for i := 0; i < 1; i++ {
		select {
		case err := <-sendResults:
			if err == nil {
				accepted++
			} else if !apperr.Is(err, apperr.CodeConflict) {
				t.Fatalf("Send error = %v, want nil or CONFLICT", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Send did not finish")
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted Sends = %d, want exactly 1", accepted)
	}
	if err := s.CloseSend(context.Background()); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	if _, err := s.Recv(context.Background()); err != nil {
		t.Fatalf("Recv accepted response: %v", err)
	}
	if _, err := s.Recv(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal Recv = %v, want io.EOF", err)
	}

	// Two Recv operations race while the server waits to send. One remains the
	// sole active read; the other must fail immediately rather than queue a waiter.
	ready := make(chan struct{})
	serverGate := make(chan struct{})
	s2, err := openLocalStream(context.Background(), cfg, func(ctx context.Context, peer contract.Stream[streamResponse, streamRequest]) error {
		_, err := peer.Recv(ctx)
		if err != nil {
			return err
		}
		close(ready)
		select {
		case <-serverGate:
		case <-ctx.Done():
			return ctx.Err()
		}
		return peer.Send(ctx, streamResponse{Sequence: 8})
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if err := s2.Send(context.Background(), streamRequest{Sequence: 8}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("server did not reach blocked-send point")
	}
	type recvResult struct {
		value streamResponse
		err   error
	}
	startRecv := make(chan struct{})
	recvResults := make(chan recvResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-startRecv
			value, err := s2.Recv(context.Background())
			recvResults <- recvResult{value: value, err: err}
		}()
	}
	close(startRecv)
	select {
	case result := <-recvResults:
		if !apperr.Is(result.err, apperr.CodeConflict) {
			t.Fatalf("overlapping Recv error = %v, want CONFLICT", result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("second Recv was neither rejected nor completed")
	}
	close(serverGate)
	select {
	case result := <-recvResults:
		if result.err != nil || result.value.Sequence != 8 {
			t.Fatalf("active Recv = (%+v, %v), want response 8", result.value, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("active Recv did not complete")
	}
}

func TestLocalStreamContextFirewallPreservesWhitelist(t *testing.T) {
	type privateKey struct{}
	meta := callctx.Meta{RequestID: "local-stream", Partition: "region-a", TenantID: "tenant-a", Caller: "gateway"}
	deadline := time.Now().Add(time.Minute)
	outer, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	outer = callctx.With(context.WithValue(outer, privateKey{}, "private"), meta)
	var traceID trace.TraceID
	traceID[0] = 1
	var parentSpanID trace.SpanID
	parentSpanID[0] = 1
	parent := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: parentSpanID, TraceFlags: trace.FlagsSampled,
	})
	outer = trace.ContextWithSpanContext(outer, parent)
	type observation struct {
		meta        callctx.Meta
		private     any
		span        trace.SpanContext
		deadline    time.Time
		hasDeadline bool
	}
	seen := make(chan observation, 1)
	s, err := openLocalStream(outer, contract.StreamConfig{MaxDuration: 2 * time.Minute, CloseTimeout: time.Second},
		func(ctx context.Context, _ contract.Stream[streamResponse, streamRequest]) error {
			gotDeadline, hasDeadline := ctx.Deadline()
			seen <- observation{callctx.From(ctx), ctx.Value(privateKey{}), trace.SpanFromContext(ctx).SpanContext(), gotDeadline, hasDeadline}
			return nil
		})
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	defer func() { _ = s.Close() }()
	select {
	case got := <-seen:
		if got.meta != meta || got.private != nil || !got.span.IsValid() || got.span.TraceID() != parent.TraceID() || !got.hasDeadline || !got.deadline.Equal(deadline) {
			t.Fatalf("local stream context = %+v, want only the whitelist and parent trace/deadline", got)
		}
	case <-time.After(time.Second):
		t.Fatal("stream handler did not start")
	}
}

func TestLocalStreamBackpressureIsOperationScoped(t *testing.T) {
	gate := make(chan struct{})
	cfg := contract.StreamConfig{MaxDuration: 2 * time.Second, CloseTimeout: time.Second, QueueSize: 1}
	s, err := openLocalStream(context.Background(), cfg, func(ctx context.Context, peer contract.Stream[streamResponse, streamRequest]) error {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
		for {
			request, err := peer.Recv(ctx)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if err := peer.Send(ctx, streamResponse{Sequence: request.Sequence}); err != nil {
				return err
			}
		}
	})
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Send(context.Background(), streamRequest{Sequence: 1}); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	opCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err = s.Send(opCtx, streamRequest{Sequence: 2})
	cancel()
	if !apperr.Is(err, apperr.CodeUnavailable) {
		t.Fatalf("full-queue Send error = %v, want operation UNAVAILABLE", err)
	}
	close(gate)
	if err := s.Send(context.Background(), streamRequest{Sequence: 2}); err != nil {
		t.Fatalf("retry Send after operation cancellation: %v", err)
	}
	if err := s.CloseSend(context.Background()); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	for _, want := range []int{1, 2} {
		got, err := s.Recv(context.Background())
		if err != nil || got.Sequence != want {
			t.Fatalf("Recv = (%+v, %v), want sequence %d", got, err, want)
		}
	}
	if _, err := s.Recv(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal Recv = %v, want io.EOF", err)
	}
}

func TestLocalStreamCloseTimeoutAndUnknownCause(t *testing.T) {
	t.Run("non-cooperative handler exceeds close budget", func(t *testing.T) {
		release := make(chan struct{})
		workerDone := make(chan struct{})
		t.Cleanup(func() {
			select {
			case <-release:
			default:
				close(release)
			}
		})
		cfg := contract.StreamConfig{MaxDuration: time.Second, CloseTimeout: 15 * time.Millisecond}
		s, err := openLocalStream(context.Background(), cfg, func(context.Context, contract.Stream[streamResponse, streamRequest]) error {
			<-release // Deliberately ignores cancellation; release it before the test exits.
			close(workerDone)
			return nil
		})
		if err != nil {
			t.Fatalf("OpenLocal: %v", err)
		}
		closeErr := s.Close()
		if !apperr.Is(closeErr, apperr.CodeUnavailable) || !errors.Is(closeErr, context.DeadlineExceeded) {
			t.Fatalf("Close error = %v, want inspectable UNAVAILABLE", closeErr)
		}
		if second := s.Close(); second != closeErr {
			t.Fatalf("Close result changed: first=%p second=%p", closeErr, second)
		}
		close(release)
		select {
		case <-workerDone:
		case <-time.After(time.Second):
			t.Fatal("released non-cooperative handler did not exit")
		}
	})

	t.Run("unknown handler cause remains inspectable locally", func(t *testing.T) {
		cause := errors.New("private handler detail")
		s, err := openLocalStream(context.Background(), contract.StreamConfig{MaxDuration: time.Second, CloseTimeout: time.Second},
			func(context.Context, contract.Stream[streamResponse, streamRequest]) error { return cause })
		if err != nil {
			t.Fatalf("OpenLocal: %v", err)
		}
		defer func() { _ = s.Close() }()
		_, err = s.Recv(context.Background())
		if !apperr.Is(err, apperr.CodeInternal) || !errors.Is(err, cause) {
			t.Fatalf("local terminal error = %v, want INTERNAL with inspectable cause", err)
		}
	})
}

func TestOpenLocalSynchronousFailures(t *testing.T) {
	cfg := contract.StreamConfig{MaxDuration: time.Second, CloseTimeout: time.Second}
	cases := []struct {
		name   string
		ctx    context.Context
		config contract.StreamConfig
		code   string
	}{
		{name: "pre-cancelled context", ctx: cancelledContext(), config: cfg, code: apperr.CodeUnavailable},
		{name: "missing maximum duration", ctx: context.Background(), config: contract.StreamConfig{CloseTimeout: time.Second}, code: apperr.CodeInvalidArgument},
		{name: "negative queue", ctx: context.Background(), config: contract.StreamConfig{MaxDuration: time.Second, CloseTimeout: time.Second, QueueSize: -1}, code: apperr.CodeInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			_, err := openLocalStream(tc.ctx, tc.config, func(context.Context, contract.Stream[streamResponse, streamRequest]) error {
				called = true
				return nil
			})
			if !apperr.Is(err, tc.code) || called {
				t.Fatalf("OpenLocal = (%v, called=%v), want %s before handler", err, called, tc.code)
			}
		})
	}
	var nilHandler contract.StreamHandler[streamRequest, streamResponse]
	if _, err := contract.OpenLocal(context.Background(), "test", "nil", cfg, nilHandler); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("nil handler error = %v, want INVALID_ARGUMENT", err)
	}
}

func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
