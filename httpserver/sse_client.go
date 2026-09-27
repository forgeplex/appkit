package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/contract"
	"github.com/forgeplex/appkit/internal/metrics"
	"github.com/forgeplex/appkit/tx"
)

// SSEClientConfig defines the budgets for one POST + SSE invocation. Stream
// budgets start before the HTTP handshake. A configured HTTPClient.Timeout is
// an additional whole-response deadline; leave it zero for a stream whose
// lifetime is controlled solely by Stream. Heartbeats do not reset IdleTimeout.
type SSEClientConfig struct {
	System string
	Method string
	Stream contract.StreamConfig
	// MaxRequestBodyBytes bounds the single JSON request.
	MaxRequestBodyBytes int64
	// MaxEventBytes bounds each wire event block, including field names,
	// comments, cursor and delimiters, before JSON decoding. A dispatching CR
	// may have one additional LF framing byte, skipped without waiting for it.
	// QueueSize bounds decoded messages; the parser may hold one additional
	// event under pressure.
	MaxEventBytes int64
	// LastEventID is an optional opaque cursor sent once in Last-Event-ID.
	// AppKit never persists this value or retries/reconnects the stream.
	LastEventID string
}

// DialSecureSSE sends one JSON request and establishes an HTTPS-only,
// origin-bound server stream. Credential delegation, TLS verification and the
// context firewall are shared with NewSecureHTTPClient. The credential is
// acquired once and its signed expiry ends the stream; it is never refreshed.
// HTTP problems before commitment are returned by Dial; in-band error events
// become Recv errors with their stable code but no remote message or details.
//
// A complete event boundary followed by HTTP EOF is normal completion. The
// existing SSE wire protocol has no terminal marker, so a clean early EOF
// cannot be distinguished from intentional completion. Incomplete data events,
// unterminated lines and transport failures are errors. Business completion
// belongs in the event DTO.
// Streaming APIs remain experimental until explicitly promoted under ADR-0047.
func DialSecureSSE[Request, Event any](
	ctx context.Context,
	address string,
	cfg SSEClientConfig,
	secure contract.SecureClientOptions,
	request Request,
) (contract.ServerStream[SSEEvent[Event]], error) {
	if ctx == nil {
		return nil, apperr.InvalidArgument("httpserver SSE dial: context must not be nil")
	}
	if tx.HasTx(ctx) {
		return nil, apperr.New(apperr.CodeTxBoundary, http.StatusInternalServerError,
			"httpserver SSE: cannot open a stream inside a transaction").
			WithDetail("system", cfg.System).WithDetail("method", cfg.Method)
	}
	if err := ctx.Err(); err != nil {
		return nil, apperr.Unavailable(err)
	}
	if err := validateSSEClientConfig(cfg); err != nil {
		return nil, err
	}
	if nilSSECredentialProvider(secure.Credentials) {
		return nil, apperr.InvalidArgument("secure SSE credential provider is required")
	}
	lease := &sseCredentialLease{provider: secure.Credentials}
	secure.Credentials = lease
	hc, err := contract.NewSecureHTTPClient(address, secure)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, apperr.InvalidArgument("SSE request cannot be encoded as JSON")
	}
	if int64(len(body)) > cfg.MaxRequestBodyBytes {
		return nil, apperr.InvalidArgument("SSE request exceeds the configured body limit")
	}
	ready := make(chan error, 1)
	started := make(chan context.Context, 1)
	stream, err := contract.OpenLocal[struct{}, SSEEvent[Event]](ctx, cfg.System, cfg.Method, cfg.Stream,
		func(streamCtx context.Context, peer contract.Stream[SSEEvent[Event], struct{}]) (retErr error) {
			started <- streamCtx
			ioCtx, cancel := context.WithCancelCause(streamCtx)
			lease.cancel = cancel
			defer cancel(nil)
			defer lease.stop()
			defer hc.CloseIdleConnections()
			established := false
			defer func() {
				if !established {
					ready <- retErr
				}
			}()
			req, err := http.NewRequestWithContext(ioCtx, http.MethodPost, address, bytes.NewReader(body))
			if err != nil {
				return apperr.InvalidArgument("invalid SSE request URL")
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "text/event-stream")
			if cfg.LastEventID != "" {
				req.Header.Set(lastEventIDKey, cfg.LastEventID)
			}
			response, err := hc.Do(req)
			if err != nil {
				return sseClientIOError(ioCtx, err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				return readSSEClientProblem(ioCtx, response, cfg.MaxEventBytes)
			}
			mediaType, params, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
			if err != nil || mediaType != "text/event-stream" || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) {
				return apperr.InvalidArgument("remote SSE response must use text/event-stream with UTF-8")
			}
			if ioCtx.Err() != nil {
				return sseClientIOError(ioCtx, ioCtx.Err())
			}
			established = true
			ready <- nil
			return readSSEClientEvents(ioCtx, response.Body, peer, cfg)
		})
	if err != nil {
		return nil, err
	}
	streamCtx := <-started
	select {
	case err = <-ready:
	case <-streamCtx.Done():
		// A fast successful response may finish (and cancel its Local Stream)
		// before Dial is scheduled again. Its handshake result remains decisive.
		select {
		case err = <-ready:
		default:
			err = apperr.Unavailable(streamCtx.Err())
		}
	}
	if err != nil {
		_ = stream.Close()
		return nil, err
	}
	return sseClientStream[Event]{inner: stream}, nil
}

