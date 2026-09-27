// Package outbound provides controlled clients for outbound HTTP calls.
package outbound

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/internal/metrics"
	"github.com/forgeplex/appkit/internal/outboundstate"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Options configures a reusable outbound HTTP client. A supplied transport is
// cloned; custom RoundTrippers and TLS bypasses are intentionally unsupported.
type Options struct {
	Timeout   time.Duration
	Transport *http.Transport
}

// Client wraps a reusable HTTP client without exposing mutable security policy.
type Client struct {
	http *http.Client
}

// NewClient builds a client with TLS verification, HTTP(S)-only requests,
// redirect refusal, and bounded telemetry. It adds no application-level retries;
// standard library transport behavior is otherwise unchanged.
func NewClient(options Options) (*Client, error) {
	if options.Timeout < 0 {
		return nil, apperr.InvalidArgument("outbound HTTP timeout must not be negative")
	}
	transport, err := cloneTransport(options.Transport)
	if err != nil {
		return nil, err
	}
	return &Client{http: &http.Client{
		Transport: InstrumentTransport(transport),
		Timeout:   options.Timeout,
		CheckRedirect: func(request *http.Request, _ []*http.Request) error {
			outboundstate.RefuseRedirect(request)
			return apperr.PermissionDenied("outbound HTTP redirects are forbidden")
		},
	}}, nil
}

func cloneTransport(base *http.Transport) (*http.Transport, error) {
	if base == nil {
		var ok bool
		base, ok = http.DefaultTransport.(*http.Transport)
		if !ok || base == nil {
			return nil, apperr.Internal(errors.New("default transport is not a standard HTTP transport"))
		}
	}
	transport := base.Clone()
	if transport.DialTLS != nil || transport.DialTLSContext != nil || len(transport.TLSNextProto) != 0 {
		return nil, apperr.InvalidArgument("outbound HTTP client forbids custom TLS dialers and protocol handlers")
	}
	if transport.Protocols != nil && transport.Protocols.UnencryptedHTTP2() {
		return nil, apperr.InvalidArgument("outbound HTTP client forbids unencrypted HTTP/2")
	}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	}
	tlsConfig := transport.TLSClientConfig
	if tlsConfig.InsecureSkipVerify || tlsConfig.KeyLogWriter != nil ||
		(tlsConfig.MinVersion != 0 && tlsConfig.MinVersion < tls.VersionTLS12) ||
		(tlsConfig.MaxVersion != 0 && tlsConfig.MaxVersion < tls.VersionTLS12) {
		return nil, apperr.InvalidArgument("outbound HTTP client requires verified TLS 1.2 or newer")
	}
	if tlsConfig.MinVersion == 0 {
		tlsConfig.MinVersion = tls.VersionTLS12
	}
	if tlsConfig.RootCAs != nil {
		tlsConfig.RootCAs = tlsConfig.RootCAs.Clone()
	}
	return transport, nil
}

// Do performs one request. The request context controls cancellation and
// deadline; response bodies remain the caller's responsibility to close.
func (c *Client) Do(request *http.Request) (*http.Response, error) {
	if c == nil || c.http == nil || request == nil || request.URL == nil {
		return nil, apperr.InvalidArgument("outbound HTTP client and request are required")
	}
	u := request.URL
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" || u.Fragment != "" {
		return nil, apperr.InvalidArgument("outbound HTTP request requires an HTTP(S) URL without credentials or fragment")
	}
	resp, err := c.http.Do(request)
	if err != nil {
		if apperr.Is(err, apperr.CodePermissionDenied) {
			return nil, apperr.PermissionDenied("outbound HTTP redirects are forbidden")
		}
		return nil, apperr.Unavailable(safeTransportError{cause: err})
	}
	return resp, nil
}

// CloseIdleConnections releases pooled connections owned by this client.
func (c *Client) CloseIdleConnections() {
	if c != nil && c.http != nil {
		c.http.CloseIdleConnections()
	}
}

