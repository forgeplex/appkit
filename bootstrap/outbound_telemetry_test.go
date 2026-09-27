package bootstrap

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/forgeplex/appkit/config"
	"github.com/forgeplex/appkit/contract"
	"github.com/forgeplex/appkit/httpserver"
	"github.com/forgeplex/appkit/outbound"
	"go.opentelemetry.io/otel"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestServiceYAMLAutomaticallyExportsOutboundClients(t *testing.T) {
	previousTrace, previousMeter, previousPropagation := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(previousTrace)
		otel.SetMeterProvider(previousMeter)
		otel.SetTextMapPropagator(previousPropagation)
	})
	for _, key := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"} {
		t.Setenv(key, "")
	}
	var mu sync.Mutex
	spans, metrics := map[string]bool{}, map[string]bool{}
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v1/traces":
			var request tracepb.ExportTraceServiceRequest
			if err := proto.Unmarshal(body, &request); err != nil {
				t.Error(err)
			}
			for _, resource := range request.ResourceSpans {
				for _, scope := range resource.ScopeSpans {
					for _, span := range scope.Spans {
						spans[span.Name] = true
					}
				}
			}
		case "/v1/metrics":
			var request metricspb.ExportMetricsServiceRequest
			if err := proto.Unmarshal(body, &request); err != nil {
				t.Error(err)
			}
			for _, resource := range request.ResourceMetrics {
				for _, scope := range resource.ScopeMetrics {
					for _, metric := range scope.Metrics {
						metrics[metric.Name] = true
					}
				}
			}
		default:
			t.Errorf("unexpected collector path %q", r.URL.Path)
		}
	}))
	defer collector.Close()
	path := filepath.Join(t.TempDir(), "service.yaml")
	yaml := fmt.Sprintf("telemetry:\n  traces:\n    enabled: true\n    endpoint: %s/v1/traces\n  metrics:\n    enabled: true\n    endpoint: %s/v1/metrics\noutbound:\n  timeout: 2s\n  response_header_timeout: 1s\n", collector.URL, collector.URL)
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	options := config.Options{Files: []string{path}}
	tel, err := initTelemetry(context.Background(), options, "outbound-test", "test", "info", "json")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	cfg, err := config.Load[struct {
		Outbound outbound.HTTPConfig `koanf:"outbound"`
	}](options)
	if err != nil {
		t.Fatal(err)
	}
	hc, err := cfg.Outbound.HTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	if hc.Timeout != 2*time.Second {
		t.Fatalf("YAML client timeout = %v", hc.Timeout)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Traceparent") == "" || r.Header.Get(contract.HeaderServiceAuthorization) != "Bearer test-service" {
			t.Error("client failed to propagate trace or service credential")
		}
		if r.URL.Path == "/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: 7\n\n")
			return
		}
		_, _ = io.WriteString(w, "{}")
	}))
	defer server.Close()
	hc.Transport.(*http.Transport).TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	secure := contract.SecureClientOptions{Audience: "fixture", HTTPClient: hc,
		Credentials: contract.ServiceCredentialProviderFunc(func(context.Context, contract.ServiceScope) (contract.ServiceCredential, error) {
			return contract.ServiceCredential{Token: "test-service", ExpiresAt: time.Now().Add(time.Hour)}, nil
		})}
	client, err := contract.NewSecureHTTPClient(server.URL, secure)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	response, err := client.Get(server.URL + "/unary")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	stream, err := httpserver.DialSecureSSE[struct{}, int](context.Background(), server.URL+"/events",
		httpserver.SSEClientConfig{System: "fixture", Method: "Watch", Stream: contract.StreamConfig{
			MaxDuration: time.Second, CloseTimeout: time.Second}, MaxRequestBodyBytes: 1024, MaxEventBytes: 1024}, secure, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if event, err := stream.Recv(context.Background()); err != nil || event.Data != 7 {
		t.Fatalf("Recv = (%+v, %v)", event, err)
	}
	if _, err := stream.Recv(context.Background()); err != io.EOF {
		t.Fatalf("terminal = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tel.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"HTTP GET", "HTTP POST", "fixture.Watch"} {
		if !spans[name] {
			t.Errorf("YAML-configured collector missing automatic span %q; got %v", name, spans)
		}
	}
	if !metrics["appkit.http.client.request"] || !metrics["appkit.http.client.request.duration"] {
		t.Errorf("YAML-configured collector missing outbound RED metrics: %v", metrics)
	}
}
