package httpserver_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/contract"
	"github.com/forgeplex/appkit/httpserver"
)

type sseTestRequest struct{ Text string }
type sseTestReply struct{ Text string }

type sseFrame struct {
	event string
	id    string
	data  string
}

func sseTestConfig() httpserver.SSEConfig {
	return httpserver.SSEConfig{
		System: "sse-test",
		Method: "Events",
		Stream: contract.StreamConfig{
			MaxDuration:  3 * time.Second,
			CloseTimeout: time.Second,
			QueueSize:    1,
		},
		MaxRequestBodyBytes: 1024,
		WriteTimeout:        time.Second,
	}
}

func makeSSEHandler(
	t *testing.T,
	cfg httpserver.SSEConfig,
	handler httpserver.SSEHandler[sseTestRequest, sseTestReply],
) http.Handler {
	t.Helper()
	h, err := httpserver.NewSSEHandler[sseTestRequest, sseTestReply](cfg, handler)
	if err != nil {
		t.Fatalf("NewSSEHandler: %v", err)
	}
	return h
}

func newSSETestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func postSSERequest(t *testing.T, client *http.Client, url, body, cursor string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cursor != "" {
		req.Header.Set("Last-Event-ID", cursor)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST SSE: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func readSSEFrame(reader *bufio.Reader) (sseFrame, error) {
	var frame sseFrame
	active := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return frame, err
		}
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			if active {
				return frame, nil
			}
			continue
		}
		active = true
		switch {
		case strings.HasPrefix(line, "event: "):
			frame.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			frame.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			frame.data = strings.TrimPrefix(line, "data: ")
		default:
			if strings.HasPrefix(line, ":") {
				frame.event = "comment"
				frame.data = strings.TrimPrefix(line, ": ")
			}
		}
	}
}

func readSSEFrames(t *testing.T, body io.Reader) []sseFrame {
	t.Helper()
	reader := bufio.NewReader(body)
	var frames []sseFrame
	for {
		frame, err := readSSEFrame(reader)
		if errors.Is(err, io.EOF) {
			return frames
		}
		if err != nil {
			t.Fatalf("read SSE frame: %v", err)
		}
		frames = append(frames, frame)
	}
}

