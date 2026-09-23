package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/contract"
)

const (
	sseMediaType   = "text/event-stream; charset=utf-8"
	sseErrorEvent  = "error"
	lastEventIDKey = "Last-Event-ID"
)

// SSEConfig defines the HTTP and Local Stream budgets for one POST + SSE route.
// Stream durations and queue capacity are independent from the ordinary HTTP
// server timeouts. WriteTimeout bounds each response frame write; heartbeat is
// optional and, when enabled, is emitted as an SSE comment rather than an
// application event. Streaming APIs remain experimental until explicitly
// promoted under ADR-0047.
type SSEConfig struct {
	// System and Method are stable contract names used by the Local Stream span.
	System string
	Method string
	// Stream defines max lifetime, idle policy, close budget, and bounded queue.
	Stream contract.StreamConfig
	// MaxRequestBodyBytes is the maximum JSON request size and must be positive.
	MaxRequestBodyBytes int64
	// WriteTimeout bounds each individual data, heartbeat, or error frame write.
	// It does not alter http.Server.WriteTimeout for other routes.
	WriteTimeout time.Duration
	// HeartbeatInterval emits comment frames to keep intermediaries from timing
	// out an otherwise active response. Zero disables heartbeats.
	HeartbeatInterval time.Duration
}

// SSEEvent is one application event written as a JSON SSE data frame. ID is an
// optional opaque application cursor, copied verbatim to the SSE id field when
// it contains no NUL, CR, or LF. AppKit does not parse, persist, or replay it.
type SSEEvent[T any] struct {
	ID   string
	Data T
}

// SSEHandler handles one HTTP request on the server side of a Local Stream.
// The opaque Last-Event-ID value is supplied unchanged. Receive one request DTO
// from peer, then Send zero or more SSEEvent values. Normal handler completion
// becomes EOF; application terminal state belongs in an event DTO.
type SSEHandler[Request, Event any] func(
	context.Context,
	string,
	contract.Stream[SSEEvent[Event], Request],
) error

// NewSSEHandler creates an HTTP handler for a single POST request whose response
// is an SSE stream. Mount the returned handler through a classified Registry
// method such as MountAuthenticated or MountPermission when strict security is
// enabled. Use a path-only route pattern (for example, "/events") if method
// errors should use problem+json; a method-qualified ServeMux pattern may reject
// the request before this handler runs. This handler does not implement
// browser-native EventSource or replay. Streaming APIs remain experimental
// until explicitly promoted under ADR-0047.
func NewSSEHandler[Request, Event any](
	cfg SSEConfig,
	handler SSEHandler[Request, Event],
) (http.Handler, error) {
	if err := validateSSEConfig(cfg); err != nil {
		return nil, err
	}
	if handler == nil {
		return nil, apperr.InvalidArgument("httpserver SSE: handler must not be nil")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveSSE(w, r, cfg, handler)
	}), nil
}

func validateSSEConfig(cfg SSEConfig) error {
	switch {
	case strings.TrimSpace(cfg.System) == "":
		return apperr.InvalidArgument("httpserver SSE: System must not be empty")
	case strings.TrimSpace(cfg.Method) == "":
		return apperr.InvalidArgument("httpserver SSE: Method must not be empty")
	case cfg.Stream.MaxDuration <= 0:
		return apperr.InvalidArgument("httpserver SSE: Stream.MaxDuration must be positive")
	case cfg.Stream.IdleTimeout < 0:
		return apperr.InvalidArgument("httpserver SSE: Stream.IdleTimeout must not be negative")
	case cfg.Stream.CloseTimeout <= 0:
		return apperr.InvalidArgument("httpserver SSE: Stream.CloseTimeout must be positive")
	case cfg.Stream.QueueSize < 0:
		return apperr.InvalidArgument("httpserver SSE: Stream.QueueSize must not be negative")
	case cfg.MaxRequestBodyBytes <= 0:
		return apperr.InvalidArgument("httpserver SSE: MaxRequestBodyBytes must be positive")
	case cfg.WriteTimeout <= 0:
		return apperr.InvalidArgument("httpserver SSE: WriteTimeout must be positive")
	case cfg.HeartbeatInterval < 0:
		return apperr.InvalidArgument("httpserver SSE: HeartbeatInterval must not be negative")
	default:
		return nil
	}
}

