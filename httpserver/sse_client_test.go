package httpserver_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/callctx"
	"github.com/forgeplex/appkit/contract"
	"github.com/forgeplex/appkit/httpserver"
	"github.com/forgeplex/appkit/tx"
)

func sseClientConfig() httpserver.SSEClientConfig {
	return httpserver.SSEClientConfig{
		System: "sse-client-test", Method: "Events",
		Stream:              contract.StreamConfig{MaxDuration: 3 * time.Second, CloseTimeout: time.Second, QueueSize: 1},
		MaxRequestBodyBytes: 1024, MaxEventBytes: 1024,
	}
}

func sseSecureOptions(server *httptest.Server) contract.SecureClientOptions {
	return contract.SecureClientOptions{
		Audience: "sse-test", HTTPClient: server.Client(),
		Credentials: contract.ServiceCredentialProviderFunc(func(context.Context, contract.ServiceScope) (contract.ServiceCredential, error) {
			return contract.ServiceCredential{Token: "sse-service-token", ExpiresAt: time.Now().Add(time.Minute)}, nil
		}),
	}
}

func TestSSEClientTLSRoundTrip(t *testing.T) {
	seen := make(chan string, 1)
	handler := makeSSEHandler(t, sseTestConfig(), func(ctx context.Context, cursor string, peer contract.Stream[httpserver.SSEEvent[sseTestReply], sseTestRequest]) error {
		seen <- cursor
		request, err := peer.Recv(ctx)
		if err != nil {
			return err
		}
		return peer.Send(ctx, httpserver.SSEEvent[sseTestReply]{ID: "next/opaque", Data: sseTestReply{Text: request.Text}})
	})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(contract.HeaderServiceAuthorization) != "Bearer sse-service-token" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("service credential was missing or user credentials crossed the boundary")
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	cfg := sseClientConfig()
	cfg.LastEventID = "start:+/1"
	stream, err := httpserver.DialSecureSSE[sseTestRequest, sseTestReply](context.Background(), server.URL, cfg, sseSecureOptions(server), sseTestRequest{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	event, err := stream.Recv(context.Background())
	if err != nil || event.ID != "next/opaque" || event.Data.Text != "hello" || <-seen != cfg.LastEventID {
		t.Fatalf("event = %+v, error = %v", event, err)
	}
	if _, err := stream.Recv(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSSEClientRemoteErrorsAreSanitized(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "problem", true: "in-band"}[committed], func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !committed {
					apperr.WriteProblem(w, apperr.PermissionDenied("remote-secret").WithDetail("secret", "remote-secret"))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: 1\n\nevent: error\ndata: {\"code\":\"PERMISSION_DENIED\",\"message\":\"remote-secret\"}\n\n")
			}))
			defer server.Close()
			stream, err := httpserver.DialSecureSSE[struct{}, int](context.Background(), server.URL, sseClientConfig(), sseSecureOptions(server), struct{}{})
			if committed {
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				if event, err := stream.Recv(context.Background()); err != nil || event.Data != 1 {
					t.Fatalf("first event = %+v, %v", event, err)
				}
				_, err = stream.Recv(context.Background())
			}
			if !apperr.Is(err, apperr.CodePermissionDenied) || strings.Contains(err.Error(), "remote-secret") || len(apperr.From(err).Problem().Details) != 0 {
				t.Fatalf("remote error = %v", err)
			}
		})
	}
}

func TestSSEClientMalformedResponses(t *testing.T) {
	for _, tc := range []struct{ name, contentType, wire, code string }{
		{"wrong content type", "application/json", "{}", apperr.CodeInvalidArgument},
		{"invalid JSON", "text/event-stream", "data: secret-invalid-json\n\n", apperr.CodeInvalidArgument},
		{"truncated data", "text/event-stream", "data: 1\n", apperr.CodeUnavailable},
		{"oversized event", "text/event-stream", "data: \"" + strings.Repeat("x", 80) + "\"\n\n", apperr.CodeInvalidArgument},
		{"bad error code", "text/event-stream", "event: error\ndata: {\"code\":\"secret-lowercase\"}\n\n", apperr.CodeInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = io.WriteString(w, tc.wire)
			}))
			defer server.Close()
			cfg := sseClientConfig()
			cfg.MaxEventBytes = 72
			stream, err := httpserver.DialSecureSSE[struct{}, int](context.Background(), server.URL, cfg, sseSecureOptions(server), struct{}{})
			if err == nil {
				defer stream.Close()
				_, err = stream.Recv(context.Background())
			}
			if !apperr.Is(err, tc.code) || strings.Contains(err.Error(), "secret-") {
				t.Fatalf("error = %v, want %s", err, tc.code)
			}
		})
	}
}

