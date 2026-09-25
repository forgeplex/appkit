package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/forgeplex/appkit/apperr"
)

func TestLoggerLevelFilter(t *testing.T) {
	tests := []struct {
		name    string
		cfgLvl  string
		logAt   slog.Level
		emitted bool
	}{
		{"debug 级放行 debug", "debug", slog.LevelDebug, true},
		{"info 级过滤 debug", "info", slog.LevelDebug, false},
		{"空级别等价 info：放行 info", "", slog.LevelInfo, true},
		{"空级别等价 info：过滤 debug", "", slog.LevelDebug, false},
		{"warn 级过滤 info", "warn", slog.LevelInfo, false},
		{"warn 级放行 warn", "warn", slog.LevelWarn, true},
		{"error 级过滤 warn", "error", slog.LevelWarn, false},
		{"error 级放行 error", "error", slog.LevelError, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger, err := newLogger(&buf, Config{LogLevel: tt.cfgLvl})
			if err != nil {
				t.Fatalf("newLogger: %v", err)
			}
			logger.Log(context.Background(), tt.logAt, "probe-message")
			if got := strings.Contains(buf.String(), "probe-message"); got != tt.emitted {
				t.Errorf("emitted = %v, want %v（输出：%q）", got, tt.emitted, buf.String())
			}
		})
	}
}

func TestLoggerFormat(t *testing.T) {
	tests := []struct {
		name     string
		format   string
		wantJSON bool
	}{
		{"json", "json", true},
		{"空值默认 json", "", true},
		{"text", "text", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger, err := newLogger(&buf, Config{LogFormat: tt.format})
			if err != nil {
				t.Fatalf("newLogger: %v", err)
			}
			logger.Info("hello")
			isJSON := json.Valid(bytes.TrimSpace(buf.Bytes()))
			if isJSON != tt.wantJSON {
				t.Errorf("json.Valid = %v, want %v（输出：%q）", isJSON, tt.wantJSON, buf.String())
			}
		})
	}
}

func TestInitConfigErrors(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"非法 LogLevel", Config{LogLevel: "verbose"}},
		{"非法 LogFormat", Config{LogFormat: "xml"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envEndpoint, "")
			_, err := Init(context.Background(), tt.cfg)
			if err == nil {
				t.Fatal("期望 Init 报错")
			}
			if !apperr.Is(err, apperr.CodeInvalidArgument) {
				t.Errorf("错误码不是 INVALID_ARGUMENT: %v", err)
			}
		})
	}
}

func TestTraceIDInjection(t *testing.T) {
	var buf bytes.Buffer
	logger, err := newLogger(&buf, Config{})
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}

	// 只有 SDK 的 tracer 才产生有效 SpanContext（noop tracer 的 IsValid 为 false）。
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("telemetry_test").Start(context.Background(), "op")
	defer span.End()

	logger.InfoContext(ctx, "in-span")
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("解析日志 JSON: %v（输出：%q）", err, buf.String())
	}
	sc := span.SpanContext()
	if rec["trace_id"] != sc.TraceID().String() {
		t.Errorf("trace_id = %v, want %s", rec["trace_id"], sc.TraceID())
	}
	if rec["span_id"] != sc.SpanID().String() {
		t.Errorf("span_id = %v, want %s", rec["span_id"], sc.SpanID())
	}

	// 派生 logger（WithAttrs）必须保留注入行为。
	buf.Reset()
	logger.With("k", "v").InfoContext(ctx, "derived")
	if !strings.Contains(buf.String(), sc.TraceID().String()) {
		t.Errorf("With 派生后丢失 trace_id（输出：%q）", buf.String())
	}

	// 无 span 的 ctx 不得出现 trace_id。
	buf.Reset()
	logger.InfoContext(context.Background(), "no-span")
	if strings.Contains(buf.String(), "trace_id") {
		t.Errorf("无 span 时不应有 trace_id（输出：%q）", buf.String())
	}
}