func TestSSEHandlerStreamsJSONAndOpaqueCursor(t *testing.T) {
	cfg := sseTestConfig()
	cursorSeen := make(chan string, 1)
	handler := makeSSEHandler(t, cfg, func(ctx context.Context, cursor string, peer contract.Stream[httpserver.SSEEvent[sseTestReply], sseTestRequest]) error {
		cursorSeen <- cursor
		request, err := peer.Recv(ctx)
		if err != nil {
			return err
		}
		if request.Text != "hello" {
			return apperr.InvalidArgument("unexpected request")
		}
		for _, text := range []string{"one", "two"} {
			if err := peer.Send(ctx, httpserver.SSEEvent[sseTestReply]{
				ID:   text + "/cursor",
				Data: sseTestReply{Text: text},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	server := newSSETestServer(t, handler)
	cursor := "opaque:+/part-1"
	resp := postSSERequest(t, &http.Client{Timeout: 4 * time.Second}, server.URL, "{\"Text\":\"hello\"}", cursor)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", got)
	}
	if got := <-cursorSeen; got != cursor {
		t.Fatalf("Last-Event-ID = %q, want unchanged %q", got, cursor)
	}
	frames := readSSEFrames(t, resp.Body)
	if len(frames) != 2 {
		t.Fatalf("frames = %+v, want two events", frames)
	}
	for i, want := range []string{"one", "two"} {
		if frames[i].id != want+"/cursor" {
			t.Errorf("frame[%d] id = %q", i, frames[i].id)
		}
		var got sseTestReply
		if err := json.Unmarshal([]byte(frames[i].data), &got); err != nil {
			t.Fatalf("frame[%d] JSON: %v", i, err)
		}
		if got.Text != want {
			t.Errorf("frame[%d] data = %+v, want %q", i, got, want)
		}
	}
}

func TestSSEHandlerFlushesBeforeProducerCompletes(t *testing.T) {
	release := make(chan struct{})
	var released atomic.Bool
	releaseHandler := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}
	t.Cleanup(releaseHandler)

	handler := makeSSEHandler(t, sseTestConfig(), func(ctx context.Context, _ string, peer contract.Stream[httpserver.SSEEvent[sseTestReply], sseTestRequest]) error {
		if _, err := peer.Recv(ctx); err != nil {
			return err
		}
		if err := peer.Send(ctx, httpserver.SSEEvent[sseTestReply]{Data: sseTestReply{Text: "first"}}); err != nil {
			return err
		}
		<-release
		return peer.Send(ctx, httpserver.SSEEvent[sseTestReply]{Data: sseTestReply{Text: "second"}})
	})
	var wrapped http.Handler = handler
	middleware := httpserver.Base(slog.New(slog.NewTextHandler(io.Discard, nil)))
	for i := len(middleware) - 1; i >= 0; i-- {
		wrapped = middleware[i](wrapped)
	}
	server := newSSETestServer(t, wrapped)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader("{\"Text\":\"hello\"}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	respCh := make(chan struct {
		resp *http.Response
		err  error
	}, 1)
	go func() {
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
		respCh <- struct {
			resp *http.Response
			err  error
		}{resp, err}
	}()
	var resp *http.Response
	select {
	case result := <-respCh:
		if result.err != nil {
			releaseHandler()
			t.Fatalf("POST SSE: %v", result.err)
		}
		resp = result.resp
	case <-time.After(time.Second):
		releaseHandler()
		cancel()
		t.Fatal("response headers were not flushed while the producer was still active")
	}
	defer resp.Body.Close()

	firstFrame := make(chan struct {
		frame sseFrame
		err   error
	}, 1)
	reader := bufio.NewReader(resp.Body)
	go func() {
		frame, err := readSSEFrame(reader)
		firstFrame <- struct {
			frame sseFrame
			err   error
		}{frame, err}
	}()
	select {
	case got := <-firstFrame:
		if got.err != nil {
			releaseHandler()
			t.Fatalf("first SSE frame: %v", got.err)
		}
		var payload sseTestReply
		if err := json.Unmarshal([]byte(got.frame.data), &payload); err != nil || payload.Text != "first" {
			releaseHandler()
			t.Fatalf("first frame = %+v (%v)", got.frame, err)
		}
	case <-time.After(time.Second):
		releaseHandler()
		t.Fatal("first event was not flushed before producer completion")
	}

	releaseHandler()
	second, err := readSSEFrame(reader)
	if err != nil {
		t.Fatalf("second SSE frame: %v", err)
	}
	var payload sseTestReply
	if err := json.Unmarshal([]byte(second.data), &payload); err != nil || payload.Text != "second" {
		t.Fatalf("second frame = %+v (%v)", second, err)
	}
}

type blockingSSEWriter struct {
	header       http.Header
	writeStarted chan struct{}
	releaseWrite chan struct{}
	startOnce    sync.Once
	releaseOnce  sync.Once
}

func newBlockingSSEWriter() *blockingSSEWriter {
	return &blockingSSEWriter{
		header:       make(http.Header),
		writeStarted: make(chan struct{}),
		releaseWrite: make(chan struct{}),
	}
}

func (w *blockingSSEWriter) Header() http.Header              { return w.header }
func (w *blockingSSEWriter) WriteHeader(int)                  {}
func (w *blockingSSEWriter) Flush()                           {}
func (w *blockingSSEWriter) SetWriteDeadline(time.Time) error { return nil }
func (w *blockingSSEWriter) release() {
	w.releaseOnce.Do(func() { close(w.releaseWrite) })
}
func (w *blockingSSEWriter) Write(p []byte) (int, error) {
	w.startOnce.Do(func() { close(w.writeStarted) })
	<-w.releaseWrite
	return len(p), nil
}

func TestSSESlowWriterBackpressuresAndCancellationUnblocksProducer(t *testing.T) {
	cfg := sseTestConfig()
	cfg.Stream.QueueSize = 1
	var attempted atomic.Int32
	var sent atomic.Int32
	producerDone := make(chan error, 1)
	handler := makeSSEHandler(t, cfg, func(ctx context.Context, _ string, peer contract.Stream[httpserver.SSEEvent[sseTestReply], sseTestRequest]) error {
		if _, err := peer.Recv(ctx); err != nil {
			producerDone <- err
			return err
		}
		for i := 0; i < 16; i++ {
			attempted.Add(1)
			err := peer.Send(ctx, httpserver.SSEEvent[sseTestReply]{Data: sseTestReply{Text: "event"}})
			if err != nil {
				producerDone <- err
				return err
			}
			sent.Add(1)
		}
		producerDone <- nil
		return nil
	})

	writer := newBlockingSSEWriter()
	t.Cleanup(writer.release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/sse", strings.NewReader("{\"Text\":\"hello\"}")).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	served := make(chan struct{})
	go func() {
		handler.ServeHTTP(writer, req)
		close(served)
	}()

	select {
	case <-writer.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("SSE response did not reach the blocked client writer")
	}
	select {
	case err := <-producerDone:
		t.Fatalf("producer unexpectedly finished while the client was blocked: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if gotAttempted, gotSent := attempted.Load(), sent.Load(); gotAttempted <= gotSent {
		t.Fatalf("no blocked stream Send observed: attempted=%d sent=%d", gotAttempted, gotSent)
	}
	if got := sent.Load(); got >= 16 {
		t.Fatalf("producer sent all events despite blocked client; sent=%d", got)
	}

	cancel()
	writer.release()
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE handler did not exit after cancellation and write release")
	}
	select {
	case err := <-producerDone:
		if err == nil {
			t.Fatal("producer completed normally after request cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("producer did not observe request cancellation")
	}
}

func TestSSEClientDisconnectCancelsProducer(t *testing.T) {
	producerCanceled := make(chan struct{})
	handler := makeSSEHandler(t, sseTestConfig(), func(ctx context.Context, _ string, peer contract.Stream[httpserver.SSEEvent[sseTestReply], sseTestRequest]) error {
		if _, err := peer.Recv(ctx); err != nil {
			return err
		}
		if err := peer.Send(ctx, httpserver.SSEEvent[sseTestReply]{Data: sseTestReply{Text: "first"}}); err != nil {
			return err
		}
		<-ctx.Done()
		close(producerCanceled)
		return ctx.Err()
	})
	server := newSSETestServer(t, handler)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader("{\"Text\":\"hello\"}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST SSE: %v", err)
	}
	reader := bufio.NewReader(resp.Body)
	if _, err := readSSEFrame(reader); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("read first event before disconnect: %v", err)
	}
	_ = resp.Body.Close()
	select {
	case <-producerCanceled:
	case <-time.After(time.Second):
		t.Fatal("closing the SSE response did not cancel its producer")
	}
}

func TestSSEHandlerUsesPerFrameDeadlineInsteadOfServerWriteTimeout(t *testing.T) {
	cfg := sseTestConfig()
	cfg.WriteTimeout = 300 * time.Millisecond
	handler := makeSSEHandler(t, cfg, func(ctx context.Context, _ string, peer contract.Stream[httpserver.SSEEvent[sseTestReply], sseTestRequest]) error {
		if _, err := peer.Recv(ctx); err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
		for _, text := range []string{"late-one", "late-two"} {
			if err := peer.Send(ctx, httpserver.SSEEvent[sseTestReply]{Data: sseTestReply{Text: text}}); err != nil {
				return err
			}
			time.Sleep(70 * time.Millisecond)
		}
		return nil
	})
	server := httptest.NewUnstartedServer(handler)
	server.Config.WriteTimeout = 30 * time.Millisecond
	server.Start()
	t.Cleanup(server.Close)

	resp := postSSERequest(t, &http.Client{Timeout: 3 * time.Second}, server.URL, "{\"Text\":\"hello\"}", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	frames := readSSEFrames(t, resp.Body)
	if len(frames) != 2 {
		t.Fatalf("frames = %+v, want two events despite server WriteTimeout", frames)
	}
	if server.Config.WriteTimeout != 30*time.Millisecond {
		t.Fatalf("ordinary server WriteTimeout changed to %s", server.Config.WriteTimeout)
	}
}

func TestSSEHandlerUsesProblemBeforeCommitAndSanitizedErrorAfterCommit(t *testing.T) {
	t.Run("before commit", func(t *testing.T) {
		handler := makeSSEHandler(t, sseTestConfig(), func(ctx context.Context, _ string, peer contract.Stream[httpserver.SSEEvent[sseTestReply], sseTestRequest]) error {
			if _, err := peer.Recv(ctx); err != nil {
				return err
			}
			return apperr.Conflict("safe conflict").WithCause(errors.New("private-cause"))
		})
		server := newSSETestServer(t, handler)
		resp := postSSERequest(t, &http.Client{Timeout: 3 * time.Second}, server.URL, "{\"Text\":\"hello\"}", "")
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
		if got := resp.Header.Get("Content-Type"); got != "application/problem+json" {
			t.Fatalf("Content-Type = %q", got)
		}
		if !strings.Contains(string(body), "\"code\":\"CONFLICT\"") || strings.Contains(string(body), "private-cause") {
			t.Fatalf("problem body leaked or lost code: %s", body)
		}
	})

	t.Run("after commit", func(t *testing.T) {
		handler := makeSSEHandler(t, sseTestConfig(), func(ctx context.Context, _ string, peer contract.Stream[httpserver.SSEEvent[sseTestReply], sseTestRequest]) error {
			if _, err := peer.Recv(ctx); err != nil {
				return err
			}
			if err := peer.Send(ctx, httpserver.SSEEvent[sseTestReply]{Data: sseTestReply{Text: "event"}}); err != nil {
				return err
			}
			return errors.New("PRIVATE-TRANSPORT-DETAIL")
		})
		server := newSSETestServer(t, handler)
		resp := postSSERequest(t, &http.Client{Timeout: 3 * time.Second}, server.URL, "{\"Text\":\"hello\"}", "")
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "event: error") ||
			!strings.Contains(string(body), "\"code\":\"INTERNAL\"") ||
			strings.Contains(string(body), "PRIVATE-TRANSPORT-DETAIL") {
			t.Fatalf("SSE terminal frame is missing or leaked detail: %s", body)
		}
	})
}

func TestSSEOutputCursorCannotInjectFrames(t *testing.T) {
	handler := makeSSEHandler(t, sseTestConfig(), func(ctx context.Context, _ string, peer contract.Stream[httpserver.SSEEvent[sseTestReply], sseTestRequest]) error {
		if _, err := peer.Recv(ctx); err != nil {
			return err
		}
		if err := peer.Send(ctx, httpserver.SSEEvent[sseTestReply]{Data: sseTestReply{Text: "first"}}); err != nil {
			return err
		}
		return peer.Send(ctx, httpserver.SSEEvent[sseTestReply]{
			ID:   "bad\n\nevent: injected",
			Data: sseTestReply{Text: "must-not-appear"},
		})
	})
	server := newSSETestServer(t, handler)
	resp := postSSERequest(t, &http.Client{Timeout: 3 * time.Second}, server.URL, "{\"Text\":\"hello\"}", "")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "event: injected") || strings.Contains(string(body), "must-not-appear") {
		t.Fatalf("invalid cursor injected an SSE frame: %s", body)
	}
	if !strings.Contains(string(body), "event: error") || !strings.Contains(string(body), `"code":"INTERNAL"`) {
		t.Fatalf("invalid application cursor did not produce sanitized terminal code: %s", body)
	}
}

func TestSSEHandlerRecoversProducerPanicAsSanitizedError(t *testing.T) {
	handler := makeSSEHandler(t, sseTestConfig(), func(ctx context.Context, _ string, peer contract.Stream[httpserver.SSEEvent[sseTestReply], sseTestRequest]) error {
		if _, err := peer.Recv(ctx); err != nil {
			return err
		}
		if err := peer.Send(ctx, httpserver.SSEEvent[sseTestReply]{Data: sseTestReply{Text: "first"}}); err != nil {
			return err
		}
		panic("PRIVATE-PANIC-DETAIL")
	})
	server := newSSETestServer(t, handler)
	resp := postSSERequest(t, &http.Client{Timeout: 3 * time.Second}, server.URL, "{\"Text\":\"hello\"}", "")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "event: error") ||
		!strings.Contains(string(body), `"code":"INTERNAL"`) ||
		strings.Contains(string(body), "PRIVATE-PANIC-DETAIL") {
		t.Fatalf("producer panic was not converted to a sanitized terminal: %s", body)
	}
}