type safeTransportError struct{ cause error }

func (e safeTransportError) Error() string { return "outbound HTTP transport failed" }
func (e safeTransportError) Unwrap() error { return e.cause }

type instrumentedTransport struct{ base http.RoundTripper }

// InstrumentTransport adds bounded HTTP client spans and metrics without
// changing the base transport's security, timeout or redirect policy. A nil
// transport uses http.DefaultTransport; wrapping its own result is idempotent.
// It propagates only W3C trace context, never baggage or incoming trace headers.
// Ordinary spans end at response-body EOF/Close; an HTTP 101 span ends at the
// handshake and leaves the upgraded bidirectional body unchanged.
func InstrumentTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if _, ok := base.(*instrumentedTransport); ok {
		return base
	}
	return &instrumentedTransport{base: base}
}

func (t *instrumentedTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t *instrumentedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil {
		return nil, apperr.InvalidArgument("outbound HTTP request is required")
	}
	method := normalizeMethod(request.Method)
	ctx, span := otel.Tracer("github.com/forgeplex/appkit/outbound").Start(
		request.Context(), "HTTP "+method, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("http.request.method", method)),
	)
	started := time.Now()
	state := &outboundstate.State{}
	ctx = outboundstate.WithContext(ctx, state)
	cloned := request.Clone(ctx)
	if cloned.Header == nil {
		cloned.Header = make(http.Header)
	}
	for name := range cloned.Header {
		switch strings.ToLower(name) {
		case "traceparent", "tracestate", "baggage":
			delete(cloned.Header, name)
		}
	}
	propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(cloned.Header))
	response, err := t.base.RoundTrip(cloned)
	statusClass := "other"
	failedStatus := false
	if response != nil {
		// Keep the actual request's sanitized headers, adding only observation
		// state to an independent copy. CheckRedirect can then report refusal
		// even when http.Client.Timeout hides the instrumented response body.
		sent := response.Request
		if sent == nil {
			sent = cloned
		}
		response.Request = sent.WithContext(outboundstate.WithContext(sent.Context(), state))
		failedStatus = response.StatusCode >= http.StatusBadRequest
		if response.StatusCode >= 100 && response.StatusCode < 600 {
			statusClass = fmt.Sprintf("%dxx", response.StatusCode/100)
		}
		span.SetAttributes(attribute.Int("http.response.status_code", response.StatusCode))
	}
	var finishOnce sync.Once
	finish := func(callErr error) {
		finishOnce.Do(func() {
			if callErr == nil && state.RedirectRefused.Load() {
				callErr = apperr.PermissionDenied("outbound HTTP redirects are forbidden")
			}
			if callErr == nil && failedStatus {
				callErr = errors.New("outbound HTTP response failed")
			}
			if callErr != nil {
				span.RecordError(errors.New("outbound HTTP request failed"))
				span.SetStatus(codes.Error, "outbound HTTP request failed")
			}
			metrics.HTTPClientCall(ctx, method, statusClass, callErr, started)
			span.End()
		})
	}
	if err != nil {
		finish(err)
	} else if response == nil || response.Body == nil || response.StatusCode == http.StatusSwitchingProtocols {
		finish(nil)
	} else {
		response.Body = &observedBody{ReadCloser: response.Body, finish: finish}
	}
	return response, err
}

type observedBody struct {
	io.ReadCloser
	finish func(error)
}

func (body *observedBody) Read(p []byte) (int, error) {
	n, err := body.ReadCloser.Read(p)
	if err != nil {
		if errors.Is(err, io.EOF) {
			body.finish(nil)
		} else {
			body.finish(err)
		}
	}
	return n, err
}

func (body *observedBody) Close() error {
	err := body.ReadCloser.Close()
	body.finish(err)
	return err
}

func normalizeMethod(method string) string {
	switch strings.ToUpper(method) {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE":
		return strings.ToUpper(method)
	default:
		return "OTHER"
	}
}

var _ http.RoundTripper = (*instrumentedTransport)(nil)
