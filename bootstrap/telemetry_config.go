package bootstrap

import (
	"context"
	"fmt"

	"github.com/forgeplex/appkit/config"
	"github.com/forgeplex/appkit/telemetry"
)

// runtimeTelemetryConfig deliberately stays private: adding fields to exported
// bootstrap or telemetry config structs could break downstream unkeyed literals.
type runtimeTelemetryConfig struct {
	Telemetry *telemetryRuntimeSettings `koanf:"telemetry"`
}

type telemetryRuntimeSettings struct {
	Traces  telemetrySignalSettings `koanf:"traces"`
	Metrics telemetrySignalSettings `koanf:"metrics"`
}

type telemetrySignalSettings struct {
	Enabled  bool   `koanf:"enabled"`
	Endpoint string `koanf:"endpoint"`
}

func initTelemetry(ctx context.Context, options config.Options, service string, env, level, format string) (*telemetry.Telemetry, error) {
	runtimeConfig, err := config.Load[runtimeTelemetryConfig](options)
	if err != nil {
		return nil, fmt.Errorf("加载遥测配置: %w", err)
	}
	cfg := telemetry.Config{
		ServiceName: service,
		Env:         env,
		LogLevel:    level,
		LogFormat:   format,
	}
	if runtimeConfig.Telemetry == nil {
		// No AppKit telemetry block means this service has not migrated; keep the
		// historical OTEL_EXPORTER_OTLP_ENDPOINT activation behavior.
		return telemetry.Init(ctx, cfg)
	}

	configured := runtimeConfig.Telemetry
	return telemetry.InitWithExportConfig(ctx, cfg, telemetry.ExportConfig{
		Traces: telemetry.OTLPExportConfig{
			Enabled:  configured.Traces.Enabled,
			Endpoint: configured.Traces.Endpoint,
		},
		Metrics: telemetry.OTLPExportConfig{
			Enabled:  configured.Metrics.Enabled,
			Endpoint: configured.Metrics.Endpoint,
		},
	})
}