func TestSSEHeartbeatDoesNotResetApplicationIdleTimeout(t *testing.T) {
	cfg := sseTestConfig()
	cfg.HeartbeatInterval = 10 * time.Millisecond
	cfg.Stream.IdleTimeout = 90 * time.Millisecond
	handler := makeSSEHandler(t, cfg, func(ctx context.Context, _ string, peer contract.Stream[httpserver.SSEEvent[sseTestReply], sseTestRequest]) error {
		if _, err := peer.Recv(ctx); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	})
	server := newSSETestServer(t, handler)
	resp := postSSERequest(t, &http.Client{Timeout: 3 * time.Second}, server.URL, "{\"Text\":\"hello\"}", "")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), ": keep-alive") ||
		!strings.Contains(string(body), "event: error") ||
		!strings.Contains(string(body), "\"code\":\"UNAVAILABLE\"") {
		t.Fatalf("heartbeats kept an idle application stream alive or terminal error was lost: %s", body)
	}
}

func TestSSEHandlerValidatesMethodContentTypeJSONAndBodyLimit(t *testing.T) {
	cfg := sseTestConfig()
	cfg.MaxRequestBodyBytes = 12
	var called atomic.Int32
	handler := makeSSEHandler(t, cfg, func(context.Context, string, contract.Stream[httpserver.SSEEvent[sseTestReply], sseTestRequest]) error {
		called.Add(1)
		return nil
	})
	server := newSSETestServer(t, handler)
	client := &http.Client{Timeout: 3 * time.Second}
	cases := []struct {
		name        string
		method      string
		contentType string
		body        string
		status      int
	}{
		{name: "method", method: http.MethodGet, contentType: "application/json", body: "{\"Text\":\"x\"}", status: http.StatusMethodNotAllowed},
		{name: "content type", method: http.MethodPost, contentType: "text/plain", body: "{\"Text\":\"x\"}", status: http.StatusUnsupportedMediaType},
		{name: "invalid JSON", method: http.MethodPost, contentType: "application/json", body: "{", status: http.StatusUnprocessableEntity},
		{name: "multiple JSON values", method: http.MethodPost, contentType: "application/json", body: "{} {}", status: http.StatusUnprocessableEntity},
		{name: "body limit", method: http.MethodPost, contentType: "application/json", body: "{\"Text\":\"too-large\"}", status: http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, server.URL, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", tc.contentType)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, tc.status, body)
			}
			if got := resp.Header.Get("Content-Type"); got != "application/problem+json" {
				t.Fatalf("Content-Type = %q", got)
			}
		})
	}
	if got := called.Load(); got != 0 {
		t.Fatalf("application handler called %d times for invalid requests", got)
	}
}

