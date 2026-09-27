package outbound

import (
	"net"
	"net/http"
	"time"

	"github.com/forgeplex/appkit/apperr"
)

// HTTPConfig is the shared YAML-decodable HTTP connection budget for outbound,
// unary contract, SSE and WebSocket clients. Zero fields retain standard-library
// transport defaults. Timeout bounds the entire HTTP response body; leave it zero
// for streams and use their explicit stream budgets instead. Credentials and TLS
// trust are supplied separately by trusted application code, never by this config.
type HTTPConfig struct {
	Timeout               time.Duration `koanf:"timeout" yaml:"timeout"`
	DialTimeout           time.Duration `koanf:"dial_timeout" yaml:"dial_timeout"`
	TLSHandshakeTimeout   time.Duration `koanf:"tls_handshake_timeout" yaml:"tls_handshake_timeout"`
	ResponseHeaderTimeout time.Duration `koanf:"response_header_timeout" yaml:"response_header_timeout"`
	IdleConnTimeout       time.Duration `koanf:"idle_conn_timeout" yaml:"idle_conn_timeout"`
	MaxIdleConns          int           `koanf:"max_idle_conns" yaml:"max_idle_conns"`
	MaxIdleConnsPerHost   int           `koanf:"max_idle_conns_per_host" yaml:"max_idle_conns_per_host"`
	MaxConnsPerHost       int           `koanf:"max_conns_per_host" yaml:"max_conns_per_host"`
}

// HTTPClient constructs an independent standard transport for an adapter to
// validate and instrument. Pass the result to SecureClientOptions.HTTPClient or
// a generated client; this configuration-only helper does not add credentials,
// instrumentation or redirect policy. Trusted TLS roots can be set on its
// *http.Transport before handing it to an adapter. Callers must close idle
// connections when the client is no longer used.
func (cfg HTTPConfig) HTTPClient() (*http.Client, error) {
	if cfg.Timeout < 0 || cfg.DialTimeout < 0 || cfg.TLSHandshakeTimeout < 0 ||
		cfg.ResponseHeaderTimeout < 0 || cfg.IdleConnTimeout < 0 || cfg.MaxIdleConns < 0 ||
		cfg.MaxIdleConnsPerHost < 0 || cfg.MaxConnsPerHost < 0 {
		return nil, apperr.InvalidArgument("outbound HTTP budgets must not be negative")
	}
	transport, err := cloneTransport(nil)
	if err != nil {
		return nil, err
	}
	if cfg.DialTimeout != 0 {
		transport.DialContext = (&net.Dialer{Timeout: cfg.DialTimeout, KeepAlive: 30 * time.Second}).DialContext
	}
	if cfg.TLSHandshakeTimeout != 0 {
		transport.TLSHandshakeTimeout = cfg.TLSHandshakeTimeout
	}
	if cfg.ResponseHeaderTimeout != 0 {
		transport.ResponseHeaderTimeout = cfg.ResponseHeaderTimeout
	}
	if cfg.IdleConnTimeout != 0 {
		transport.IdleConnTimeout = cfg.IdleConnTimeout
	}
	if cfg.MaxIdleConns != 0 {
		transport.MaxIdleConns = cfg.MaxIdleConns
	}
	if cfg.MaxIdleConnsPerHost != 0 {
		transport.MaxIdleConnsPerHost = cfg.MaxIdleConnsPerHost
	}
	if cfg.MaxConnsPerHost != 0 {
		transport.MaxConnsPerHost = cfg.MaxConnsPerHost
	}
	return &http.Client{Transport: transport, Timeout: cfg.Timeout}, nil
}
