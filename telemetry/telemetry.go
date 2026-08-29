// Package telemetry 统一初始化三信号：slog 日志、OpenTelemetry trace 与 metric。
//
// 日志始终可用；trace/metric SDK 只在 OTEL_EXPORTER_OTLP_ENDPOINT 存在时装配，
// 否则全局 provider 保持默认 noop——本地开发无 collector 也零成本。
// Telemetry.Shutdown 应作为最后一个 OnStop 注册，保证其余关停钩子产生的
// span/metric 也能被 flush。
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/forgeplex/appkit/apperr"
)

// envEndpoint 是 OTLP HTTP exporter 的开关：存在即装 SDK，端点解析交给 exporter 本身
// （含 http/https scheme 与各信号专属变量的标准语义）。
const envEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"

// Config 是遥测初始化配置。零值可用：info 级 JSON 日志、不装 trace/metric SDK。
type Config struct {
	// ServiceName 写入 resource 的 service.name。
	ServiceName string
	// Env 写入 resource 的 deployment.environment（如 dev/staging/prod）。
	Env string
	// LogLevel 取 debug|info|warn|error，空值等价 info。
	LogLevel string
	// LogFormat 取 json|text，空值等价 json。
	LogFormat string
}

// Telemetry 持有初始化产物。字段在 Init 返回后不再变更，可并发使用。
type Telemetry struct {
	// Logger 输出到 stdout；ctx 携带有效 span 时自动附加 trace_id/span_id。
	Logger *slog.Logger

	traces  *sdktrace.TracerProvider
	metrics *sdkmetric.MeterProvider
}

// Init 构造日志器；若 OTEL_EXPORTER_OTLP_ENDPOINT 存在，再装配 OTLP HTTP
// trace/metric SDK 并设置全局 provider 与 W3C propagator。
func Init(ctx context.Context, cfg Config) (*Telemetry, error) {
	logger, err := newLogger(os.Stdout, cfg)
	if err != nil {
		return nil, err
	}
	t := &Telemetry{Logger: logger}

	if os.Getenv(envEndpoint) == "" {
		return t, nil
	}

	res, err := newResource(cfg)
	if err != nil {
		return nil, err
	}
	texp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("telemetry: 创建 OTLP trace exporter: %w", err))
	}
	mexp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		// trace exporter 已创建成功，避免泄漏其连接资源。
		_ = texp.Shutdown(ctx)
		return nil, apperr.Internal(fmt.Errorf("telemetry: 创建 OTLP metric exporter: %w", err))
	}

	t.traces = sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(texp),
		sdktrace.WithResource(res),
	)
	t.metrics = sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(mexp)),
		sdkmetric.WithResource(res),
	)

	otel.SetTracerProvider(t.traces)
	otel.SetMeterProvider(t.metrics)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	return t, nil
}

// Shutdown flush 缓冲中的 span/metric 后关闭 SDK；未装 SDK 时是空操作。
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var errs []error
	if t.traces != nil {
		if err := t.traces.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("telemetry: 关闭 tracer provider: %w", err))
		}
	}
	if t.metrics != nil {
		if err := t.metrics.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("telemetry: 关闭 meter provider: %w", err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return apperr.Internal(err)
	}
	return nil
}

// newLogger 单独拆出 writer 参数以便测试注入缓冲区；Init 固定传 os.Stdout。
func newLogger(w io.Writer, cfg Config) (*slog.Logger, error) {
	level, err := parseLevel(cfg.LogLevel)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	switch cfg.LogFormat {
	case "", "json":
		h = slog.NewJSONHandler(w, opts)
	case "text":
		h = slog.NewTextHandler(w, opts)
	default:
		return nil, apperr.InvalidArgument("telemetry: 未知 LogFormat %q（可用 json|text）", cfg.LogFormat)
	}
	return slog.New(spanHandler{h}), nil
}

func parseLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, apperr.InvalidArgument("telemetry: 未知 LogLevel %q（可用 debug|info|warn|error）", s)
	}
}

// spanHandler 在 ctx 携带有效 span 时向日志记录附加 trace_id/span_id，
// 让日志与 trace 可互查。对无 span 的记录零开销转发。
type spanHandler struct{ slog.Handler }

func (h spanHandler) Handle(ctx context.Context, rec slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		// Record 的属性后备数组可能被多个副本共享，追加前必须 Clone。
		rec = rec.Clone()
		rec.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, rec)
}

// WithAttrs/WithGroup 必须保持包装，否则派生 logger 会丢掉 trace 注入。
func (h spanHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return spanHandler{h.Handler.WithAttrs(attrs)}
}

func (h spanHandler) WithGroup(name string) slog.Handler {
	return spanHandler{h.Handler.WithGroup(name)}
}

// newResource 以 Default 为底合并服务标识；自身 schemaless，规避 schema URL 冲突。
func newResource(cfg Config) (*resource.Resource, error) {
	var attrs []attribute.KeyValue
	if cfg.ServiceName != "" {
		attrs = append(attrs, attribute.String("service.name", cfg.ServiceName))
	}
	if cfg.Env != "" {
		attrs = append(attrs, attribute.String("deployment.environment", cfg.Env))
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(attrs...))
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("telemetry: 构造 resource: %w", err))
	}
	return res, nil
}