func TestSSEClientOperationCancellationAndClose(t *testing.T) {
	release := make(chan struct{})
	closed := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(closed)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		http.NewResponseController(w).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "data: 7\n\n")
			http.NewResponseController(w).Flush()
		case <-r.Context().Done():
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	stream, err := httpserver.DialSecureSSE[struct{}, int](context.Background(), server.URL, sseClientConfig(), sseSecureOptions(server), struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	op, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := stream.Recv(op); !apperr.Is(err, apperr.CodeUnavailable) {
		t.Fatalf("operation cancellation = %v", err)
	}
	close(release)
	if event, err := stream.Recv(context.Background()); err != nil || event.Data != 7 {
		t.Fatalf("stream after operation cancel = %+v, %v", event, err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel server IO")
	}
}

func TestSSEClientBudgetsCoverHandshakeAndStream(t *testing.T) {
	for _, kind := range []string{"max-handshake", "idle-handshake", "expiry-handshake", "max-stream", "idle-stream", "expiry-stream"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if strings.HasSuffix(kind, "stream") {
					w.Header().Set("Content-Type", "text/event-stream")
					http.NewResponseController(w).Flush()
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			cfg := sseClientConfig()
			secure := sseSecureOptions(server)
			code := apperr.CodeUnavailable
			switch {
			case strings.HasPrefix(kind, "max"):
				cfg.Stream.MaxDuration = 80 * time.Millisecond
			case strings.HasPrefix(kind, "idle"):
				cfg.Stream.IdleTimeout = 80 * time.Millisecond
			case strings.HasPrefix(kind, "expiry"):
				code = apperr.CodeUnauthenticated
				secure.Credentials = contract.ServiceCredentialProviderFunc(func(context.Context, contract.ServiceScope) (contract.ServiceCredential, error) {
					return contract.ServiceCredential{Token: "expiring", ExpiresAt: time.Now().Add(80 * time.Millisecond)}, nil
				})
			}
			start := time.Now()
			stream, err := httpserver.DialSecureSSE[struct{}, int](context.Background(), server.URL, cfg, secure, struct{}{})
			if err == nil {
				defer stream.Close()
				_, err = stream.Recv(context.Background())
			}
			if !apperr.Is(err, code) || time.Since(start) > time.Second {
				t.Fatalf("budget = %v, elapsed=%s", err, time.Since(start))
			}
		})
	}
}

func TestSSEClientProviderFirewallAndRequestLimit(t *testing.T) {
	type privateKey struct{}
	var calls atomic.Int64
	ctx := callctx.With(context.WithValue(context.Background(), privateKey{}, "private"), callctx.Meta{TenantID: "tenant", Partition: "p", RequestID: "request"})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(callctx.HeaderRequestID) != "request" {
			t.Error("whitelisted request metadata was lost")
		}
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	defer server.Close()
	secure := sseSecureOptions(server)
	secure.Credentials = contract.ServiceCredentialProviderFunc(func(ctx context.Context, scope contract.ServiceScope) (contract.ServiceCredential, error) {
		calls.Add(1)
		if ctx.Value(privateKey{}) != nil || scope.TenantID != "tenant" || scope.Partition != "p" || scope.Audience != "sse-test" {
			t.Errorf("provider firewall failed: %+v", scope)
		}
		return contract.ServiceCredential{Token: "token", ExpiresAt: time.Now().Add(time.Minute)}, nil
	})
	cfg := sseClientConfig()
	cfg.MaxRequestBodyBytes = 3
	if _, err := httpserver.DialSecureSSE[string, int](ctx, server.URL, cfg, secure, "oversized"); !apperr.Is(err, apperr.CodeInvalidArgument) || calls.Load() != 0 {
		t.Fatalf("oversized request = %v, credential calls=%d", err, calls.Load())
	}
	stream, err := httpserver.DialSecureSSE[struct{}, int](ctx, server.URL, cfg, secure, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Recv(context.Background()); !errors.Is(err, io.EOF) || calls.Load() != 1 {
		t.Fatalf("EOF = %v, credential calls=%d", err, calls.Load())
	}
}

func TestSSEClientRejectsUnsafeInputsBeforeProvider(t *testing.T) {
	var calls atomic.Int64
	provider := contract.ServiceCredentialProviderFunc(func(context.Context, contract.ServiceScope) (contract.ServiceCredential, error) {
		calls.Add(1)
		return contract.ServiceCredential{}, errors.New("provider-secret")
	})
	for _, tc := range []struct {
		name string
		ctx  context.Context
		url  string
		code string
		edit func(*httpserver.SSEClientConfig, *contract.SecureClientOptions)
	}{
		{name: "nil context", code: apperr.CodeInvalidArgument},
		{name: "transaction", ctx: tx.With(context.Background(), "tx"), code: apperr.CodeTxBoundary},
		{name: "http", ctx: context.Background(), url: "http://example.test", code: apperr.CodeInvalidArgument},
		{name: "query", ctx: context.Background(), url: "https://example.test?token=secret", code: apperr.CodeInvalidArgument},
		{name: "cursor injection", ctx: context.Background(), code: apperr.CodeInvalidArgument, edit: func(cfg *httpserver.SSEClientConfig, _ *contract.SecureClientOptions) {
			cfg.LastEventID = "bad\r\nAuthorization: secret"
		}},
		{name: "nil provider", ctx: context.Background(), code: apperr.CodeInvalidArgument, edit: func(_ *httpserver.SSEClientConfig, secure *contract.SecureClientOptions) { secure.Credentials = nil }},
		{name: "typed nil provider", ctx: context.Background(), code: apperr.CodeInvalidArgument, edit: func(_ *httpserver.SSEClientConfig, secure *contract.SecureClientOptions) {
			secure.Credentials = contract.ServiceCredentialProviderFunc(nil)
		}},
		{name: "invalid budget", ctx: context.Background(), code: apperr.CodeInvalidArgument, edit: func(cfg *httpserver.SSEClientConfig, _ *contract.SecureClientOptions) { cfg.Stream.CloseTimeout = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := sseClientConfig()
			secure := contract.SecureClientOptions{Audience: "test", Credentials: provider}
			if tc.edit != nil {
				tc.edit(&cfg, &secure)
			}
			address := tc.url
			if address == "" {
				address = "https://example.test"
			}
			_, err := httpserver.DialSecureSSE[struct{}, int](tc.ctx, address, cfg, secure, struct{}{})
			if !apperr.Is(err, tc.code) || calls.Load() != 0 || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error = %v; provider calls=%d", err, calls.Load())
			}
		})
	}
}

