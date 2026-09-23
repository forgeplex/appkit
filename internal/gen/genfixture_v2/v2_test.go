package fixturev2

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/contract"
	"github.com/forgeplex/appkit/httpserver"
)

type fixtureService struct{}

func (fixtureService) Ping(_ context.Context, req PingRequestV2) (PingResponseV2, error) {
	return PingResponseV2{Message: "hello " + req.Name}, nil
}

func (fixtureService) Watch(ctx context.Context, cursor string, req WatchRequestV2, sender StreamSenderV2[WatchResponseV2]) error {
	if cursor != "opaque-cursor" && cursor != "local-cursor" {
		return errors.New("cursor was not forwarded")
	}
	return sender.Send(ctx, WatchResponseV2{
		EventID: "event-1",
		Event:   "update",
		Meta:    EventMetaV2{Source: req.Topic},
	})
}

func (fixtureService) Tail(ctx context.Context, cursor string, sender StreamSenderV2[TailResponseV2]) error {
	if cursor != "tail-cursor" && cursor != "local-tail-cursor" {
		return errors.New("tail cursor was not forwarded")
	}
	return sender.Send(ctx, TailResponseV2{Sequence: "next"})
}

func (fixtureService) Chat(ctx context.Context, stream contract.Stream[ChatResponseV2, ChatRequestV2]) error {
	for {
		request, err := stream.Recv(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(ctx, ChatResponseV2{Text: "echo: " + request.Text}); err != nil {
			return err
		}
	}
}

func fixtureConfig() contract.StreamConfig {
	return contract.StreamConfig{
		MaxDuration:  time.Minute,
		IdleTimeout:  time.Minute,
		CloseTimeout: time.Second,
		QueueSize:    1,
	}
}

func TestV2UnaryHTTPClientServer(t *testing.T) {
	service := WrapServiceV2(fixtureService{}, 0)
	server := httptest.NewServer(NewHTTPHandlerV2(service))
	defer server.Close()
	client := NewClientV2(server.URL, "fixture", nil)
	got, err := client.Ping(context.Background(), PingRequestV2{Name: "AppKit"})
	if err != nil || got.Message != "hello AppKit" {
		t.Fatalf("Ping() = %+v, %v", got, err)
	}
}

func TestV2LocalServerStream(t *testing.T) {
	reader, err := OpenWatchLocalV2(context.Background(), "local-cursor", fixtureConfig(), fixtureService{}, WatchRequestV2{Topic: "local"})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	event, err := reader.Recv(context.Background())
	if err != nil || event.Event != "update" || event.Meta.Source != "local" {
		t.Fatalf("Recv() = %+v, %v", event, err)
	}
}

func TestV2LocalServerStreamWithoutRequest(t *testing.T) {
	reader, err := OpenTailLocalV2(context.Background(), "local-tail-cursor", fixtureConfig(), fixtureService{})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	event, err := reader.Recv(context.Background())
	if err != nil || event.Sequence != "next" {
		t.Fatalf("Recv() = %+v, %v", event, err)
	}
}

func TestV2SSEAdapter(t *testing.T) {
	cfg := httpserver.SSEConfig{
		Stream:              fixtureConfig(),
		MaxRequestBodyBytes: 4096,
		WriteTimeout:        time.Second,
	}
	handler, err := NewWatchSSEHandlerV2(cfg, fixtureService{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v2/watch", strings.NewReader(`{"topic":"http"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Last-Event-ID", "opaque-cursor")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), "id: event-1\n") || !strings.Contains(string(body), `"event":"update"`) {
		t.Fatalf("unexpected SSE body: %s", body)
	}
}

