package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/contract"
	"github.com/forgeplex/appkit/internal/metrics"
	"github.com/forgeplex/appkit/outbound"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestSecureWebSocketSharedHTTPConfigAndHandshakeObservability(t *testing.T) {
	previous := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	hub := newTransportTestWebSocketHub(t)
	if err := hub.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := transportMetricsWebSocketConfig(hub, "shared-websocket", nextTransportMetricMethod("outbound"))
	handler, err := NewWebSocketHandler[string, string](cfg,
		func(context.Context) (WebSocketIdentity, error) {
			return WebSocketIdentity{Subject: "test-service"}, nil
		},
		func(ctx context.Context, peer contract.Stream[string, string]) error {
			value, err := peer.Recv(ctx)
			if err != nil {
				return err
			}
			if _, err := peer.Recv(ctx); !errors.Is(err, io.EOF) {
				return apperr.Internal(errors.New("missing half-close"))
			}
			return peer.Send(ctx, value)
		})
	if err != nil {
		t.Fatal(err)
	}
	remoteSpan := make(chan trace.SpanContext, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get(contract.HeaderServiceAuthorization) != "Bearer local-test-credential" {
			t.Error("missing handshake credential")
		}
		remote := propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(request.Header))
		remoteSpan <- trace.SpanContextFromContext(remote)
		handler.ServeHTTP(w, request)
	}))
	defer server.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = hub.Close(ctx)
	}()
	httpClient, err := (outbound.HTTPConfig{ResponseHeaderTimeout: time.Second, MaxIdleConnsPerHost: 2}).HTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	httpClient.Transport.(*http.Transport).TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	attrs := map[string]string{
		"http.request.method": http.MethodGet, "http.response.status_class": "1xx", metrics.AttrOutcome: metrics.OutcomeOK,
	}
	before := transportMetricValue(t, "appkit.http.client.request", attrs)
	ctx, parent := provider.Tracer("test").Start(context.Background(), "parent")
	defer parent.End()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cfg.Hub = nil
	stream, err := DialSecureWebSocket[string, string](ctx, "wss"+strings.TrimPrefix(server.URL, "https")+"/bidi", cfg, contract.SecureClientOptions{
		Audience: "test-service", HTTPClient: httpClient,
		Credentials: contract.ServiceCredentialProviderFunc(func(context.Context, contract.ServiceScope) (contract.ServiceCredential, error) {
			return contract.ServiceCredential{Token: "local-test-credential", ExpiresAt: time.Now().Add(time.Minute)}, nil
		}),
	})
	if err != nil {
		t.Fatalf("DialSecureWebSocket: %v", err)
	}
	defer stream.Close()
	var handshake sdktrace.ReadOnlySpan
	for _, span := range recorder.Ended() {
		if span.InstrumentationScope().Name == "github.com/forgeplex/appkit/outbound" {
			if handshake != nil {
				t.Fatal("handshake was instrumented more than once")
			}
			handshake = span
		}
	}
	if handshake == nil || handshake.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Fatal("WSS handshake did not emit a completed client span under its parent")
	}
	if got := <-remoteSpan; got.TraceID() != parent.SpanContext().TraceID() || got.SpanID() != handshake.SpanContext().SpanID() {
		t.Fatal("WSS handshake did not propagate the shared HTTP client span")
	}
	if got := transportMetricValue(t, "appkit.http.client.request", attrs); got != before+1 {
		t.Fatalf("handshake request count = %d, want %d before stream closes", got, before+1)
	}
	if err := stream.Send(ctx, "private-payload"); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(ctx); err != nil {
		t.Fatal(err)
	}
	if value, err := stream.Recv(ctx); err != nil || value != "private-payload" {
		t.Fatalf("upgrade lost bidirectional body: value=%q error=%v", value, err)
	}
	if _, err := stream.Recv(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal = %v, want EOF", err)
	}
	frameAttrs := map[string]string{
		metrics.AttrSystem: cfg.System, metrics.AttrMethod: cfg.Method, metrics.AttrTransport: metrics.TransportWebSocket,
		metrics.AttrDirection: metrics.DirectionClientToServer, metrics.AttrFrameType: metrics.FrameData,
	}
	if got := transportMetricValue(t, "appkit.contract.stream.transport.frame.sent", frameAttrs); got != 1 {
		t.Fatalf("client stream data frames = %d, want 1", got)
	}
}

func TestSecureWebSocketSharedResponseHeaderTimeout(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	httpClient, err := (outbound.HTTPConfig{ResponseHeaderTimeout: 20 * time.Millisecond}).HTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	httpClient.Transport.(*http.Transport).TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	cfg := transportMetricsWebSocketConfig(nil, "shared-websocket", "header-timeout")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = DialSecureWebSocket[string, string](ctx, "wss"+strings.TrimPrefix(server.URL, "https")+"/bidi", cfg, contract.SecureClientOptions{
		Audience: "test-service", HTTPClient: httpClient,
		Credentials: contract.ServiceCredentialProviderFunc(func(context.Context, contract.ServiceScope) (contract.ServiceCredential, error) {
			return contract.ServiceCredential{Token: "local-test-credential", ExpiresAt: time.Now().Add(time.Minute)}, nil
		}),
	})
	if !apperr.Is(err, apperr.CodeUnavailable) || ctx.Err() != nil {
		t.Fatalf("shared header timeout = %v, root context error = %v", err, ctx.Err())
	}
}
