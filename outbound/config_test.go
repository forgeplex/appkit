package outbound

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/config"
)

func TestHTTPConfigLoadsYAMLAndBuildsIndependentStandardClients(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "client.yaml")
	if err := os.WriteFile(filename, []byte(`http:
  timeout: 3s
  dial_timeout: 400ms
  tls_handshake_timeout: 500ms
  response_header_timeout: 600ms
  idle_conn_timeout: 20s
  max_idle_conns: 24
  max_idle_conns_per_host: 8
  max_conns_per_host: 12
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load[struct {
		HTTP HTTPConfig `koanf:"http"`
	}](config.Options{Files: []string{filename}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.DialTimeout != 400*time.Millisecond {
		t.Fatalf("dial timeout = %s", cfg.HTTP.DialTimeout)
	}
	first, err := cfg.HTTP.HTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	second, err := cfg.HTTP.HTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.CloseIdleConnections)
	t.Cleanup(second.CloseIdleConnections)
	transport, ok := first.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want standard transport accepted by secure clients", first.Transport)
	}
	if first.Timeout != 3*time.Second || transport.TLSHandshakeTimeout != 500*time.Millisecond ||
		transport.ResponseHeaderTimeout != 600*time.Millisecond || transport.IdleConnTimeout != 20*time.Second ||
		transport.MaxIdleConns != 24 || transport.MaxIdleConnsPerHost != 8 || transport.MaxConnsPerHost != 12 || transport.DialContext == nil {
		t.Fatalf("HTTP config not applied: client=%+v transport=%+v", first, transport)
	}
	if transport == second.Transport || transport.TLSClientConfig == second.Transport.(*http.Transport).TLSClientConfig {
		t.Fatal("clients share mutable transport or TLS config")
	}
}

func TestHTTPConfigRejectsNegativeBudgets(t *testing.T) {
	for name, cfg := range map[string]HTTPConfig{
		"timeout": {Timeout: -1}, "dial": {DialTimeout: -1},
		"tls": {TLSHandshakeTimeout: -1}, "header": {ResponseHeaderTimeout: -1},
		"idle": {IdleConnTimeout: -1}, "idle connections": {MaxIdleConns: -1},
		"idle per host": {MaxIdleConnsPerHost: -1}, "connections per host": {MaxConnsPerHost: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := cfg.HTTPClient(); !apperr.Is(err, apperr.CodeInvalidArgument) {
				t.Fatalf("HTTPClient error = %v, want INVALID_ARGUMENT", err)
			}
		})
	}
}

func TestHTTPConfigResponseHeaderTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	client, err := (HTTPConfig{ResponseHeaderTimeout: 20 * time.Millisecond}).HTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	request := mustRequest(t, http.MethodGet, server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = client.Do(request.WithContext(ctx))
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("response header timeout = %v, context error = %v", err, ctx.Err())
	}
}