func TestV2SSEAdapterWithoutRequest(t *testing.T) {
	cfg := httpserver.SSEConfig{
		Stream:              fixtureConfig(),
		MaxRequestBodyBytes: 4096,
		WriteTimeout:        time.Second,
	}
	handler, err := NewTailSSEHandlerV2(cfg, fixtureService{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v2/tail", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Last-Event-ID", "tail-cursor")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"sequence":"next"`) {
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
}

func TestV2LocalBidiStream(t *testing.T) {
	stream, err := OpenChatLocalV2(context.Background(), fixtureConfig(), fixtureService{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	ctx := context.Background()
	if err := stream.Send(ctx, ChatRequestV2{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := stream.Recv(ctx)
	if err != nil || got.Text != "echo: hello" {
		t.Fatalf("Recv() = %+v, %v", got, err)
	}
	if _, err := stream.Recv(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal Recv() error = %v, want EOF", err)
	}
}

func TestV2GeneratedWebSocketSecureBidi(t *testing.T) {
	service := fixtureService{}
	hub := httpserver.NewWebSocketHub()
	if err := hub.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := httpserver.WebSocketConfig{
		Stream:           fixtureConfig(),
		MaxMessageBytes:  4096,
		HandshakeTimeout: time.Second,
		WriteTimeout:     time.Second,
		Hub:              hub,
	}
	wsHandler, err := NewChatWebSocketHandlerV2(cfg, service)
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().Add(30 * time.Second)
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(contract.HeaderServiceAuthorization) != "Bearer test-service-credential" {
			apperr.WriteProblem(w, apperr.Unauthenticated("service authentication required"))
			return
		}
		ctx := appkit.WithServicePrincipal(r.Context(), appkit.ServicePrincipal{
			Subject: "test-service", ExpiresAt: expiresAt,
		})
		wsHandler.ServeHTTP(w, r.WithContext(ctx))
	})
	base := httpserver.Base(slog.New(slog.NewTextHandler(io.Discard, nil)))
	var handler http.Handler = root
	for i := len(base) - 1; i >= 0; i-- {
		handler = base[i](handler)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = hub.Close(ctx)
		server.Close()
	})
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
	}
	client := &http.Client{Transport: transport}
	var credentialCalls atomic.Int32
	secure := contract.SecureClientOptions{
		Audience: "fixture-chat",
		Credentials: contract.ServiceCredentialProviderFunc(func(_ context.Context, scope contract.ServiceScope) (contract.ServiceCredential, error) {
			credentialCalls.Add(1)
			if scope.Audience != "fixture-chat" {
				return contract.ServiceCredential{}, apperr.PermissionDenied("unexpected audience")
			}
			return contract.ServiceCredential{Token: "test-service-credential", ExpiresAt: expiresAt}, nil
		}),
		HTTPClient: client,
	}
	address := strings.Replace(server.URL, "https://", "wss://", 1) + "/chat"
	stream, err := DialChatWebSocketV2(context.Background(), address, cfg, secure)
	if err != nil {
		t.Fatalf("DialChatWebSocketV2: %v", err)
	}
	defer stream.Close()
	if err := stream.Send(context.Background(), ChatRequestV2{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(context.Background()); err != nil {
		t.Fatal(err)
	}
	response, err := stream.Recv(context.Background())
	if err != nil || response.Text != "echo: hello" {
		t.Fatalf("Recv() = %+v, %v", response, err)
	}
	if _, err := stream.Recv(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal Recv() = %v, want EOF", err)
	}
	if got := credentialCalls.Load(); got != 1 {
		t.Fatalf("credential provider called %d times, want once per handshake", got)
	}
}

func TestV2GeneratedWebSocketCredentialExpiryStopsClient(t *testing.T) {
	hub := httpserver.NewWebSocketHub()
	if err := hub.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := httpserver.WebSocketConfig{
		Stream: contract.StreamConfig{
			MaxDuration:  5 * time.Second,
			IdleTimeout:  4 * time.Second,
			CloseTimeout: time.Second,
			QueueSize:    1,
		},
		MaxMessageBytes:  4096,
		HandshakeTimeout: time.Second,
		WriteTimeout:     time.Second,
		Hub:              hub,
	}
	wsHandler, err := NewChatWebSocketHandlerV2(cfg, fixtureService{})
	if err != nil {
		t.Fatal(err)
	}
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(contract.HeaderServiceAuthorization) != "Bearer short-lived-credential" {
			apperr.WriteProblem(w, apperr.Unauthenticated("service authentication required"))
			return
		}
		ctx := appkit.WithServicePrincipal(r.Context(), appkit.ServicePrincipal{Subject: "short-lived-service"})
		wsHandler.ServeHTTP(w, r.WithContext(ctx))
	})
	base := httpserver.Base(slog.New(slog.NewTextHandler(io.Discard, nil)))
	var handler http.Handler = root
	for i := len(base) - 1; i >= 0; i-- {
		handler = base[i](handler)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = hub.Close(ctx)
		server.Close()
	})
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
	}
	var credentialCalls atomic.Int32
	expiresAt := time.Now().Add(time.Second)
	secure := contract.SecureClientOptions{
		Audience: "fixture-chat",
		Credentials: contract.ServiceCredentialProviderFunc(func(context.Context, contract.ServiceScope) (contract.ServiceCredential, error) {
			credentialCalls.Add(1)
			return contract.ServiceCredential{Token: "short-lived-credential", ExpiresAt: expiresAt}, nil
		}),
		HTTPClient: &http.Client{Transport: transport},
	}
	address := strings.Replace(server.URL, "https://", "wss://", 1) + "/chat"
	stream, err := DialChatWebSocketV2(context.Background(), address, cfg, secure)
	if err != nil {
		t.Fatalf("DialChatWebSocketV2: %v", err)
	}
	defer stream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := stream.Recv(ctx); !apperr.Is(err, apperr.CodeUnauthenticated) {
		t.Fatalf("Recv after credential expiry = %v, want UNAUTHENTICATED", err)
	}
	if got := credentialCalls.Load(); got != 1 {
		t.Fatalf("credential provider called %d times, want once without refresh", got)
	}
}