func validateSSEClientConfig(cfg SSEClientConfig) error {
	if strings.TrimSpace(cfg.System) == "" || strings.TrimSpace(cfg.Method) == "" {
		return apperr.InvalidArgument("httpserver SSE dial: System and Method are required")
	}
	if cfg.Stream.MaxDuration <= 0 || cfg.Stream.CloseTimeout <= 0 || cfg.Stream.IdleTimeout < 0 || cfg.Stream.QueueSize < 0 {
		return apperr.InvalidArgument("httpserver SSE dial: Stream config is invalid")
	}
	if cfg.MaxRequestBodyBytes <= 0 || cfg.MaxEventBytes <= 0 || cfg.MaxEventBytes == int64(^uint64(0)>>1) {
		return apperr.InvalidArgument("httpserver SSE dial: request and event limits must be positive and bounded")
	}
	if strings.ContainsAny(cfg.LastEventID, "\x00\r\n") {
		return apperr.InvalidArgument("httpserver SSE dial: LastEventID must not contain NUL, CR or LF")
	}
	return nil
}

// A concrete wrapper prevents a ServerStream from being type-asserted back to
// the underlying bidirectional local stream and exposing Send/CloseSend.
type sseClientStream[T any] struct {
	inner contract.ClientStream[struct{}, SSEEvent[T]]
}

func (s sseClientStream[T]) Recv(ctx context.Context) (SSEEvent[T], error) { return s.inner.Recv(ctx) }
func (s sseClientStream[T]) Close() error                                  { return s.inner.Close() }

var errSSECredentialExpired = errors.New("SSE service credential expired")

type sseCredentialLease struct {
	provider contract.ServiceCredentialProvider
	cancel   context.CancelCauseFunc
	mu       sync.Mutex
	timer    *time.Timer
	stopped  bool
}

func (p *sseCredentialLease) ServiceCredential(ctx context.Context, scope contract.ServiceScope) (contract.ServiceCredential, error) {
	credential, err := p.provider.ServiceCredential(ctx, scope)
	if err == nil {
		p.mu.Lock()
		if !p.stopped && credential.ExpiresAt.After(time.Now()) {
			p.timer = time.AfterFunc(time.Until(credential.ExpiresAt), func() { p.cancel(errSSECredentialExpired) })
		}
		p.mu.Unlock()
	}
	return credential, err
}

func (p *sseCredentialLease) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	if p.timer != nil {
		p.timer.Stop()
	}
}

func nilSSECredentialProvider(provider contract.ServiceCredentialProvider) bool {
	if provider == nil {
		return true
	}
	v := reflect.ValueOf(provider)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func sseClientIOError(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), errSSECredentialExpired) {
		return apperr.Unauthenticated("service credential expired")
	}
	if ctx.Err() != nil {
		return apperr.Unavailable(ctx.Err())
	}
	// net/http wraps transport errors with a URL. Keep only the stable identity;
	// neither the endpoint path nor TLS/provider diagnostics belong in the error.
	for _, code := range []string{apperr.CodeUnauthenticated, apperr.CodePermissionDenied, apperr.CodeInvalidArgument} {
		if apperr.Is(err, code) {
			return apperr.New(code, apperr.From(err).Status(), "secure SSE request failed")
		}
	}
	return apperr.Unavailable(nil)
}

func readSSEClientProblem(ctx context.Context, response *http.Response, limit int64) error {
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return sseClientIOError(ctx, err)
	}
	if int64(len(body)) > limit {
		return apperr.InvalidArgument("SSE problem response exceeds the configured event limit")
	}
	problem := apperr.FromProblem(response.StatusCode, body)
	return apperr.New(safeWireCode(problem.Code()), response.StatusCode, "remote SSE request failed")
}

func readSSEClientEvents[T any](ctx context.Context, body io.Reader, peer contract.Stream[SSEEvent[T], struct{}], cfg SSEClientConfig) error {
	decoder := newSSEDecoder(sseClientMetricReader{Reader: body, ctx: ctx, cfg: cfg}, cfg.MaxEventBytes, cfg.LastEventID)
	for {
		frame, err := decoder.next()
		if ctx.Err() != nil {
			return sseClientIOError(ctx, ctx.Err())
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if apperr.Is(err, apperr.CodeInvalidArgument) {
				metrics.StreamProtocolError(ctx, cfg.System, cfg.Method, metrics.TransportSSE, metrics.DirectionServerToClient, err)
				return err
			}
			return sseClientIOError(ctx, err)
		}
		metrics.StreamTransportFrameReceived(ctx, cfg.System, cfg.Method, metrics.TransportSSE, metrics.DirectionServerToClient, frame.kind)
		switch frame.kind {
		case metrics.FrameHeartbeat, metrics.OutcomeOther:
			continue
		case metrics.FrameError:
			var payload struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(frame.data, &payload); err != nil {
				err := apperr.InvalidArgument("invalid SSE error event JSON")
				metrics.StreamProtocolError(ctx, cfg.System, cfg.Method, metrics.TransportSSE, metrics.DirectionServerToClient, err)
				return err
			}
			return remoteWireError(payload.Code)
		default:
			var event T
			if err := json.Unmarshal(frame.data, &event); err != nil {
				err := apperr.InvalidArgument("invalid SSE event JSON")
				metrics.StreamProtocolError(ctx, cfg.System, cfg.Method, metrics.TransportSSE, metrics.DirectionServerToClient, err)
				return err
			}
			if err := peer.Send(ctx, SSEEvent[T]{ID: frame.id, Data: event}); err != nil {
				if ctx.Err() != nil {
					return sseClientIOError(ctx, err)
				}
				return err
			}
		}
	}
}

type sseClientMetricReader struct {
	io.Reader
	ctx context.Context
	cfg SSEClientConfig
}

func (r sseClientMetricReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	metrics.StreamTransportBytesReceived(r.ctx, r.cfg.System, r.cfg.Method, metrics.TransportSSE, metrics.DirectionServerToClient, int64(n))
	return n, err
}