func TestSSEClientRefusesRedirectAndDoesNotReconnect(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		t.Run(map[bool]string{false: "EOF", true: "redirect"}[redirect], func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if redirect {
					http.Redirect(w, r, "/redirect-target", http.StatusTemporaryRedirect)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
			}))
			defer server.Close()
			stream, err := httpserver.DialSecureSSE[struct{}, int](context.Background(), server.URL+"/private-path", sseClientConfig(), sseSecureOptions(server), struct{}{})
			if redirect {
				if !apperr.Is(err, apperr.CodePermissionDenied) || strings.Contains(err.Error(), "private-path") {
					t.Fatalf("redirect = %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				for range 2 {
					if _, err := stream.Recv(context.Background()); !errors.Is(err, io.EOF) {
						t.Fatalf("EOF = %v", err)
					}
				}
			}
			if requests.Load() != 1 {
				t.Fatalf("requests = %d, want exactly one", requests.Load())
			}
		})
	}
}

type sseRecvObservedContext struct {
	context.Context
	entered chan struct{}
}

func (c sseRecvObservedContext) Done() <-chan struct{} {
	select {
	case c.entered <- struct{}{}:
	default:
	}
	return c.Context.Done()
}

func TestSSEClientRejectsOverlappingRecv(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		http.NewResponseController(w).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	stream, err := httpserver.DialSecureSSE[struct{}, int](context.Background(), server.URL, sseClientConfig(), sseSecureOptions(server), struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() { _, err := stream.Recv(sseRecvObservedContext{Context: ctx, entered: entered}); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("Recv did not begin")
	}
	if _, err := stream.Recv(context.Background()); !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("overlapping Recv = %v", err)
	}
	cancel()
	if err := <-done; !apperr.Is(err, apperr.CodeUnavailable) {
		t.Fatalf("first Recv cancellation = %v", err)
	}
}

func TestSSEClientHeartbeatsDoNotExtendIdle(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		http.NewResponseController(w).Flush()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				_, _ = io.WriteString(w, ": heartbeat\n\n")
				if http.NewResponseController(w).Flush() != nil {
					return
				}
			}
		}
	}))
	defer server.Close()
	cfg := sseClientConfig()
	cfg.Stream.IdleTimeout = 80 * time.Millisecond
	stream, err := httpserver.DialSecureSSE[struct{}, int](context.Background(), server.URL, cfg, sseSecureOptions(server), struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	start := time.Now()
	if _, err := stream.Recv(context.Background()); !apperr.Is(err, apperr.CodeUnavailable) || time.Since(start) > time.Second {
		t.Fatalf("idle terminal = %v, elapsed=%s", err, time.Since(start))
	}
}
