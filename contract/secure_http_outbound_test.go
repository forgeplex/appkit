package contract_test

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
	"github.com/forgeplex/appkit/outbound"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestSecureHTTPSharedConfigAndClientSpan(t *testing.T) {
	previous := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	remoteSpan := make(chan trace.SpanContext, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get(contract.HeaderServiceAuthorization) != "Bearer service.jwt.token" {
			t.Error("service credential did not reach verified TLS endpoint")
		}
		for _, name := range []string{"Authorization", "Cookie", "Baggage"} {
			if request.Header.Get(name) != "" {
				t.Errorf("sensitive incoming header forwarded: %s", name)
			}
		}
		remoteCtx := propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(request.Header))
		remoteSpan <- trace.SpanContextFromContext(remoteCtx)
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	configured, err := (outbound.HTTPConfig{Timeout: time.Second, ResponseHeaderTimeout: time.Second, MaxIdleConnsPerHost: 4}).HTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	configured.Transport.(*http.Transport).TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	client, err := contract.NewSecureHTTPClient(server.URL, contract.SecureClientOptions{
		Audience: "test-target", Credentials: freshProvider(), HTTPClient: configured,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	ctx, parent := provider.Tracer("test").Start(context.Background(), "parent")
	defer parent.End()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/private?token=secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "user-secret")
	request.Header.Set("Cookie", "private=secret")
	request.Header.Set("Baggage", "tenant=secret")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Authorization", "Cookie", "Baggage"} {
		if response.Request.Header.Get(name) != "" {
			t.Errorf("response request no longer reflects sanitized headers: %s", name)
		}
	}
	_, err = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	spans := recorder.Ended()
	if len(spans) != 1 || spans[0].SpanKind() != trace.SpanKindClient || spans[0].Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Fatalf("ended client spans = %v", spans)
	}
	if got := <-remoteSpan; got.TraceID() != parent.SpanContext().TraceID() || got.SpanID() != spans[0].SpanContext().SpanID() {
		t.Fatalf("TLS request did not propagate client span: %v", got)
	}
	for _, attr := range spans[0].Attributes() {
		if strings.Contains(attr.Value.AsString(), "secret") || strings.Contains(attr.Value.AsString(), "/private") {
			t.Fatalf("span leaked sensitive request content: %v", attr)
		}
	}
	if client.Timeout != time.Second || configured.Transport.(*http.Transport).TLSClientConfig.ServerName != "" {
		t.Fatal("shared timeout was lost or caller transport was mutated")
	}
}

func TestSecureHTTPSharedResponseHeaderTimeout(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	configured, err := (outbound.HTTPConfig{ResponseHeaderTimeout: 20 * time.Millisecond}).HTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	configured.Transport.(*http.Transport).TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	client, err := contract.NewSecureHTTPClient(server.URL, contract.SecureClientOptions{
		Audience: "test-target", Credentials: freshProvider(), HTTPClient: configured,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(request); !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("header budget error = %v, root error = %v", err, ctx.Err())
	}
}

func TestSecureHTTPRedirectRefusalRecordsFailureWithAndWithoutTimeout(t *testing.T) {
	previous := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/destination" {
			t.Error("refused redirect reached destination")
		}
		http.Redirect(w, request, "/destination", http.StatusFound)
	}))
	defer server.Close()
	for _, timeout := range []time.Duration{0, time.Second} {
		t.Run(timeout.String(), func(t *testing.T) {
			configured := *server.Client()
			configured.Timeout = timeout
			client, err := contract.NewSecureHTTPClient(server.URL, contract.SecureClientOptions{
				Audience: "test-target", Credentials: freshProvider(), HTTPClient: &configured,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			attrs := map[string]string{"http.request.method": http.MethodGet, "http.response.status_class": "3xx", "appkit.outcome": "error"}
			var before int64
			for _, metric := range collectStreamMetrics(t) {
				if metric.Name == "appkit.http.client.request" {
					for _, point := range metric.Data.(metricdata.Sum[int64]).DataPoints {
						if streamMetricAttrsMatch(point.Attributes.ToSlice(), attrs) {
							before = point.Value
						}
					}
				}
			}
			request, err := http.NewRequest(http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Do(request); !apperr.Is(err, apperr.CodePermissionDenied) {
				t.Fatalf("redirect refusal = %v", err)
			}
			spans := recorder.Ended()
			if len(spans) == 0 || spans[len(spans)-1].Status().Code != codes.Error {
				t.Fatal("redirect refusal did not end an error span")
			}
			if got := collectStreamSum(t, "appkit.http.client.request", attrs); got != before+1 {
				t.Fatalf("refused redirect metric = %d, want %d", got, before+1)
			}
		})
	}
}
