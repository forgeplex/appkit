package outbound

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

var outboundMetricsReader = sdkmetric.NewManualReader()

func init() {
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(outboundMetricsReader)))
}

func TestInstrumentTransportHTTPResponseOutcomes(t *testing.T) {
	previous := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	for _, status := range []int{101, 204, 404, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			wantOutcome, wantSpanStatus := "ok", codes.Unset
			if status >= 400 {
				wantOutcome, wantSpanStatus = "error", codes.Error
			}
			statusClass := strconv.Itoa(status/100) + "xx"
			before := outboundRequestCount(t, statusClass, wantOutcome)
			transport := InstrumentTransport(roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Body: http.NoBody}, nil
			}))
			response, err := transport.RoundTrip(mustRequest(t, http.MethodGet, "https://example.test/private?token=secret"))
			if err != nil || response.StatusCode != status {
				t.Fatalf("HTTP response semantics changed: response=%v err=%v", response, err)
			}
			_ = response.Body.Close()
			spans := recorder.Ended()
			if got := spans[len(spans)-1].Status().Code; got != wantSpanStatus {
				t.Errorf("span status = %v, want %v", got, wantSpanStatus)
			}
			if got := outboundRequestCount(t, statusClass, wantOutcome); got != before+1 {
				t.Errorf("request counter = %d, want %d", got, before+1)
			}
		})
	}
}

func TestInstrumentTransportRetainsAllowedRedirectOutcome(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/destination" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Redirect(w, request, "/destination", http.StatusFound)
	}))
	defer server.Close()
	client := &http.Client{Transport: InstrumentTransport(nil), Timeout: time.Second}
	defer client.CloseIdleConnections()
	before := outboundRequestCount(t, "3xx", "ok")
	response, err := client.Do(mustRequest(t, http.MethodGet, server.URL))
	if err != nil || response.StatusCode != http.StatusNoContent {
		t.Fatalf("allowed redirect = %v, %v", response, err)
	}
	_ = response.Body.Close()
	if got := outboundRequestCount(t, "3xx", "ok"); got != before+1 {
		t.Fatalf("allowed redirect success metric = %d, want %d", got, before+1)
	}
}

func outboundRequestCount(t *testing.T, statusClass, outcome string) int64 {
	t.Helper()
	var data metricdata.ResourceMetrics
	if err := outboundMetricsReader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != "appkit.http.client.request" {
				continue
			}
			for _, point := range metric.Data.(metricdata.Sum[int64]).DataPoints {
				attributes := make(map[string]string)
				for _, attr := range point.Attributes.ToSlice() {
					attributes[string(attr.Key)] = attr.Value.AsString()
				}
				if len(attributes) != 3 {
					t.Fatalf("HTTP request metric has unbounded attributes: %v", attributes)
				}
				if attributes["http.request.method"] == http.MethodGet && attributes["http.response.status_class"] == statusClass && attributes["appkit.outcome"] == outcome {
					return point.Value
				}
			}
		}
	}
	return 0
}
