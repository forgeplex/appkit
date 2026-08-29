package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
