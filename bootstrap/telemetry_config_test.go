package bootstrap

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/forgeplex/appkit/config"
)

func TestRuntimeTelemetryConfigPresenceAndValues(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		present bool
	}{
		{name: "missing block", yaml: "env: test\n"},
		{name: "empty block is explicit", yaml: "telemetry: {}\n", present: true},
		{name: "nested block is explicit", yaml: "telemetry:\n  traces:\n    enabled: false\n", present: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "service.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := config.Load[runtimeTelemetryConfig](config.Options{Files: []string{path}})
			if err != nil {
				t.Fatalf("load telemetry config: %v", err)
			}
			if (got.Telemetry != nil) != tt.present {
				t.Fatalf("Telemetry presence = %v, want %v", got.Telemetry != nil, tt.present)
			}
		})
	}
}

func TestRuntimeTelemetryConfigServiceEnvOverridesYAML(t *testing.T) {
	t.Setenv("APPKITTEST_TELEMETRY__TRACES__ENABLED", "true")
	t.Setenv("APPKITTEST_TELEMETRY__TRACES__ENDPOINT", "https://env.example/v1/traces")
	path := filepath.Join(t.TempDir(), "service.yaml")
	const source = `telemetry:
  traces:
    enabled: false
    endpoint: https://yaml.example/v1/traces
`
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := config.Load[runtimeTelemetryConfig](config.Options{
		Files:     []string{path},
		EnvPrefix: "APPKITTEST",
	})
	if err != nil {
		t.Fatalf("load telemetry config: %v", err)
	}
	if got.Telemetry == nil {
		t.Fatal("Telemetry 配置块丢失")
	}
	if !got.Telemetry.Traces.Enabled {
		t.Error("service-prefixed env 未覆盖 YAML enabled")
	}
	if got.Telemetry.Traces.Endpoint != "https://env.example/v1/traces" {
		t.Errorf("endpoint = %q, want service env override", got.Telemetry.Traces.Endpoint)
	}
}

func TestInitTelemetryAutomaticallyExportsFromServiceYAML(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	prevMP := otel.GetMeterProvider()
	prevProp := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetMeterProvider(prevMP)
		otel.SetTextMapPropagator(prevProp)
	})
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			t.Errorf("collector path = %q, want /v1/traces", r.URL.Path)
		}
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "service.yaml")
	source := fmt.Sprintf(`telemetry:
  traces:
    enabled: true
    endpoint: %s/v1/traces
  metrics:
    enabled: false
`, srv.URL)
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	options := config.Options{Files: []string{path}, EnvPrefix: "APPKITTEST"}
	tel, err := initTelemetry(context.Background(), options, "svc", "test", "info", "json")
	if err != nil {
		t.Fatalf("initTelemetry: %v", err)
	}
	_, span := otel.GetTracerProvider().Tracer("bootstrap_test").Start(context.Background(), "yaml-configured")
	span.End()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tel.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if requests.Load() == 0 {
		t.Fatal("service YAML 没有自动导出 trace 到 fake collector")
	}
}
