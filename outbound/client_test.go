package outbound

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forgeplex/appkit/apperr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestNewClientRejectsUnsafeTLSAndNegativeTimeout(t *testing.T) {
	for name, options := range map[string]Options{
		"negative timeout": {Timeout: -time.Second},
		"insecure TLS":     {Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}},
		"old TLS":          {Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS10}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewClient(options); !apperr.Is(err, apperr.CodeInvalidArgument) {
				t.Fatalf("NewClient error = %v, want INVALID_ARGUMENT", err)
			}
		})
	}
}

func TestDoRefusesCredentialsAndRedirects(t *testing.T) {
	var destinationCalls atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationCalls.Add(1)
	}))
	defer destination.Close()
	redirect := httptest.NewServer(http.RedirectHandler(destination.URL, http.StatusFound))
	defer redirect.Close()

	client, err := NewClient(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	if _, err := client.Do(mustRequest(t, "GET", "http://user:secret@example.test/path")); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("credential URL error = %v", err)
	}
	if _, err := client.Do(mustRequest(t, "GET", redirect.URL)); !apperr.Is(err, apperr.CodePermissionDenied) {
		t.Fatalf("redirect error = %v, want PERMISSION_DENIED", err)
	}
	if got := destinationCalls.Load(); got != 0 {
		t.Fatalf("redirect destination calls = %d, want 0", got)
	}
}

func TestDoPropagatesTraceAndDoesNotRetry(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("traceparent"); got != "00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01" {
			t.Errorf("traceparent = %q", got)
		}
		http.Error(w, "retry must not happen", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client, err := NewClient(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:     [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext)
	resp, err := client.Do(mustRequest(t, "GET", srv.URL+"/private?token=secret").WithContext(ctx))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	if got := calls.Load(); got != 1 {
		t.Fatalf("server calls = %d, want exactly one", got)
	}
}

func TestDoClearsForgedTraceHeadersWithoutActiveSpan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{"Traceparent", "Tracestate", "Baggage"} {
			if got := r.Header.Get(name); got != "" {
				t.Errorf("%s = %q, want cleared", name, got)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	client, err := NewClient(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	req := mustRequest(t, "GET", srv.URL)
	req.Header.Set("Traceparent", "forged")
	req.Header.Set("Tracestate", "forged=value")
	req.Header.Set("Baggage", "tenant=secret")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
}

func TestSpanDoesNotCaptureURL(t *testing.T) {
	previous := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	client, err := NewClient(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	resp, err := client.Do(mustRequest(t, "GET", srv.URL+"/sensitive?token=secret"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	for _, attr := range spans[0].Attributes() {
		if strings.Contains(string(attr.Value.AsString()), "sensitive") || strings.Contains(string(attr.Value.AsString()), "secret") {
			t.Fatalf("span attribute leaked URL content: %s=%s", attr.Key, attr.Value.AsString())
		}
	}
}

func TestReadingResponseToEOFEndsSpanSuccessfully(t *testing.T) {
	previous := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	client, err := NewClient(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	resp, err := client.Do(mustRequest(t, "GET", srv.URL))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	if got := spans[0].Status().Code; got != codes.Unset {
		t.Fatalf("span status = %v, want Unset", got)
	}
}

func TestDoCancellationAndTransportErrorAreSanitized(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	client, err := NewClient(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Do(mustRequest(t, "GET", srv.URL+"/path?token=secret").WithContext(ctx))
		done <- err
	}()
	<-started
	cancel()
	err = <-done
	if !apperr.Is(err, apperr.CodeUnavailable) || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request error = %v", err)
	}
	if strings.Contains(err.Error(), "token=secret") || strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("transport error leaked request URL: %v", err)
	}
}

func mustRequest(t *testing.T, method, target string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