func TestSSEPathOnlyRouteReturnsProblemForUnsupportedMethod(t *testing.T) {
	handler := makeSSEHandler(t, sseTestConfig(), func(context.Context, string, contract.Stream[httpserver.SSEEvent[sseTestReply], sseTestRequest]) error {
		return nil
	})
	_, baseURL := startSSEApp(t, func(reg *appkit.Registry, h http.Handler) {
		reg.MountPublic("/sse", h)
	}, handler, time.Second)
	req, err := http.NewRequest(http.MethodGet, baseURL+"/sse", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET SSE route: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := resp.Header.Get("Allow"); got != http.MethodPost {
		t.Fatalf("Allow = %q, want POST", got)
	}
}

type sseRouteMount func(*appkit.Registry, http.Handler)

func startSSEApp(t *testing.T, route sseRouteMount, handler http.Handler, shutdownTimeout time.Duration) (*appkit.RunningApp, string) {
	return startSSEAppWithSecurity(t, appkit.SecurityUserFacing, route, handler, shutdownTimeout)
}

func startSSEAppWithSecurity(
	t *testing.T,
	mode appkit.SecurityMode,
	route sseRouteMount,
	handler http.Handler,
	shutdownTimeout time.Duration,
	trustedMiddleware ...func(http.Handler) http.Handler,
) (*appkit.RunningApp, string) {
	t.Helper()
	address := make(chan string, 1)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	module := appkit.ModuleFunc("sse-test", func(reg *appkit.Registry) error {
		route(reg, handler)
		return nil
	})
	options := []appkit.Option{
		appkit.Security(mode),
		appkit.HTTPAddr("127.0.0.1:0"),
		appkit.Logger(logger),
		appkit.ShutdownTimeout(shutdownTimeout),
		appkit.Middleware(httpserver.Base(logger)...),
		appkit.HTTPServer(func(server *http.Server) {
			server.BaseContext = func(listener net.Listener) context.Context {
				address <- "http://" + listener.Addr().String()
				return context.Background()
			}
		}),
	}
	if len(trustedMiddleware) > 0 {
		options = append(options, appkit.Middleware(trustedMiddleware...))
	}
	app := appkit.New([]appkit.Module{module}, options...)
	parent, cancel := context.WithCancel(context.Background())
	host, err := app.Start(parent)
	if err != nil {
		cancel()
		t.Fatalf("App.Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		waited := make(chan struct{})
		go func() {
			_ = host.Wait()
			close(waited)
		}()
		select {
		case <-waited:
		case <-time.After(3 * time.Second):
			t.Error("App did not stop during test cleanup")
		}
	})
	select {
	case addr := <-address:
		return host, addr
	case <-time.After(time.Second):
		t.Fatal("HTTP listener did not publish its address")
		return nil, ""
	}
}

func TestSSEHandlerRespectsStrictRouteClassifications(t *testing.T) {
	stream := makeSSEHandler(t, sseTestConfig(), func(ctx context.Context, _ string, peer contract.Stream[httpserver.SSEEvent[sseTestReply], sseTestRequest]) error {
		_, err := peer.Recv(ctx)
		return err
	})
	actorMiddleware := func(perms ...string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := appkit.WithActor(r.Context(), appkit.Actor{UserID: "test-user", Perms: perms})
				next.ServeHTTP(w, r.WithContext(ctx))
			})
		}
	}
	serviceMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := appkit.WithServicePrincipal(r.Context(), appkit.ServicePrincipal{Subject: "test-service"})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	tests := []struct {
		name       string
		mode       appkit.SecurityMode
		mount      sseRouteMount
		middleware []func(http.Handler) http.Handler
		wantStatus int
	}{
		{
			name: "public route allows anonymous request", mode: appkit.SecurityUserFacing,
			mount:      func(reg *appkit.Registry, h http.Handler) { reg.MountPublic("POST /sse", h) },
			wantStatus: http.StatusOK,
		},
		{
			name: "authenticated route denies missing actor", mode: appkit.SecurityUserFacing,
			mount:      func(reg *appkit.Registry, h http.Handler) { reg.MountAuthenticated("POST /sse", h) },
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "authenticated route accepts verified actor", mode: appkit.SecurityUserFacing,
			mount:      func(reg *appkit.Registry, h http.Handler) { reg.MountAuthenticated("POST /sse", h) },
			middleware: []func(http.Handler) http.Handler{actorMiddleware()},
			wantStatus: http.StatusOK,
		},
		{
			name: "permission route denies missing actor", mode: appkit.SecurityUserFacing,
			mount: func(reg *appkit.Registry, h http.Handler) {
				reg.Permissions(appkit.PermissionDecl{Code: "stream:read"})
				reg.MountPermission("POST /sse", "stream:read", h)
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "permission route denies missing permission", mode: appkit.SecurityUserFacing,
			mount: func(reg *appkit.Registry, h http.Handler) {
				reg.Permissions(appkit.PermissionDecl{Code: "stream:read"})
				reg.MountPermission("POST /sse", "stream:read", h)
			},
			middleware: []func(http.Handler) http.Handler{actorMiddleware()},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "permission route accepts authorized actor", mode: appkit.SecurityUserFacing,
			mount: func(reg *appkit.Registry, h http.Handler) {
				reg.Permissions(appkit.PermissionDecl{Code: "stream:read"})
				reg.MountPermission("POST /sse", "stream:read", h)
			},
			middleware: []func(http.Handler) http.Handler{actorMiddleware("stream:read")},
			wantStatus: http.StatusOK,
		},
		{
			name: "internal route denies missing service", mode: appkit.SecurityInternalService,
			mount:      func(reg *appkit.Registry, h http.Handler) { reg.MountInternalService("POST /sse", h) },
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "internal route accepts verified service", mode: appkit.SecurityInternalService,
			mount:      func(reg *appkit.Registry, h http.Handler) { reg.MountInternalService("POST /sse", h) },
			middleware: []func(http.Handler) http.Handler{serviceMiddleware},
			wantStatus: http.StatusOK,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, baseURL := startSSEAppWithSecurity(t, tc.mode, tc.mount, stream, time.Second, tc.middleware...)
			resp := postSSERequest(t, &http.Client{Timeout: 3 * time.Second}, baseURL+"/sse", "{\"Text\":\"hello\"}", "")
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, tc.wantStatus, body)
			}
			if tc.wantStatus < http.StatusBadRequest && resp.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" {
				t.Fatalf("successful route Content-Type = %q", resp.Header.Get("Content-Type"))
			}
			if tc.wantStatus >= http.StatusBadRequest && resp.Header.Get("Content-Type") != "application/problem+json" {
				t.Fatalf("denied route Content-Type = %q", resp.Header.Get("Content-Type"))
			}
		})
	}
}

