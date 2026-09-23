package contract

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/internal/metrics"
	"github.com/forgeplex/appkit/tx"
)

var (
	errStreamIdle     = errors.New("stream idle timeout")
	errConcurrentSend = apperr.Conflict("stream permits only one concurrent Send")
	errConcurrentRecv = apperr.Conflict("stream permits only one concurrent Recv")
	errSendSideClosed = apperr.Conflict("stream send side is closed")
)

// StreamConfig defines the independent lifetime and buffering policy of a stream.
// MaxDuration and CloseTimeout are required. IdleTimeout disables idle detection
// when zero. QueueSize is the bounded capacity of each direction; zero means
// rendezvous-only backpressure. No field inherits contract.Call's unary timeout.
type StreamConfig struct {
	// MaxDuration is the required maximum lifetime of the stream.
	MaxDuration time.Duration
	// IdleTimeout ends the stream after this much time without successful Send or
	// Recv activity. Zero disables idle detection.
	IdleTimeout time.Duration
	// CloseTimeout is the required budget for waiting for the producer to exit.
	CloseTimeout time.Duration
	// QueueSize is the bounded message capacity in each direction. Zero applies
	// direct rendezvous backpressure with no buffered messages.
	QueueSize int
}

// Stream is one endpoint of a bidirectional contract stream. Send and Recv each
// accept an operation context: cancellation of that context affects only that
// operation. The stream's root context is controlled by the adapter's Open and
// ClientStream.Close operations.
// Concurrent Send calls and concurrent Recv calls are rejected; one Send and one
// Recv may run at the same time.
// Streaming APIs remain experimental until explicitly promoted under ADR-0047.
type Stream[Send, Receive any] interface {
	// Send enqueues a message in order. A full queue waits for capacity or for
	// either the operation or stream context to end.
	Send(context.Context, Send) error
	// Recv returns the next message, io.EOF, or a stable stream terminal error.
	Recv(context.Context) (Receive, error)
}

// ClientStream is returned by a stream adapter. Close cancels the whole stream,
// waits up to the configured CloseTimeout, and is idempotent.
type ClientStream[Send, Receive any] interface {
	Stream[Send, Receive]
	// CloseSend half-closes the client-to-server direction after any active Send.
	CloseSend(context.Context) error
	// Close cancels the root stream and waits up to StreamConfig.CloseTimeout.
	Close() error
}

// StreamHandler handles the server side of an in-process stream. Its Send and
// Receive types are reversed from the client endpoint.
type StreamHandler[Send, Receive any] func(context.Context, Stream[Receive, Send]) error

// OpenLocal establishes a bidirectional in-process contract stream. Synchronous
// validation, transaction-boundary, and pre-cancelled-context errors are
// returned before the handler starts. Once established, handler errors are
// delivered by Recv and normalized to *apperr.Error. Callers should pass stable
// system and method names, just as with Call.
func OpenLocal[Send, Receive any](ctx context.Context, system, method string, cfg StreamConfig, handler StreamHandler[Send, Receive]) (ClientStream[Send, Receive], error) {
	if ctx == nil {
		return nil, apperr.InvalidArgument("contract stream: context must not be nil")
	}
	if tx.HasTx(ctx) {
		return nil, errTxBoundary.WithDetail("system", system).WithDetail("method", method)
	}
	if err := ctx.Err(); err != nil {
		return nil, apperr.Unavailable(err).
			WithDetail("system", system).WithDetail("method", method)
	}
	if err := validateStreamConfig(cfg); err != nil {
		return nil, err
	}
	if handler == nil {
		return nil, apperr.InvalidArgument("contract stream: handler must not be nil")
	}

	started := time.Now()
	root, cancel := context.WithTimeout(Firewall(ctx), cfg.MaxDuration)
	tracer := otel.Tracer("appkit/contract")
	streamCtx, span := tracer.Start(root, system+"."+method,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String(metrics.AttrSystem, system),
			attribute.String(metrics.AttrMethod, method),
		),
	)
	session := &localSession{
		ctx:         streamCtx,
		cancel:      cancel,
		done:        make(chan struct{}),
		handlerDone: make(chan struct{}),
		watchDone:   make(chan struct{}),
		force:       make(chan struct{}),
		touch:       make(chan struct{}, 1),
		span:        span,
		started:     started,
	}
	requests := newStreamDirection[Send](cfg.QueueSize)
	responses := newStreamDirection[Receive](cfg.QueueSize)
	client := &localStream[Send, Receive]{
		session: session,
		out:     requests,
		in:      responses,
		config:  cfg,
	}
	server := &localStream[Receive, Send]{
		session: session,
		out:     responses,
		in:      requests,
		config:  cfg,
	}

	go func() {
		defer close(session.watchDone)
		session.watch(cfg.IdleTimeout)
	}()
	go func() {
		defer close(session.handlerDone)
		err := handler(streamCtx, server)
		session.finish(err)
	}()
	return client, nil
}

