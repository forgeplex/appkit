package httpserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/contract"
	"github.com/forgeplex/appkit/httpserver"
	"github.com/forgeplex/appkit/tx"
)

var _ appkit.ManagedService = (*httpserver.WebSocketHub)(nil)

type testWireFrame struct {
	Type    string `json:"type"`
	Data    string `json:"data,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

func websocketTestConfig(hub *httpserver.WebSocketHub) httpserver.WebSocketConfig {
	return httpserver.WebSocketConfig{
		System: "websocket-test",
		Method: "Chat",
		Stream: contract.StreamConfig{
			MaxDuration:  10 * time.Second,
			IdleTimeout:  5 * time.Second,
			CloseTimeout: time.Second,
			QueueSize:    1,
		},
		MaxMessageBytes:  128,
		HandshakeTimeout: time.Second,
		WriteTimeout:     time.Second,
		Hub:              hub,
	}
}

func startWebSocketServer(
	t *testing.T,
	cfg httpserver.WebSocketConfig,
	identity httpserver.WebSocketIdentityResolver,
	handler httpserver.WebSocketHandler[string, string],
) (*httptest.Server, *httpserver.WebSocketHub) {
	return startWebSocketTestServer(t, cfg, identity, handler, false)
}

func startWebSocketTLSServer(
	t *testing.T,
	cfg httpserver.WebSocketConfig,
	identity httpserver.WebSocketIdentityResolver,
	handler httpserver.WebSocketHandler[string, string],
) (*httptest.Server, *httpserver.WebSocketHub) {
	return startWebSocketTestServer(t, cfg, identity, handler, true)
}

func startWebSocketTestServer(
	t *testing.T,
	cfg httpserver.WebSocketConfig,
	identity httpserver.WebSocketIdentityResolver,
	handler httpserver.WebSocketHandler[string, string],
	tls bool,
) (*httptest.Server, *httpserver.WebSocketHub) {
	t.Helper()
	h, err := httpserver.NewWebSocketHandler[string, string](cfg, identity, handler)
	if err != nil {
		t.Fatalf("NewWebSocketHandler: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	mux.Handle("/bidi", h)
	var root http.Handler = mux
	middleware := httpserver.Base(logger)
	for i := len(middleware) - 1; i >= 0; i-- {
		root = middleware[i](root)
	}
	var server *httptest.Server
	if tls {
		server = httptest.NewTLSServer(root)
	} else {
		server = httptest.NewServer(root)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = cfg.Hub.Close(ctx)
		server.Close()
	})
	return server, cfg.Hub
}

func startWebSocketHub(t *testing.T) *httpserver.WebSocketHub {
	t.Helper()
	hub := httpserver.NewWebSocketHub()
	if err := hub.Start(context.Background()); err != nil {
		t.Fatalf("Hub.Start: %v", err)
	}
	return hub
}

func dialTestWebSocket(t *testing.T, server *httptest.Server) *websocket.Conn {
	t.Helper()
	conn, response, err := websocket.Dial(context.Background(), server.URL+"/bidi", &websocket.DialOptions{
		HTTPClient:   server.Client(),
		Subprotocols: []string{"appkit.contract.bidi.v1"},
	})
	if err != nil {
		if response != nil && response.Body != nil {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("WebSocket Dial: %v; status=%d body=%s", err, response.StatusCode, body)
		}
		t.Fatalf("WebSocket Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func writeTestFrame(t *testing.T, conn *websocket.Conn, frame testWireFrame) {
	t.Helper()
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("write WebSocket frame: %v", err)
	}
}

func readTestFrame(t *testing.T, conn *websocket.Conn) testWireFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	typ, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read WebSocket frame: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("message type = %v, want text", typ)
	}
	var frame testWireFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("decode WebSocket frame: %v; raw=%s", err, raw)
	}
	return frame
}

func TestWebSocketBidiWorksThroughBaseMiddleware(t *testing.T) {
	hub := startWebSocketHub(t)
	cfg := websocketTestConfig(hub)
	server, _ := startWebSocketServer(t, cfg, func(context.Context) (httpserver.WebSocketIdentity, error) {
		return httpserver.WebSocketIdentity{Subject: "test-user"}, nil
	}, func(ctx context.Context, peer contract.Stream[string, string]) error {
		for {
			value, err := peer.Recv(ctx)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if err := peer.Send(ctx, strings.ToUpper(value)); err != nil {
				return err
			}
		}
	})
	conn := dialTestWebSocket(t, server)
	for _, value := range []string{"hello", "world", "again"} {
		writeTestFrame(t, conn, testWireFrame{Type: "data", Data: value})
	}
	for i, want := range []string{"HELLO", "WORLD", "AGAIN"} {
		if got := readTestFrame(t, conn); got.Type != "data" || got.Data != want {
			t.Fatalf("response frame[%d] = %+v, want %q", i, got, want)
		}
	}
	writeTestFrame(t, conn, testWireFrame{Type: "half_close"})
	if got := readTestFrame(t, conn); got.Type != "end" {
		t.Fatalf("terminal frame = %+v, want end", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusNormalClosure {
		t.Fatalf("close status = %d, error=%v; want normal closure", got, err)
	}
}

func TestWebSocketUpgradeFailuresStayProblemJSON(t *testing.T) {
	hub := startWebSocketHub(t)
	cfg := websocketTestConfig(hub)
	cfg.OriginPatterns = []string{"https://client.example.test"}
	server, _ := startWebSocketServer(t, cfg, func(context.Context) (httpserver.WebSocketIdentity, error) {
		return httpserver.WebSocketIdentity{}, apperr.Unauthenticated("no identity")
	}, func(context.Context, contract.Stream[string, string]) error { return nil })

	t.Run("allowlisted cross-origin accepted", func(t *testing.T) {
		allowlistedCfg := cfg
		allowlistedCfg.Hub = startWebSocketHub(t)
		allowlistedServer, _ := startWebSocketServer(t, allowlistedCfg, func(context.Context) (httpserver.WebSocketIdentity, error) {
			return httpserver.WebSocketIdentity{Subject: "browser-user"}, nil
		}, func(context.Context, contract.Stream[string, string]) error { return nil })
		conn, response, err := websocket.Dial(context.Background(), allowlistedServer.URL+"/bidi", &websocket.DialOptions{
			HTTPClient:   allowlistedServer.Client(),
			HTTPHeader:   http.Header{"Origin": []string{"https://client.example.test"}},
			Subprotocols: []string{"appkit.contract.bidi.v1"},
		})
		if err != nil {
			t.Fatalf("allowlisted WebSocket Dial failed: %v (response=%v)", err, response)
		}
		_ = conn.CloseNow()
	})

	t.Run("origin denied", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, server.URL+"/bidi", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", "https://evil.example.test")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Content-Type") != "application/problem+json" {
			t.Fatalf("response status/content-type = %d/%q", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	})

	t.Run("query rejected", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, server.URL+"/bidi?tenant=forged", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity || resp.Header.Get("Content-Type") != "application/problem+json" {
			t.Fatalf("response status/content-type = %d/%q", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	})

	t.Run("identity rejected before upgrade", func(t *testing.T) {
		conn, response, err := websocket.Dial(context.Background(), server.URL+"/bidi", &websocket.DialOptions{
			HTTPClient: server.Client(), Subprotocols: []string{"appkit.contract.bidi.v1"},
		})
		if err == nil {
			_ = conn.CloseNow()
			t.Fatal("Dial succeeded without identity")
		}
		if response == nil || response.StatusCode != http.StatusUnauthorized || response.Header.Get("Content-Type") != "application/problem+json" {
			t.Fatalf("response = %#v; error=%v", response, err)
		}
	})
	t.Run("empty subject rejected before upgrade", func(t *testing.T) {
		server, _ := startWebSocketServer(t, cfg, func(context.Context) (httpserver.WebSocketIdentity, error) {
			return httpserver.WebSocketIdentity{}, nil
		}, func(context.Context, contract.Stream[string, string]) error { return nil })
		conn, response, err := websocket.Dial(context.Background(), server.URL+"/bidi", &websocket.DialOptions{
			HTTPClient: server.Client(), Subprotocols: []string{"appkit.contract.bidi.v1"},
		})
		if err == nil {
			_ = conn.CloseNow()
			t.Fatal("Dial succeeded without a subject")
		}
		if response == nil || response.StatusCode != http.StatusUnauthorized || response.Header.Get("Content-Type") != "application/problem+json" {
			t.Fatalf("response = %#v; error=%v", response, err)
		}
	})
}

func TestWebSocketOriginRequiresMatchingSchemeForSameHost(t *testing.T) {
	identity := func(context.Context) (httpserver.WebSocketIdentity, error) {
		return httpserver.WebSocketIdentity{Subject: "browser-user"}, nil
	}
	streamHandler := func(ctx context.Context, _ contract.Stream[string, string]) error {
		<-ctx.Done()
		return nil
	}

	t.Run("direct TLS rejects same-host HTTP origin", func(t *testing.T) {
		hub := startWebSocketHub(t)
		cfg := websocketTestConfig(hub)
		cfg.OriginPatterns = []string{"https://127.0.0.1:*"}
		server, _ := startWebSocketTLSServer(t, cfg, identity, streamHandler)
		host := strings.TrimPrefix(server.URL, "https://")
		req, err := http.NewRequest(http.MethodGet, server.URL+"/bidi", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", "http://"+host)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Content-Type") != "application/problem+json" {
			t.Fatalf("response status/content-type = %d/%q, want 403 problem+json", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	})

	t.Run("direct TLS accepts same-host HTTPS origin", func(t *testing.T) {
		hub := startWebSocketHub(t)
		server, _ := startWebSocketTLSServer(t, websocketTestConfig(hub), identity, streamHandler)
		host := strings.TrimPrefix(server.URL, "https://")
		conn, response, err := websocket.Dial(context.Background(), server.URL+"/bidi", &websocket.DialOptions{
			HTTPClient:   server.Client(),
			HTTPHeader:   http.Header{"Origin": []string{"https://" + host}},
			Subprotocols: []string{"appkit.contract.bidi.v1"},
		})
		if err != nil {
			t.Fatalf("same-origin TLS WebSocket Dial failed: %v (response=%v)", err, response)
		}
		_ = conn.CloseNow()
	})

	t.Run("TLS-terminating proxy requires explicit HTTPS allowlist", func(t *testing.T) {
		t.Run("forged forwarded proto does not establish same-origin", func(t *testing.T) {
			hub := startWebSocketHub(t)
			server, _ := startWebSocketServer(t, websocketTestConfig(hub), identity, streamHandler)
			host := strings.TrimPrefix(server.URL, "http://")
			req, err := http.NewRequest(http.MethodGet, server.URL+"/bidi", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Origin", "https://"+host)
			req.Header.Set("X-Forwarded-Proto", "https")
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Content-Type") != "application/problem+json" {
				t.Fatalf("response status/content-type = %d/%q, want 403 problem+json", resp.StatusCode, resp.Header.Get("Content-Type"))
			}
		})

		t.Run("explicit full HTTPS origin pattern allows proxy request", func(t *testing.T) {
			hub := startWebSocketHub(t)
			cfg := websocketTestConfig(hub)
			cfg.OriginPatterns = []string{"https://127.0.0.1:*"}
			server, _ := startWebSocketServer(t, cfg, identity, streamHandler)
			host := strings.TrimPrefix(server.URL, "http://")
			conn, response, err := websocket.Dial(context.Background(), server.URL+"/bidi", &websocket.DialOptions{
				HTTPClient:   server.Client(),
				HTTPHeader:   http.Header{"Origin": []string{"https://" + host}},
				Subprotocols: []string{"appkit.contract.bidi.v1"},
			})
			if err != nil {
				t.Fatalf("explicitly allowlisted proxy WebSocket Dial failed: %v (response=%v)", err, response)
			}
			_ = conn.CloseNow()
		})
	})
}

func TestWebSocketOriginPatternsRejectUnsafeOrMalformedValues(t *testing.T) {
	for _, pattern := range []string{"", " *", "*", "**", "*.*", "*.*.*", "https://*", "https://**", "https://example.test/path", "ftp://example.test", "["} {
		t.Run(pattern, func(t *testing.T) {
			cfg := websocketTestConfig(httpserver.NewWebSocketHub())
			cfg.OriginPatterns = []string{pattern}
			_, err := httpserver.NewWebSocketHandler[string, string](cfg,
				func(context.Context) (httpserver.WebSocketIdentity, error) {
					return httpserver.WebSocketIdentity{Subject: "user"}, nil
				},
				func(context.Context, contract.Stream[string, string]) error { return nil })
			if !apperr.Is(err, apperr.CodeInvalidArgument) {
				t.Fatalf("NewWebSocketHandler() error = %v, want INVALID_ARGUMENT", err)
			}
		})
	}
}

func TestWebSocketRejectsServerOnlyFrameFromClient(t *testing.T) {
	hub := startWebSocketHub(t)
	cfg := websocketTestConfig(hub)
	server, _ := startWebSocketServer(t, cfg, func(context.Context) (httpserver.WebSocketIdentity, error) {
		return httpserver.WebSocketIdentity{Subject: "test-user"}, nil
	}, func(ctx context.Context, peer contract.Stream[string, string]) error {
		_, err := peer.Recv(ctx)
		return err
	})
	conn := dialTestWebSocket(t, server)
	writeTestFrame(t, conn, testWireFrame{Type: "end"})
	if got := readTestFrame(t, conn); got.Type != "error" || got.Code != apperr.CodeInvalidArgument {
		t.Fatalf("terminal frame = %+v, want invalid-argument error", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
		t.Fatalf("close status = %d; error=%v", got, err)
	}
}

func TestWebSocketErrorFrameDoesNotExposeHandlerError(t *testing.T) {
	hub := startWebSocketHub(t)
	cfg := websocketTestConfig(hub)
	server, _ := startWebSocketServer(t, cfg, func(context.Context) (httpserver.WebSocketIdentity, error) {
		return httpserver.WebSocketIdentity{Subject: "test-user"}, nil
	}, func(context.Context, contract.Stream[string, string]) error {
		return errors.New("private-token-shaped-detail")
	})
	conn := dialTestWebSocket(t, server)
	frame := readTestFrame(t, conn)
	if frame.Type != "error" || frame.Code != apperr.CodeInternal || frame.Message != "" {
		t.Fatalf("terminal frame = %+v", frame)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusInternalError {
		t.Fatalf("close status = %d; error=%v", got, err)
	}
}

func TestWebSocketCredentialExpirySendsSanitizedTerminal(t *testing.T) {
	hub := startWebSocketHub(t)
	cfg := websocketTestConfig(hub)
	server, _ := startWebSocketServer(t, cfg, func(context.Context) (httpserver.WebSocketIdentity, error) {
		return httpserver.WebSocketIdentity{Subject: "expiring-user", ExpiresAt: time.Now().Add(150 * time.Millisecond)}, nil
	}, func(ctx context.Context, peer contract.Stream[string, string]) error {
		_, err := peer.Recv(ctx)
		return err
	})
	conn := dialTestWebSocket(t, server)
	frame := readTestFrame(t, conn)
	if frame.Type != "error" || frame.Code != apperr.CodeUnauthenticated || frame.Message != "" {
		t.Fatalf("expiry terminal frame = %+v", frame)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
		t.Fatalf("expiry close status = %d; error=%v", got, err)
	}
}

func TestWebSocketMessageLimitAndHubDrain(t *testing.T) {
	hub := startWebSocketHub(t)
	cfg := websocketTestConfig(hub)
	cfg.MaxMessageBytes = 8
	server, _ := startWebSocketServer(t, cfg, func(context.Context) (httpserver.WebSocketIdentity, error) {
		return httpserver.WebSocketIdentity{Subject: "test-user"}, nil
	}, func(ctx context.Context, peer contract.Stream[string, string]) error {
		_, err := peer.Recv(ctx)
		return err
	})
	conn := dialTestWebSocket(t, server)
	writeTestFrame(t, conn, testWireFrame{Type: "data", Data: "payload-too-long"})
	frame := readTestFrame(t, conn)
	if frame.Type != "error" || frame.Code != apperr.CodeInvalidArgument {
		t.Fatalf("oversize terminal frame = %+v", frame)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
	_, _, _ = conn.Read(closeCtx)
	closeCancel()

	// A second idle connection exercises the managed close-frame path.
	conn2 := dialTestWebSocket(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	closeResult := make(chan error, 1)
	go func() {
		_, _, err := conn2.Read(ctx)
		closeResult <- err
	}()
	if err := hub.Drain(ctx); err != nil {
		t.Fatalf("Hub.Drain: %v", err)
	}
	err := <-closeResult
	if got := websocket.CloseStatus(err); got != websocket.StatusGoingAway {
		t.Fatalf("drain close status = %d; error=%v", got, err)
	}
}

func TestWebSocketCancellationReachesServiceAndBoundedQueuePreservesOrder(t *testing.T) {
	t.Run("connection cancellation", func(t *testing.T) {
		hub := startWebSocketHub(t)
		cfg := websocketTestConfig(hub)
		started := make(chan struct{})
		cancelled := make(chan struct{})
		server, _ := startWebSocketServer(t, cfg, func(context.Context) (httpserver.WebSocketIdentity, error) {
			return httpserver.WebSocketIdentity{Subject: "test-user"}, nil
		}, func(ctx context.Context, _ contract.Stream[string, string]) error {
			close(started)
			<-ctx.Done()
			close(cancelled)
			return ctx.Err()
		})
		conn := dialTestWebSocket(t, server)
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("service handler did not start")
		}
		if err := conn.CloseNow(); err != nil {
			t.Fatalf("CloseNow: %v", err)
		}
		select {
		case <-cancelled:
		case <-time.After(2 * time.Second):
			t.Fatal("WebSocket disconnect did not cancel service context")
		}
	})

	t.Run("slow consumer does not lose or reorder frames", func(t *testing.T) {
		hub := startWebSocketHub(t)
		cfg := websocketTestConfig(hub)
		cfg.Stream.QueueSize = 1
		release := make(chan struct{})
		server, _ := startWebSocketServer(t, cfg, func(context.Context) (httpserver.WebSocketIdentity, error) {
			return httpserver.WebSocketIdentity{Subject: "test-user"}, nil
		}, func(ctx context.Context, peer contract.Stream[string, string]) error {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			for {
				value, err := peer.Recv(ctx)
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				if err := peer.Send(ctx, value); err != nil {
					return err
				}
			}
		})
		conn := dialTestWebSocket(t, server)
		values := []string{"first", "second", "third", "fourth"}
		for _, value := range values {
			writeTestFrame(t, conn, testWireFrame{Type: "data", Data: value})
			time.Sleep(10 * time.Millisecond) // Let the single-slot contract queue fill before resuming the consumer.
		}
		close(release)
		for i, want := range values {
			if got := readTestFrame(t, conn); got.Type != "data" || got.Data != want {
				t.Fatalf("response frame[%d] = %+v, want %q", i, got, want)
			}
		}
		writeTestFrame(t, conn, testWireFrame{Type: "half_close"})
		if got := readTestFrame(t, conn); got.Type != "end" {
			t.Fatalf("terminal frame = %+v, want end", got)
		}
	})
}

func TestWebSocketSecureClientOperationCancellationIsRetryable(t *testing.T) {
	release := make(chan struct{})
	ready := make(chan struct{})
	cfg := websocketTestConfig(httpserver.NewWebSocketHub())
	stream, err := openSecureWebSocketStream(t, context.Background(), cfg.Stream,
		func(ctx context.Context, peer contract.Stream[string, string]) error {
			value, err := peer.Recv(ctx)
			if err != nil {
				return err
			}
			close(ready)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			return peer.Send(ctx, value)
		})
	if err != nil {
		t.Fatalf("openSecureWebSocketStream: %v", err)
	}
	defer stream.Close()
	if err := stream.Send(context.Background(), "retryable"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("service handler did not receive request")
	}
	opCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_, firstErr := stream.Recv(opCtx)
	cancel()
	if !apperr.Is(firstErr, apperr.CodeUnavailable) {
		t.Fatalf("cancelled Recv error = %v, want UNAVAILABLE", firstErr)
	}
	close(release)
	got, err := stream.Recv(context.Background())
	if err != nil || got != "retryable" {
		t.Fatalf("retry Recv = (%q, %v), want (retryable, nil)", got, err)
	}
}

func TestWebSocketSecureClientRemoteErrorIsStableAndSanitized(t *testing.T) {
	cfg := websocketTestConfig(httpserver.NewWebSocketHub())
	stream, err := openSecureWebSocketStream(t, context.Background(), cfg.Stream,
		func(context.Context, contract.Stream[string, string]) error {
			return errors.New("private-upstream-token-shaped-detail")
		})
	if err != nil {
		t.Fatalf("openSecureWebSocketStream: %v", err)
	}
	defer stream.Close()
	_, first := stream.Recv(context.Background())
	_, second := stream.Recv(context.Background())
	if !apperr.Is(first, apperr.CodeInternal) || first != second {
		t.Fatalf("terminal errors = (%p, %v), (%p, %v); want stable INTERNAL", first, first, second, second)
	}
	if strings.Contains(first.Error(), "private-upstream-token-shaped-detail") {
		t.Fatalf("remote terminal leaked handler detail: %v", first)
	}
}

func TestWebSocketSecureClientRejectsTransactionBoundary(t *testing.T) {
	ctx := tx.With(context.Background(), "test-transaction")
	_, err := httpserver.DialSecureWebSocket[string, string](ctx, "wss://example.test/bidi", httpserver.WebSocketConfig{}, contract.SecureClientOptions{})
	if !apperr.Is(err, apperr.CodeTxBoundary) {
		t.Fatalf("DialSecureWebSocket under transaction = %v, want TX_BOUNDARY", err)
	}
}

func openSecureWebSocketStream(
	t *testing.T,
	ctx context.Context,
	cfg contract.StreamConfig,
	handler contract.StreamHandler[string, string],
) (contract.ClientStream[string, string], error) {
	t.Helper()
	hub := startWebSocketHub(t)
	webSocketConfig := websocketTestConfig(hub)
	webSocketConfig.Stream = cfg
	wsHandler, err := httpserver.NewWebSocketHandler[string, string](webSocketConfig,
		func(context.Context) (httpserver.WebSocketIdentity, error) {
			return httpserver.WebSocketIdentity{Subject: "conformance-client"}, nil
		}, httpserver.WebSocketHandler[string, string](handler))
	if err != nil {
		return nil, err
	}
	server := httptest.NewTLSServer(wsHandler)
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = hub.Close(closeCtx)
		server.Close()
	})
	address := "wss" + strings.TrimPrefix(server.URL, "https") + "/bidi"
	return httpserver.DialSecureWebSocket[string, string](ctx, address, webSocketConfig, contract.SecureClientOptions{
		Audience: "websocket-conformance",
		Credentials: contract.ServiceCredentialProviderFunc(func(context.Context, contract.ServiceScope) (contract.ServiceCredential, error) {
			return contract.ServiceCredential{Token: "conformance-token", ExpiresAt: time.Now().Add(time.Hour)}, nil
		}),
		HTTPClient: server.Client(),
	})
}

func TestWebSocketHubIsDrainedByHostManagedService(t *testing.T) {
	hub := httpserver.NewWebSocketHub()
	cfg := websocketTestConfig(hub)
	wsHandler, err := httpserver.NewWebSocketHandler[string, string](cfg,
		func(ctx context.Context) (httpserver.WebSocketIdentity, error) {
			if _, ok := appkit.ServicePrincipalFrom(ctx); !ok {
				return httpserver.WebSocketIdentity{}, apperr.Unauthenticated("authentication required")
			}
			return httpserver.WebSocketIdentity{Subject: "host-test"}, nil
		},
		func(ctx context.Context, peer contract.Stream[string, string]) error {
			_, err := peer.Recv(ctx)
			return err
		})
	if err != nil {
		t.Fatal(err)
	}
	address := make(chan string, 1)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(contract.HeaderServiceAuthorization) != "Bearer host-test-credential" {
				apperr.WriteProblem(w, apperr.Unauthenticated("service authentication required"))
				return
			}
			ctx := appkit.WithServicePrincipal(r.Context(), appkit.ServicePrincipal{Subject: "host-test"})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	module := appkit.ModuleFunc("websocket-host-test", func(reg *appkit.Registry) error {
		if err := reg.ManagedService("websocket-hub", appkit.ServiceCritical, func(*appkit.Registry) (appkit.ManagedService, error) {
			return hub, nil
		}); err != nil {
			return err
		}
		reg.MountInternalService("GET /bidi", wsHandler)
		return nil
	})
	middleware := append(httpserver.Base(logger), auth)
	app := appkit.New([]appkit.Module{module},
		appkit.Security(appkit.SecurityInternalService),
		appkit.HTTPAddr("127.0.0.1:0"),
		appkit.Logger(logger),
		appkit.Middleware(middleware...),
		appkit.HTTPServer(func(server *http.Server) {
			server.BaseContext = func(listener net.Listener) context.Context {
				address <- "http://" + listener.Addr().String()
				return context.Background()
			}
		}),
	)
	parent, cancel := context.WithCancel(context.Background())
	host, err := app.Start(parent)
	if err != nil {
		cancel()
		t.Fatalf("App.Start: %v", err)
	}
	defer func() {
		cancel()
		ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		waited := make(chan struct{})
		go func() { _ = host.Wait(); close(waited) }()
		select {
		case <-waited:
		case <-ctx.Done():
			t.Error("Host did not stop")
		}
	}()
	baseURL := <-address
	conn, response, err := websocket.Dial(context.Background(), baseURL+"/bidi", &websocket.DialOptions{
		HTTPHeader:   http.Header{contract.HeaderServiceAuthorization: []string{"Bearer host-test-credential"}},
		Subprotocols: []string{"appkit.contract.bidi.v1"},
	})
	if err != nil {
		t.Fatalf("WebSocket Dial: %v (response=%v)", err, response)
	}
	defer conn.CloseNow()
	closed := make(chan error, 1)
	ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	go func() {
		_, _, err := conn.Read(ctx)
		closed <- err
	}()
	cancel()
	err = <-closed
	if got := websocket.CloseStatus(err); got != websocket.StatusGoingAway {
		t.Fatalf("Host drain close status = %d, error=%v", got, err)
	}
	if err := host.Wait(); err != nil {
		t.Fatalf("Host.Wait: %v", err)
	}
}