func serveSSE[Request, Event any](
	w http.ResponseWriter,
	r *http.Request,
	cfg SSEConfig,
	handler SSEHandler[Request, Event],
) {
	controller := http.NewResponseController(w)
	// net/http starts the server-wide WriteTimeout when it reads request headers.
	// Clear it for this route before validation/body reads; every response below
	// installs a bounded write deadline and flushes before clearing that deadline.
	if err := controller.SetWriteDeadline(time.Time{}); err != nil {
		writeSSEProblem(w, controller, cfg.WriteTimeout, apperr.Unavailable(err))
		return
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeSSEProblem(w, controller, cfg.WriteTimeout,
			apperr.New(apperr.CodeInvalidArgument, http.StatusMethodNotAllowed, "method not allowed"))
		return
	}

	if !isJSONRequest(r.Header.Get("Content-Type")) {
		writeSSEProblem(w, controller, cfg.WriteTimeout,
			apperr.New(apperr.CodeInvalidArgument, http.StatusUnsupportedMediaType, "content type must be JSON"))
		return
	}
	request, err := decodeSSERequest[Request](w, r, cfg.MaxRequestBodyBytes)
	if err != nil {
		writeSSEProblem(w, controller, cfg.WriteTimeout, err)
		return
	}

	stream, err := contract.OpenLocal[Request, SSEEvent[Event]](
		r.Context(), cfg.System, cfg.Method, cfg.Stream,
		func(ctx context.Context, peer contract.Stream[SSEEvent[Event], Request]) (handlerErr error) {
			defer func() {
				if recover() != nil {
					// This callback runs in OpenLocal's producer goroutine, outside
					// net/http's Recover middleware. Keep panic details off the wire.
					handlerErr = apperr.Internal(nil)
				}
			}()
			return handler(ctx, r.Header.Get(lastEventIDKey), peer)
		},
	)
	if err != nil {
		writeSSEProblem(w, controller, cfg.WriteTimeout, err)
		return
	}
	defer func() { _ = stream.Close() }()

	if err := stream.Send(r.Context(), request); err != nil {
		if r.Context().Err() == nil {
			writeSSEProblem(w, controller, cfg.WriteTimeout, err)
		}
		return
	}
	// A fast producer may finish before this half-close races through. Recv is
	// authoritative and drains accepted events before returning the terminal.
	_ = stream.CloseSend(r.Context())

	type recvResult struct {
		event SSEEvent[Event]
		err   error
	}
	received := make(chan recvResult, 1)
	stopRecv := make(chan struct{})
	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		for {
			event, err := stream.Recv(r.Context())
			select {
			case received <- recvResult{event: event, err: err}:
			case <-stopRecv:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		close(stopRecv)
		_ = stream.Close()
		<-recvDone
	}()

	var heartbeat *time.Ticker
	var heartbeatC <-chan time.Time
	if cfg.HeartbeatInterval > 0 {
		heartbeat = time.NewTicker(cfg.HeartbeatInterval)
		heartbeatC = heartbeat.C
		defer heartbeat.Stop()
	}

	committed := false
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeatC:
			if err := writeSSEFrame(w, controller, cfg.WriteTimeout, ": keep-alive\n\n", &committed); err != nil {
				if !committed {
					writeSSEProblem(w, controller, cfg.WriteTimeout, err)
				}
				return
			}
		case result := <-received:
			if result.err != nil {
				if r.Context().Err() != nil {
					return
				}
				if errors.Is(result.err, io.EOF) {
					if !committed {
						wasCommitted, err := writeEmptySSE(w, controller, cfg.WriteTimeout)
						if err != nil && !wasCommitted {
							writeSSEProblem(w, controller, cfg.WriteTimeout, err)
						}
					}
					return
				}
				if !committed {
					writeSSEProblem(w, controller, cfg.WriteTimeout, result.err)
				} else {
					_ = writeSSEError(w, controller, cfg.WriteTimeout, result.err)
				}
				return
			}

			frame, err := marshalSSEEvent(result.event)
			if err != nil {
				if !committed {
					writeSSEProblem(w, controller, cfg.WriteTimeout, err)
				} else {
					_ = writeSSEError(w, controller, cfg.WriteTimeout, err)
				}
				return
			}
			if err := writeSSEFrame(w, controller, cfg.WriteTimeout, frame, &committed); err != nil {
				if !committed {
					writeSSEProblem(w, controller, cfg.WriteTimeout, err)
				}
				return
			}
		}
	}
}