func validateStreamConfig(cfg StreamConfig) error {
	switch {
	case cfg.MaxDuration <= 0:
		return apperr.InvalidArgument("contract stream: MaxDuration must be positive")
	case cfg.IdleTimeout < 0:
		return apperr.InvalidArgument("contract stream: IdleTimeout must not be negative")
	case cfg.CloseTimeout <= 0:
		return apperr.InvalidArgument("contract stream: CloseTimeout must be positive")
	case cfg.QueueSize < 0:
		return apperr.InvalidArgument("contract stream: QueueSize must not be negative")
	default:
		return nil
	}
}

type streamDirection[T any] struct {
	queue  chan T
	closed chan struct{}
	state  sendState
}

func newStreamDirection[T any](capacity int) *streamDirection[T] {
	return &streamDirection[T]{
		queue:  make(chan T, capacity),
		closed: make(chan struct{}),
		state:  sendState{changed: closedSignal()},
	}
}

// sendState serializes the close-send transition with the single in-flight Send
// without holding a mutex while a bounded queue applies backpressure.
type sendState struct {
	mu      sync.Mutex
	busy    bool
	closed  bool
	changed chan struct{}
}

func closedSignal() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func (s *sendState) begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errSendSideClosed
	}
	if s.busy {
		return errConcurrentSend
	}
	s.busy = true
	s.changed = make(chan struct{})
	return nil
}

func (s *sendState) end() {
	s.mu.Lock()
	s.busy = false
	close(s.changed)
	s.mu.Unlock()
}

func (d *streamDirection[T]) closeSend(ctx context.Context, session *localSession) error {
	for {
		if err := session.currentTerminal(); err != nil {
			return err
		}
		d.state.mu.Lock()
		if d.state.closed {
			d.state.mu.Unlock()
			return nil
		}
		if err := ctx.Err(); err != nil {
			d.state.mu.Unlock()
			return normalize(ctx, err)
		}
		if !d.state.busy {
			d.state.closed = true
			close(d.closed)
			d.state.mu.Unlock()
			return nil
		}
		changed := d.state.changed
		d.state.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			return normalize(ctx, ctx.Err())
		case <-session.done:
			return session.terminalError()
		case <-session.ctx.Done():
			session.finish(normalize(session.ctx, session.ctx.Err()))
			return session.terminalError()
		case <-session.force:
			return session.terminalError()
		}
	}
}

type localSession struct {
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	handlerDone  chan struct{}
	watchDone    chan struct{}
	force        chan struct{}
	touch        chan struct{}
	span         trace.Span
	started      time.Time
	lastActivity atomic.Int64

	finishOnce sync.Once
	forceOnce  sync.Once
	terminalMu sync.RWMutex
	terminal   error
}

func (s *localSession) touchActivity() {
	s.lastActivity.Store(time.Since(s.started).Nanoseconds())
	select {
	case s.touch <- struct{}{}:
	default:
	}
}

func (s *localSession) terminalError() error {
	s.terminalMu.RLock()
	defer s.terminalMu.RUnlock()
	return s.terminal
}

func (s *localSession) isDone() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *localSession) currentTerminal() error {
	if s.isDone() {
		return s.terminalError()
	}
	if err := s.ctx.Err(); err != nil {
		s.finish(normalize(s.ctx, err))
		return s.terminalError()
	}
	return nil
}

func (s *localSession) finish(err error) {
	s.finishOnce.Do(func() {
		if err == nil || errors.Is(err, io.EOF) {
			if ctxErr := s.ctx.Err(); ctxErr != nil {
				err = normalize(s.ctx, ctxErr)
			} else {
				err = io.EOF
			}
		} else {
			err = normalize(s.ctx, err)
		}
		s.terminalMu.Lock()
		s.terminal = err
		s.terminalMu.Unlock()
		if !errors.Is(err, io.EOF) {
			code := apperr.From(err).Code()
			s.span.SetStatus(codes.Error, code)
		} else {
			s.span.SetStatus(codes.Ok, "")
		}
		close(s.done)
		s.cancel()
		s.span.End()
	})
}

func (s *localSession) watch(idleTimeout time.Duration) {
	if idleTimeout <= 0 {
		<-s.ctx.Done()
		s.finish(normalize(s.ctx, s.ctx.Err()))
		return
	}
	timer := time.NewTimer(idleTimeout)
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			s.finish(normalize(s.ctx, s.ctx.Err()))
			return
		case <-s.touch:
			remaining := idleTimeout - (time.Since(s.started) - time.Duration(s.lastActivity.Load()))
			if remaining <= 0 {
				s.finish(apperr.Unavailable(errStreamIdle))
				return
			}
			resetTimer(timer, remaining)
		case <-timer.C:
			remaining := idleTimeout - (time.Since(s.started) - time.Duration(s.lastActivity.Load()))
			if remaining <= 0 {
				s.finish(apperr.Unavailable(errStreamIdle))
				return
			}
			timer.Reset(remaining)
		}
	}
}

func resetTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}

type localStream[Send, Receive any] struct {
	session *localSession
	out     *streamDirection[Send]
	in      *streamDirection[Receive]
	config  StreamConfig

	recvBusy  busyGate
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

// busyGate is a small gate used only to reject overlapping Recv calls. It
// avoids leaving a second blocked Recv goroutine behind the active operation.
type busyGate struct {
	mu   sync.Mutex
	busy bool
}

func (f *busyGate) tryBegin() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy {
		return false
	}
	f.busy = true
	return true
}

func (f *busyGate) end() {
	f.mu.Lock()
	f.busy = false
	f.mu.Unlock()
}

func (s *localStream[Send, Receive]) Send(ctx context.Context, value Send) error {
	if ctx == nil {
		return apperr.InvalidArgument("contract stream: Send context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return normalize(ctx, err)
	}
	if err := s.out.state.begin(); err != nil {
		return apperr.From(err)
	}
	defer s.out.state.end()
	if err := s.session.currentTerminal(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return normalize(ctx, ctx.Err())
	case <-s.session.done:
		return s.session.terminalError()
	case <-s.session.ctx.Done():
		s.session.finish(normalize(s.session.ctx, s.session.ctx.Err()))
		return s.session.terminalError()
	case <-s.session.force:
		return s.session.terminalError()
	case <-s.out.closed:
		return apperr.From(errSendSideClosed)
	case s.out.queue <- value:
		s.session.touchActivity()
		return nil
	}
}

func (s *localStream[Send, Receive]) Recv(ctx context.Context) (Receive, error) {
	var zero Receive
	if ctx == nil {
		return zero, apperr.InvalidArgument("contract stream: Recv context must not be nil")
	}
	if !s.recvBusy.tryBegin() {
		return zero, apperr.From(errConcurrentRecv)
	}
	defer s.recvBusy.end()
	if err := ctx.Err(); err != nil {
		return zero, normalize(ctx, err)
	}
	for {
		// Drain accepted messages before reporting a half-close or stream terminal.
		select {
		case value := <-s.in.queue:
			s.session.touchActivity()
			return value, nil
		default:
		}
		if s.session.isDone() {
			select {
			case value := <-s.in.queue:
				s.session.touchActivity()
				return value, nil
			default:
			}
			return zero, s.session.terminalError()
		}
		select {
		case value := <-s.in.queue:
			s.session.touchActivity()
			return value, nil
		case <-s.in.closed:
			// CloseSend may race with the blocking select after the first drain;
			// drain once more before exposing EOF so accepted values are not lost.
			select {
			case value := <-s.in.queue:
				s.session.touchActivity()
				return value, nil
			default:
				return zero, io.EOF
			}
		case <-ctx.Done():
			return zero, normalize(ctx, ctx.Err())
		case <-s.session.done:
			continue
		case <-s.session.ctx.Done():
			s.session.finish(normalize(s.session.ctx, s.session.ctx.Err()))
			continue
		case <-s.session.force:
			continue
		}
	}
}

func (s *localStream[Send, Receive]) CloseSend(ctx context.Context) error {
	if ctx == nil {
		return apperr.InvalidArgument("contract stream: CloseSend context must not be nil")
	}
	if err := s.session.currentTerminal(); err != nil {
		return err
	}
	return s.out.closeSend(ctx, s.session)
}

func (s *localStream[Send, Receive]) Close() error {
	s.closeOnce.Do(func() {
		s.closeDone = make(chan struct{})
		go s.closeBounded()
	})
	<-s.closeDone
	return s.closeErr
}

func (s *localStream[Send, Receive]) closeBounded() {
	defer close(s.closeDone)
	if !s.session.isDone() {
		s.session.finish(apperr.Unavailable(context.Canceled))
	}
	timer := time.NewTimer(s.config.CloseTimeout)
	defer timer.Stop()
	producerDone := (<-chan struct{})(s.session.handlerDone)
	watchDone := (<-chan struct{})(s.session.watchDone)
	for producerDone != nil || watchDone != nil {
		select {
		case <-producerDone:
			producerDone = nil
		case <-watchDone:
			watchDone = nil
		case <-timer.C:
			s.session.forceOnce.Do(func() { close(s.session.force) })
			s.closeErr = apperr.Unavailable(fmt.Errorf("local producer did not stop within %s: %w", s.config.CloseTimeout, context.DeadlineExceeded))
			return
		}
	}
}