func TestInitNoEndpoint(t *testing.T) {
	t.Setenv(envEndpoint, "")
	prevTP := otel.GetTracerProvider()
	prevMP := otel.GetMeterProvider()

	tm, err := Init(context.Background(), Config{ServiceName: "svc", Env: "test"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if tm.Logger == nil {
		t.Fatal("Logger 为 nil")
	}
	if tm.traces != nil || tm.metrics != nil {
		t.Error("无 endpoint 时不应装 SDK")
	}
	if otel.GetTracerProvider() != prevTP {
		t.Error("无 endpoint 时不应改动全局 TracerProvider")
	}
	if otel.GetMeterProvider() != prevMP {
		t.Error("无 endpoint 时不应改动全局 MeterProvider")
	}
	if err := tm.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown 应为空操作: %v", err)
	}
}

func TestInitWithEndpoint(t *testing.T) {
	// 假 collector：OTLP HTTP 对 2xx 空响应体按成功处理。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(envEndpoint, srv.URL)

	prevTP := otel.GetTracerProvider()
	prevMP := otel.GetMeterProvider()
	prevProp := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetMeterProvider(prevMP)
		otel.SetTextMapPropagator(prevProp)
	})

	tm, err := Init(context.Background(), Config{ServiceName: "svc", Env: "test"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if tm.traces == nil || tm.metrics == nil {
		t.Fatal("有 endpoint 时应装 trace/metric SDK")
	}
	if otel.GetTracerProvider() != trace.TracerProvider(tm.traces) {
		t.Error("全局 TracerProvider 未指向 SDK provider")
	}
	fields := otel.GetTextMapPropagator().Fields()
	if !contains(fields, "traceparent") {
		t.Errorf("propagator 缺 W3C traceparent 字段: %v", fields)
	}

	// 记一个 span 走完导出路径，Shutdown 应 flush 成功。
	_, span := tm.traces.Tracer("telemetry_test").Start(context.Background(), "op")
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tm.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func TestInitWithExportConfigUsesIndependentYAMLEndpoints(t *testing.T) {
	t.Setenv(envEndpoint, "")
	t.Setenv(envTraceEndpoint, "")
	t.Setenv(envMetricEndpoint, "")
	restoreGlobalTelemetry(t)

	var traceRequests, metricRequests atomic.Int64
	traceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			t.Errorf("trace endpoint path = %q, want /v1/traces", r.URL.Path)
		}
		traceRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer traceServer.Close()
	metricServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/metrics" {
			t.Errorf("metrics endpoint path = %q, want /v1/metrics", r.URL.Path)
		}
		metricRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer metricServer.Close()

	tm, err := InitWithExportConfig(context.Background(), Config{ServiceName: "svc", Env: "test"}, ExportConfig{
		Traces:  OTLPExportConfig{Enabled: true, Endpoint: traceServer.URL + "/v1/traces"},
		Metrics: OTLPExportConfig{Enabled: true, Endpoint: metricServer.URL + "/v1/metrics"},
	})
	if err != nil {
		t.Fatalf("InitWithExportConfig: %v", err)
	}
	if tm.traces == nil || tm.metrics == nil {
		t.Fatal("两个 signal 均启用时应分别装配 provider")
	}
	_, span := tm.traces.Tracer("telemetry_test").Start(context.Background(), "configured")
	span.End()
	counter, err := tm.metrics.Meter("telemetry_test").Int64Counter("configured.counter")
	if err != nil {
		t.Fatalf("create metric counter: %v", err)
	}
	counter.Add(context.Background(), 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tm.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if traceRequests.Load() == 0 || metricRequests.Load() == 0 {
		t.Fatalf("fake collectors requests: traces=%d metrics=%d", traceRequests.Load(), metricRequests.Load())
	}
}

func TestInitWithExportConfigOTELSignalEndpointOverridesGenericAndYAML(t *testing.T) {
	t.Setenv(envEndpoint, "")
	restoreGlobalTelemetry(t)

	var yamlRequests, genericRequests, signalRequests atomic.Int64
	newCollector := func(counter *atomic.Int64, path string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != path {
				t.Errorf("collector path = %q, want %q", r.URL.Path, path)
			}
			counter.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
	}
	yamlServer := newCollector(&yamlRequests, "/v1/traces")
	defer yamlServer.Close()
	genericServer := newCollector(&genericRequests, "/v1/traces")
	defer genericServer.Close()
	signalServer := newCollector(&signalRequests, "/v1/traces")
	defer signalServer.Close()
	t.Setenv(envEndpoint, genericServer.URL)
	t.Setenv(envTraceEndpoint, signalServer.URL+"/v1/traces")

	tm, err := InitWithExportConfig(context.Background(), Config{ServiceName: "svc"}, ExportConfig{
		Traces: OTLPExportConfig{Enabled: true, Endpoint: yamlServer.URL + "/v1/traces"},
	})
	if err != nil {
		t.Fatalf("InitWithExportConfig: %v", err)
	}
	_, span := tm.traces.Tracer("telemetry_test").Start(context.Background(), "precedence")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tm.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if signalRequests.Load() == 0 || genericRequests.Load() != 0 || yamlRequests.Load() != 0 {
		t.Fatalf("endpoint precedence requests: signal=%d generic=%d yaml=%d", signalRequests.Load(), genericRequests.Load(), yamlRequests.Load())
	}
}

func TestInitWithExportConfigDisabledDoesNotEnableFromEndpointEnv(t *testing.T) {
	restoreGlobalTelemetry(t)
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv(envEndpoint, srv.URL)
	t.Setenv(envTraceEndpoint, "")
	t.Setenv(envMetricEndpoint, "")

	tm, err := InitWithExportConfig(context.Background(), Config{ServiceName: "svc"}, ExportConfig{})
	if err != nil {
		t.Fatalf("InitWithExportConfig: %v", err)
	}
	if tm.traces != nil || tm.metrics != nil {
		t.Fatal("disabled signals were activated by endpoint environment variable")
	}
	if err := tm.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if requests.Load() != 0 {
		t.Errorf("disabled signals sent %d requests", requests.Load())
	}
}

func TestInitWithExportConfigRequiresEndpointForEnabledSignal(t *testing.T) {
	t.Setenv(envEndpoint, "")
	t.Setenv(envTraceEndpoint, "")
	t.Setenv(envMetricEndpoint, "")
	_, err := InitWithExportConfig(context.Background(), Config{ServiceName: "svc"}, ExportConfig{
		Traces: OTLPExportConfig{Enabled: true},
	})
	if !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("error = %v, want INVALID_ARGUMENT", err)
	}
}

func TestInitWithExportConfigRejectsCredentialEndpointWithoutEcho(t *testing.T) {
	t.Setenv(envEndpoint, "")
	t.Setenv(envTraceEndpoint, "")
	t.Setenv(envMetricEndpoint, "")
	for _, endpoint := range []string{
		"https://user:sensitive-value@collector.example/v1/traces",
		"https://collector.example/v1/traces?token=sensitive-value",
	} {
		_, err := InitWithExportConfig(context.Background(), Config{ServiceName: "svc"}, ExportConfig{
			Traces: OTLPExportConfig{Enabled: true, Endpoint: endpoint},
		})
		if !apperr.Is(err, apperr.CodeInvalidArgument) {
			t.Fatalf("endpoint %q error = %v, want INVALID_ARGUMENT", endpoint, err)
		}
		if strings.Contains(err.Error(), "sensitive-value") {
			t.Fatalf("error echoed credential component: %v", err)
		}
	}
}

func restoreGlobalTelemetry(t *testing.T) {
	t.Helper()
	prevTP := otel.GetTracerProvider()
	prevMP := otel.GetMeterProvider()
	prevProp := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetMeterProvider(prevMP)
		otel.SetTextMapPropagator(prevProp)
	})
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