func isJSONRequest(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

func decodeSSERequest[T any](w http.ResponseWriter, r *http.Request, maxBytes int64) (T, error) {
	var request T
	body := http.MaxBytesReader(w, r.Body, maxBytes)
	decoder := json.NewDecoder(body)
	if err := decoder.Decode(&request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return request, apperr.New(apperr.CodeInvalidArgument, http.StatusRequestEntityTooLarge, "request body too large")
		}
		return request, apperr.InvalidArgument("invalid JSON request")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return request, apperr.New(apperr.CodeInvalidArgument, http.StatusRequestEntityTooLarge, "request body too large")
		}
		return request, apperr.InvalidArgument("request body must contain exactly one JSON value")
	}
	return request, nil
}

func marshalSSEEvent[T any](event SSEEvent[T]) (string, error) {
	if strings.ContainsAny(event.ID, "\x00\r\n") {
		return "", apperr.Internal(errors.New("invalid SSE event cursor"))
	}
	data, err := json.Marshal(event.Data)
	if err != nil {
		return "", apperr.Internal(err)
	}
	var frame strings.Builder
	if event.ID != "" {
		frame.WriteString("id: ")
		frame.WriteString(event.ID)
		frame.WriteByte('\n')
	}
	frame.WriteString("data: ")
	frame.Write(data)
	frame.WriteString("\n\n")
	return frame.String(), nil
}

func commitSSE(w http.ResponseWriter) {
	header := w.Header()
	header.Set("Content-Type", sseMediaType)
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Accel-Buffering", "no")
	header.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
}

func writeSSEFrame(w http.ResponseWriter, controller *http.ResponseController, timeout time.Duration, frame string, committed *bool) error {
	if err := controller.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	if committed != nil && !*committed {
		commitSSE(w)
		*committed = true
	}
	if _, err := io.WriteString(w, frame); err != nil {
		return err
	}
	if err := controller.Flush(); err != nil {
		return err
	}
	return controller.SetWriteDeadline(time.Time{})
}

func writeEmptySSE(w http.ResponseWriter, controller *http.ResponseController, timeout time.Duration) (bool, error) {
	if err := controller.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return false, err
	}
	commitSSE(w)
	if err := controller.Flush(); err != nil {
		return true, err
	}
	return true, controller.SetWriteDeadline(time.Time{})
}

func writeSSEError(w http.ResponseWriter, controller *http.ResponseController, timeout time.Duration, err error) error {
	e := apperr.From(err)
	payload, _ := json.Marshal(map[string]string{"code": e.Code()})
	frame := fmt.Sprintf("event: %s\ndata: %s\n\n", sseErrorEvent, payload)
	return writeSSEFrame(w, controller, timeout, frame, nil)
}

func writeSSEProblem(w http.ResponseWriter, controller *http.ResponseController, timeout time.Duration, err error) {
	if controller != nil {
		_ = controller.SetWriteDeadline(time.Now().Add(timeout))
	}
	apperr.WriteProblem(w, err)
	if controller != nil {
		// Problem responses are also subject to the route's write budget. Flush
		// before clearing it so net/http's final handler flush is not unbounded.
		_ = controller.Flush()
		_ = controller.SetWriteDeadline(time.Time{})
	}
}
