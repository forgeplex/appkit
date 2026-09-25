package contract_test

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forgeplex/appkit/contract"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var streamMetricReader = sdkmetric.NewManualReader()
var streamMetricTestSequence atomic.Uint64

func nextStreamMetricMethod(prefix string) string {
	return prefix + "_" + strconv.FormatUint(streamMetricTestSequence.Add(1), 10)
}

func TestMain(m *testing.M) {
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(streamMetricReader)))
	os.Exit(m.Run())
}

func TestOpenLocalRecordsLifecycleAndBackpressureMetrics(t *testing.T) {
	const system = "contract-stream-metrics-test"
	method := nextStreamMetricMethod("Lifecycle")
	started := make(chan struct{})
	stream, err := contract.OpenLocal[struct{}, string](
		context.Background(), system, method,
		contract.StreamConfig{MaxDuration: time.Second, CloseTimeout: time.Second},
		func(ctx context.Context, peer contract.Stream[string, struct{}]) error {
			close(started)
			return peer.Send(ctx, "first")
		},
	)
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	<-started

	base := map[string]string{
		"appkit.contract.system":           system,
		"appkit.contract.method":           method,
		"appkit.contract.stream.transport": "local",
	}
	if got := collectStreamSum(t, "appkit.contract.stream.active", base); got != 1 {
		t.Fatalf("active while producer awaits rendezvous = %d, want 1", got)
	}
	if got := collectStreamSum(t, "appkit.contract.stream.opened", base); got != 1 {
		t.Fatalf("opened = %d, want 1", got)
	}

	value, err := stream.Recv(context.Background())
	if err != nil || value != "first" {
		t.Fatalf("Recv = (%q, %v), want (first, nil)", value, err)
	}
	if _, err := stream.Recv(context.Background()); err != io.EOF {
		t.Fatalf("terminal Recv = %v, want io.EOF", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := collectStreamSum(t, "appkit.contract.stream.active", base); got != 0 {
		t.Fatalf("active after EOF/Close = %d, want 0", got)
	}
	if got := collectStreamSum(t, "appkit.contract.stream.closed", map[string]string{
		"appkit.contract.system": system, "appkit.contract.method": method,
		"appkit.contract.stream.transport": "local", "appkit.outcome": "ok",
	}); got != 1 {
		t.Fatalf("closed(ok) = %d, want 1", got)
	}
	direction := map[string]string{
		"appkit.contract.system": system, "appkit.contract.method": method,
		"appkit.contract.stream.transport": "local",
		"appkit.contract.stream.direction": "server_to_client",
	}
	if got := collectStreamSum(t, "appkit.contract.stream.message.sent", direction); got != 1 {
		t.Errorf("message.sent = %d, want 1", got)
	}
	if got := collectStreamSum(t, "appkit.contract.stream.message.received", direction); got != 1 {
		t.Errorf("message.received = %d, want 1", got)
	}
	terminal := map[string]string{
		"appkit.contract.system": system, "appkit.contract.method": method,
		"appkit.contract.stream.transport": "local", "appkit.outcome": "ok",
	}
	for name, attrs := range map[string]map[string]string{
		"appkit.contract.stream.duration":               terminal,
		"appkit.contract.stream.time_to_first_message":  direction,
		"appkit.contract.stream.send.backpressure.wait": direction,
	} {
		if got := collectStreamHistogramCount(t, name, attrs); got != 1 {
			t.Errorf("%s samples = %d, want 1", name, got)
		}
	}
}

func TestOpenLocalDoesNotRecordBackpressureWhenQueueHasRoom(t *testing.T) {
	const system = "contract-stream-metrics-test"
	method := nextStreamMetricMethod("NoBackpressure")
	sent := make(chan struct{})
	stream, err := contract.OpenLocal[struct{}, string](
		context.Background(), system, method,
		contract.StreamConfig{
			MaxDuration: time.Second, CloseTimeout: time.Second, QueueSize: 1,
		},
		func(ctx context.Context, peer contract.Stream[string, struct{}]) error {
			if err := peer.Send(ctx, "first"); err != nil {
				return err
			}
			close(sent)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("producer did not use the available buffer")
	}
	if _, err := stream.Recv(context.Background()); err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if _, err := stream.Recv(context.Background()); err != io.EOF {
		t.Fatalf("terminal Recv = %v, want io.EOF", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	want := map[string]string{
		"appkit.contract.system": system, "appkit.contract.method": method,
		"appkit.contract.stream.transport": "local",
		"appkit.contract.stream.direction": "server_to_client",
	}
	if count, found := findStreamHistogramCount(t,
		"appkit.contract.stream.send.backpressure.wait", want); found {
		t.Fatalf("backpressure samples = %d, want no sample for an available buffer", count)
	}
}

func TestOpenLocalRecordsEveryTerminalPathOnce(t *testing.T) {
	tests := []struct {
		name          string
		configure     func(*contract.StreamConfig)
		handlerError  error
		cancelRoot    bool
		closeStream   bool
		wantOutcome   string
		wantErrorCode string
		wantCanceled  int64
	}{
		{
			name: "handler_error", handlerError: errors.New("private cause"),
			wantOutcome: "error", wantErrorCode: "INTERNAL",
		},
		{
			name: "root_cancel", cancelRoot: true,
			wantOutcome: "canceled", wantErrorCode: "UNAVAILABLE", wantCanceled: 1,
		},
		{
			name: "max_duration", configure: func(cfg *contract.StreamConfig) {
				cfg.MaxDuration = 35 * time.Millisecond
			},
			wantOutcome: "timeout", wantErrorCode: "UNAVAILABLE",
		},
		{
			name: "idle_timeout", configure: func(cfg *contract.StreamConfig) {
				cfg.IdleTimeout = 35 * time.Millisecond
			},
			wantOutcome: "timeout", wantErrorCode: "UNAVAILABLE",
		},
		{
			name: "explicit_close", closeStream: true,
			wantOutcome: "canceled", wantErrorCode: "UNAVAILABLE", wantCanceled: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const system = "contract-stream-terminal-test"
			method := nextStreamMetricMethod("Terminal_" + tc.name)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{})
			cfg := contract.StreamConfig{MaxDuration: time.Second, CloseTimeout: time.Second}
			if tc.configure != nil {
				tc.configure(&cfg)
			}
			stream, err := contract.OpenLocal[struct{}, string](parent, system, method, cfg,
				func(ctx context.Context, _ contract.Stream[string, struct{}]) error {
					close(started)
					if tc.handlerError != nil {
						return tc.handlerError
					}
					<-ctx.Done()
					return ctx.Err()
				},
			)
			if err != nil {
				t.Fatalf("OpenLocal: %v", err)
			}
			<-started

			switch {
			case tc.cancelRoot:
				cancel()
			case tc.closeStream:
				if err := stream.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
			default:
				_, _ = stream.Recv(context.Background())
			}
			if err := stream.Close(); err != nil {
				t.Fatalf("Close after terminal: %v", err)
			}

			base := map[string]string{
				"appkit.contract.system":           system,
				"appkit.contract.method":           method,
				"appkit.contract.stream.transport": "local",
			}
			closedAttrs := map[string]string{
				"appkit.contract.system":           system,
				"appkit.contract.method":           method,
				"appkit.contract.stream.transport": "local",
				"appkit.outcome":                   tc.wantOutcome,
				"appkit.error.code":                tc.wantErrorCode,
			}
			if got := collectStreamSum(t, "appkit.contract.stream.active", base); got != 0 {
				t.Errorf("active after terminal = %d, want 0", got)
			}
			if got := collectStreamSum(t, "appkit.contract.stream.closed", closedAttrs); got != 1 {
				t.Errorf("closed = %d, want 1", got)
			}
			if tc.wantCanceled > 0 {
				if got := collectStreamSum(t, "appkit.contract.stream.canceled", closedAttrs); got != tc.wantCanceled {
					t.Errorf("canceled = %d, want %d", got, tc.wantCanceled)
				}
			}
			if got := collectStreamHistogramCount(t, "appkit.contract.stream.duration", closedAttrs); got != 1 {
				t.Errorf("duration samples = %d, want 1", got)
			}
		})
	}
}

func collectStreamMetrics(t *testing.T) []metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := streamMetricReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name == "github.com/forgeplex/appkit" {
			return sm.Metrics
		}
	}
	return nil
}

func collectStreamSum(t *testing.T, name string, want map[string]string) int64 {
	t.Helper()
	for _, m := range collectStreamMetrics(t) {
		if m.Name != name {
			continue
		}
		sum, ok := m.Data.(metricdata.Sum[int64])
		if !ok {
			t.Fatalf("%s data = %T, want int64 sum", name, m.Data)
		}
		for _, dp := range sum.DataPoints {
			if streamMetricAttrsMatch(dp.Attributes.ToSlice(), want) {
				return dp.Value
			}
		}
		t.Fatalf("%s has no point with attributes %v", name, want)
	}
	t.Fatalf("metric %q not found", name)
	return 0
}

func collectStreamHistogramCount(t *testing.T, name string, want map[string]string) uint64 {
	t.Helper()
	count, found := findStreamHistogramCount(t, name, want)
	if !found {
		t.Fatalf("%s has no point with attributes %v", name, want)
	}
	return count
}

func findStreamHistogramCount(t *testing.T, name string, want map[string]string) (uint64, bool) {
	t.Helper()
	for _, m := range collectStreamMetrics(t) {
		if m.Name != name {
			continue
		}
		hist, ok := m.Data.(metricdata.Histogram[float64])
		if !ok {
			t.Fatalf("%s data = %T, want float64 histogram", name, m.Data)
		}
		for _, dp := range hist.DataPoints {
			if streamMetricAttrsMatch(dp.Attributes.ToSlice(), want) {
				return dp.Count, true
			}
		}
		return 0, false
	}
	return 0, false
}

func streamMetricAttrsMatch(attrs []attribute.KeyValue, want map[string]string) bool {
	if len(attrs) != len(want) {
		return false
	}
	for _, attr := range attrs {
		if want[string(attr.Key)] != attr.Value.AsString() {
			return false
		}
	}
	return true
}
