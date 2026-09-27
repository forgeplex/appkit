package gen_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/callctx"
	"github.com/forgeplex/appkit/contract"
	fixture "github.com/forgeplex/appkit/internal/gen/genfixture"
	fixturev2 "github.com/forgeplex/appkit/internal/gen/genfixture_v2"
	"github.com/forgeplex/appkit/tx"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestGeneratedUnarySharedOutboundRoundTrip(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		for _, mode := range []string{"legacy", "secure"} {
			t.Run(version+"/"+mode, func(t *testing.T) {
				recorder := tracetest.NewSpanRecorder()
				provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
				previous := otel.GetTracerProvider()
				otel.SetTracerProvider(provider)
				t.Cleanup(func() {
					otel.SetTracerProvider(previous)
					_ = provider.Shutdown(context.Background())
				})
				type observation struct {
					trace                   trace.SpanContext
					caller, requestID, auth string
				}
				observed := make(chan observation, 1)
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					ctx := propagation.TraceContext{}.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
					observed <- observation{trace.SpanContextFromContext(ctx), r.Header.Get(callctx.HeaderCaller), r.Header.Get(callctx.HeaderRequestID), r.Header.Get(contract.HeaderServiceAuthorization)}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"message":"ok"}`))
				}))
				t.Cleanup(server.Close)
				hc := server.Client()
				hc.Timeout = 2 * time.Second
				originalTransport := hc.Transport
				secure := contract.SecureClientOptions{
					Audience:   "greet",
					HTTPClient: hc,
					Credentials: contract.ServiceCredentialProviderFunc(func(context.Context, contract.ServiceScope) (contract.ServiceCredential, error) {
						return contract.ServiceCredential{Token: "fixture-service-token", ExpiresAt: time.Now().Add(time.Minute)}, nil
					}),
				}
				var call func(context.Context) (string, error)
				if version == "v1" {
					client := fixture.NewClient(server.URL, "fixture-caller", hc)
					if mode == "secure" {
						var err error
						client, err = fixture.NewSecureClient(server.URL, secure)
						if err != nil {
							t.Fatal(err)
						}
					}
					call = func(ctx context.Context) (string, error) {
						result, err := client.Greet(ctx, fixture.GreetRequest{Name: "private-payload"})
						return result.Message, err
					}
				} else {
					client := fixturev2.NewClientV2(server.URL, "fixture-caller", hc)
					if mode == "secure" {
						var err error
						client, err = fixturev2.NewSecureClientV2(server.URL, secure)
						if err != nil {
							t.Fatal(err)
						}
					}
					call = func(ctx context.Context) (string, error) {
						result, err := client.Ping(ctx, fixturev2.PingRequestV2{Name: "private-payload"})
						return result.Message, err
					}
				}
				if hc.Transport != originalTransport || hc.Timeout != 2*time.Second {
					t.Fatal("generated constructor modified the supplied HTTP client")
				}
				ctx, parent := provider.Tracer("fixture").Start(context.Background(), "parent")
				defer parent.End()
				ctx = callctx.With(ctx, callctx.Meta{RequestID: "private-request-id", TenantID: "private-tenant"})
				message, err := call(ctx)
				if err != nil || message != "ok" {
					t.Fatalf("call = %q, %v", message, err)
				}
				got := <-observed
				if !got.trace.IsValid() || got.trace.TraceID() != parent.SpanContext().TraceID() {
					t.Fatalf("trace context was not propagated: %+v", got.trace)
				}
				if got.requestID != "private-request-id" {
					t.Fatalf("request ID = %q", got.requestID)
				}
				if mode == "legacy" && got.caller != "fixture-caller" {
					t.Fatalf("caller = %q", got.caller)
				}
				if mode == "secure" && got.auth != "Bearer fixture-service-token" {
					t.Fatal("service credential was not propagated")
				}
				outboundSpans := 0
				for _, span := range recorder.Ended() {
					if span.InstrumentationScope().Name != "github.com/forgeplex/appkit/outbound" {
						continue
					}
					outboundSpans++
					if span.SpanContext().SpanID() != got.trace.SpanID() {
						t.Fatal("wire trace context does not identify the outbound span")
					}
					for _, attr := range span.Attributes() {
						if strings.Contains(attr.Value.Emit(), "private-") || strings.Contains(attr.Value.Emit(), "fixture-service-token") {
							t.Fatalf("outbound attribute leaked private context: %s", attr.Key)
						}
					}
				}
				if outboundSpans != 1 {
					t.Fatalf("outbound spans = %d, want exactly one", outboundSpans)
				}
				if _, err := call(tx.With(ctx, struct{}{})); !apperr.Is(err, apperr.CodeTxBoundary) {
					t.Fatalf("transaction guard = %v", err)
				}
				if len(observed) != 0 {
					t.Fatal("transaction guard dispatched an HTTP request")
				}
			})
		}
	}
}
