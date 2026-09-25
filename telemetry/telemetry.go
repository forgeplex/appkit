// Package telemetry 统一初始化三信号：slog 日志、OpenTelemetry trace 与 metric。
//
// 日志始终可用。Init 为兼容旧服务，仍由 OTEL_EXPORTER_OTLP_ENDPOINT 门控
// trace/metric；Bootstrap 对显式 YAML 配置使用 InitWithExportConfig。
// 未启用 exporter 时全局 provider 保持默认 noop——本地开发无 collector 也零成本。
// Telemetry.Shutdown 应作为最后一个 OnStop 注册，保证其余关停钩子产生的
// span/metric 也能被 flush。
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"

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
	"github.com/forgeplex/appkit/internal/metrics"
)

// envEndpoint 在旧兼容路径中是 OTLP HTTP exporter 的开关；显式 ExportConfig
// 则把它作为地址覆盖，signal 专属变量仍由 exporter 按标准语义优先解析。
const envEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"

const (
	envTraceEndpoint  = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	envMetricEndpoint = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
)

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

// OTLPExportConfig 配置一个 OTLP/HTTP signal exporter。
// Endpoint 使用完整 URL；signal 专属或通用 OTEL endpoint 环境变量存在时，
// exporter 按 OpenTelemetry 规则优先使用环境变量。
type OTLPExportConfig struct {
	Enabled  bool
	Endpoint string
}

// ExportConfig 是 traces 与 metrics 的运行时导出配置。
// Bootstrap 从服务配置文件构造它；业务服务通常不应直接初始化 exporter。
type ExportConfig struct {
	Traces  OTLPExportConfig
	Metrics OTLPExportConfig
}

// Telemetry 持有初始化产物。字段在 Init 返回后不再变更，可并发使用。
type Telemetry struct {
	// Logger 输出到 stdout；ctx 携带有效 span 时自动附加 trace_id/span_id。
	Logger *slog.Logger

	traces  *sdktrace.TracerProvider
	metrics *sdkmetric.MeterProvider
}

// Init 保留既有兼容行为：构造日志器；若 OTEL_EXPORTER_OTLP_ENDPOINT 非空，
// 同时装配 OTLP HTTP trace/metric SDK；否则全局 provider 保持默认 noop。
func Init(ctx context.Context, cfg Config) (*Telemetry, error) {
	enabled := os.Getenv(envEndpoint) != ""
	return InitWithExportConfig(ctx, cfg, ExportConfig{
		Traces:  OTLPExportConfig{Enabled: enabled},
		Metrics: OTLPExportConfig{Enabled: enabled},
	})
}

// InitWithExportConfig 按显式运行时配置构造日志器及 OTLP/HTTP signal exporter。
// 配置块存在时由调用方决定 Enabled；OTEL endpoint 环境变量仅覆盖地址，不会
// 打开 Enabled=false 的信号。启用的信号若没有有效 endpoint 则 fail-fast。
func InitWithExportConfig(ctx context.Context, cfg Config, exporters ExportConfig) (*Telemetry, error) {
	logger, err := newLogger(os.Stdout, cfg)
	if err != nil {
		return nil, err
	}
	t := &Telemetry{Logger: logger}

	if !exporters.Traces.Enabled && !exporters.Metrics.Enabled {
		return t, nil
	}

	res, err := newResource(cfg)
	if err != nil {
		return nil, err
	}
	if exporters.Traces.Enabled {
		texp, err := newTraceExporter(ctx, exporters.Traces)
		if err != nil {
			return nil, err
		}
		t.traces = sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(texp),
			sdktrace.WithResource(res),
		)
	}
	if exporters.Metrics.Enabled {
		mexp, err := newMetricExporter(ctx, exporters.Metrics)
		if err != nil {
			// trace exporter 已创建成功时也要释放其连接资源。
			if t.traces != nil {
				_ = t.traces.Shutdown(ctx)
			}
			return nil, err
		}
		t.metrics = sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(mexp)),
			sdkmetric.WithResource(res),
		)
	}

	if t.traces != nil {
		otel.SetTracerProvider(t.traces)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{}, propagation.Baggage{},
		))
	}
	if t.metrics != nil {
		otel.SetMeterProvider(t.metrics)
		metrics.Initialize()
	}
	return t, nil
}

func newTraceExporter(ctx context.Context, cfg OTLPExportConfig) (sdktrace.SpanExporter, error) {
	endpoint, fromEnv := configuredEndpoint(cfg.Endpoint, envTraceEndpoint)
	if err := validateEndpoint("traces", endpoint, fromEnv); err != nil {
		return nil, err
	}
	var opts []otlptracehttp.Option
	if !fromEnv {
		opts = append(opts, otlptracehttp.WithEndpointURL(endpoint))
	}
	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("telemetry: 创建 OTLP trace exporter: %w", err))
	}
	return exporter, nil
}

func newMetricExporter(ctx context.Context, cfg OTLPExportConfig) (*otlpmetrichttp.Exporter, error) {
	endpoint, fromEnv := configuredEndpoint(cfg.Endpoint, envMetricEndpoint)
	if err := validateEndpoint("metrics", endpoint, fromEnv); err != nil {
		return nil, err
	}
	var opts []otlpmetrichttp.Option
	if !fromEnv {
		opts = append(opts, otlpmetrichttp.WithEndpointURL(endpoint))
	}
	exporter, err := otlpmetrichttp.New(ctx, opts...)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("telemetry: 创建 OTLP metric exporter: %w", err))
	}
	return exporter, nil
}

// configuredEndpoint leaves standard OTEL endpoint variables to the exporter,
// which applies signal-specific-over-generic precedence and its path semantics.
func configuredEndpoint(configured, signalEnv string) (string, bool) {
	if os.Getenv(signalEnv) != "" || os.Getenv(envEndpoint) != "" {
		endpoint := os.Getenv(signalEnv)
		if endpoint == "" {
			endpoint = os.Getenv(envEndpoint)
		}
		return endpoint, true
	}
	return configured, false
}

func validateEndpoint(signal, endpoint string, fromEnv bool) error {
	if endpoint == "" {
		return apperr.InvalidArgument("telemetry.%s.enabled=true，但缺少 endpoint（配置 telemetry.%s.endpoint 或 OTEL_EXPORTER_OTLP_%s_ENDPOINT / OTEL_EXPORTER_OTLP_ENDPOINT）", signal, signal, strings.ToUpper(signal))
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return apperr.InvalidArgument("telemetry.%s.endpoint 必须是有效的 http/https URL（值来自 %s）", signal, endpointSource(fromEnv))
	}
	return nil
}

func endpointSource(fromEnv bool) string {
	if fromEnv {
		return "环境变量"
	}
	return "配置文件"
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

// newLogger 单独拆出 writer 参数以便测试注入缓冲区；初始化入口固定传 os.Stdout。
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