func TestSSEStrictSecurityRejectsUnclassifiedAndIncompatibleRoutes(t *testing.T) {
	handler := makeSSEHandler(t, sseTestConfig(), func(context.Context, string, contract.Stream[httpserver.SSEEvent[sseTestReply], sseTestRequest]) error {
		return nil
	})
	tests := []struct {
		name  string
		mode  appkit.SecurityMode
		mount sseRouteMount
		want  string
	}{
		{
			name: "unclassified route", mode: appkit.SecurityUserFacing,
			mount: func(reg *appkit.Registry, h http.Handler) { reg.Mount("POST /sse", h) },
			want:  "未声明安全分类",
		},
		{
			name: "internal route in user-facing mode", mode: appkit.SecurityUserFacing,
			mount: func(reg *appkit.Registry, h http.Handler) { reg.MountInternalService("POST /sse", h) },
			want:  "InternalService",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			module := appkit.ModuleFunc("sse-security-test", func(reg *appkit.Registry) error {
				tc.mount(reg, handler)
				return nil
			})
			app := appkit.New([]appkit.Module{module}, appkit.Security(tc.mode), appkit.HTTPAddr("127.0.0.1:0"))
			_, err := app.Start(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("App.Start error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestSSEStreamDrainsDuringHostShutdown(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	cancelled := make(chan struct{}, 1)
	var startedCount atomic.Int32
	cfg := sseTestConfig()
	cfg.HeartbeatInterval = 15 * time.Millisecond
	handler := makeSSEHandler(t, cfg, func(ctx context.Context, _ string, peer contract.Stream[httpserver.SSEEvent[sseTestReply], sseTestRequest]) error {
		if _, err := peer.Recv(ctx); err != nil {
			return err
		}
		startedCount.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return peer.Send(ctx, httpserver.SSEEvent[sseTestReply]{Data: sseTestReply{Text: "drained"}})
		case <-ctx.Done():
			cancelled <- struct{}{}
			return ctx.Err()
		}
	})
	host, baseURL := startSSEApp(t, func(reg *appkit.Registry, h http.Handler) {
		reg.MountPublic("POST /sse", h)
	}, handler, 2*time.Second)
	resp := postSSERequest(t, &http.Client{Timeout: 4 * time.Second}, baseURL+"/sse", "{\"Text\":\"hello\"}", "")
	defer resp.Body.Close()
	<-started

	shutdown := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		shutdown <- host.Shutdown(ctx)
	}()
	client := &http.Client{
		Timeout: 150 * time.Millisecond,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}
	deadline := time.Now().Add(time.Second)
	for {
		probeResp, probeErr := client.Get(baseURL + "/healthz")
		if probeErr != nil {
			break // Shutdown has closed the listener to new connections.
		}
		_ = probeResp.Body.Close()
		if time.Now().After(deadline) {
			t.Fatal("Host.Shutdown did not stop accepting new connections")
		}
		time.Sleep(10 * time.Millisecond)
	}
	secondReq, err := http.NewRequest(http.MethodPost, baseURL+"/sse", strings.NewReader("{\"Text\":\"second\"}"))
	if err != nil {
		t.Fatal(err)
	}
	secondReq.Header.Set("Content-Type", "application/json")
	if secondResp, err := client.Do(secondReq); err == nil {
		_ = secondResp.Body.Close()
		t.Fatal("Host.Shutdown accepted a new SSE stream")
	}
	if got := startedCount.Load(); got != 1 {
		t.Fatalf("SSE producer starts during shutdown = %d, want only the in-flight stream", got)
	}
	select {
	case <-cancelled:
		t.Fatal("Host cancellation reached an active stream before its drain budget")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatalf("read drained stream: %v", readErr)
	}
	if !strings.Contains(string(body), "\"Text\":\"drained\"") {
		t.Fatalf("active stream did not drain: %s", body)
	}
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatalf("Host.Shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Host shutdown did not complete after stream drained")
	}
}

func TestSSEStreamIsCancelledWhenHostShutdownBudgetExpires(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	cfg := sseTestConfig()
	cfg.HeartbeatInterval = 10 * time.Millisecond
	handler := makeSSEHandler(t, cfg, func(ctx context.Context, _ string, peer contract.Stream[httpserver.SSEEvent[sseTestReply], sseTestRequest]) error {
		if _, err := peer.Recv(ctx); err != nil {
			return err
		}
		close(started)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	})
	host, baseURL := startSSEApp(t, func(reg *appkit.Registry, h http.Handler) {
		reg.MountPublic("POST /sse", h)
	}, handler, 120*time.Millisecond)
	resp := postSSERequest(t, &http.Client{Timeout: 4 * time.Second}, baseURL+"/sse", "{\"Text\":\"hello\"}", "")
	defer resp.Body.Close()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := host.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Host.Shutdown error = %v, want expired stream drain budget", err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("forced HTTP close did not cancel the stream producer")
	}
}
