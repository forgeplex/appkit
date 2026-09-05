package contract

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/callctx"
)

// HeaderServiceAuthorization carries a service credential, never a user token.
const HeaderServiceAuthorization = "X-Service-Authorization"

// ServiceScope is an explicit delegation request, not proof of authorization.
// The provider must authorize its audience and every nonempty scope before
// signing. Tenant and partition originate in the contract context whitelist;
// user actors, merchant IDs and incoming HTTP credentials are not forwarded.
type ServiceScope struct {
	Audience  string
	Partition string
	TenantID  string
}

// ServiceCredential contains a bare service JWT and its actual signed expiry.
// Providers must not return a user token or invent a later expiry for a token.
type ServiceCredential struct {
	Token     string
	ExpiresAt time.Time
}

// ServiceCredentialProvider obtains a fresh, explicitly authorized service
// credential for each HTTP attempt. Implementations must honor cancellation and
// be safe for concurrent calls. The transport does not cache credentials.
type ServiceCredentialProvider interface {
	ServiceCredential(context.Context, ServiceScope) (ServiceCredential, error)
}

// ServiceCredentialProviderFunc adapts an explicit provider function.
type ServiceCredentialProviderFunc func(context.Context, ServiceScope) (ServiceCredential, error)

func (f ServiceCredentialProviderFunc) ServiceCredential(ctx context.Context, scope ServiceScope) (ServiceCredential, error) {
	return f(ctx, scope)
}

// SecureClientOptions configures authenticated, origin-bound contract HTTP.
// Audience and Credentials are mandatory. HTTPClient may customize timeouts and
// a standard *http.Transport (including trusted root certificates). Custom round
// trippers, TLS dialers/protocol handlers, insecure TLS and cookie jars are
// rejected. Requests with trailers are rejected rather than forwarding hidden
// credentials or silently dropping a caller's integrity trailer. The supplied
// client/transport configuration is copied. Redirects
// are always refused; a supplied CheckRedirect callback is not used.
type SecureClientOptions struct {
	Audience    string
	Credentials ServiceCredentialProvider
	HTTPClient  *http.Client
}

// NewSecureHTTPClient constructs an HTTPS-only client bound to base's origin.
// Use it through a generated NewSecureClient so contract.Call also supplies the
// transaction guard, timeout and error normalization. Configuration and provider
// code are trusted application code, not a sandbox. Do not mutate the returned
// client's fields to replace its security transport or redirect policy.
func NewSecureHTTPClient(base string, opts SecureClientOptions) (*http.Client, error) {
	u, err := url.Parse(base)
	if err != nil || u == nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, apperr.InvalidArgument("secure contract base must be an HTTPS URL without query, fragment or credentials")
	}
	origin, err := secureOrigin(u)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.Audience) == "" || strings.TrimSpace(opts.Audience) != opts.Audience {
		return nil, apperr.InvalidArgument("secure contract audience is required")
	}
	if nilProvider(opts.Credentials) {
		return nil, apperr.InvalidArgument("secure contract credential provider is required")
	}
	hc := http.Client{}
	if opts.HTTPClient != nil {
		hc = *opts.HTTPClient
	}
	if hc.Jar != nil {
		return nil, apperr.InvalidArgument("secure contract client must not use a cookie jar")
	}
	baseTransport := hc.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	standard, ok := baseTransport.(*http.Transport)
	if !ok || standard == nil {
		return nil, apperr.InvalidArgument("secure contract client requires a standard HTTP transport")
	}
	transport := standard.Clone()
	if transport.DialTLS != nil || transport.DialTLSContext != nil || len(transport.TLSNextProto) != 0 {
		return nil, apperr.InvalidArgument("secure contract client forbids custom TLS dialers and protocol handlers")
	}
	if transport.Protocols != nil && transport.Protocols.UnencryptedHTTP2() {
		return nil, apperr.InvalidArgument("secure contract client forbids unencrypted HTTP/2")
	}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	config := transport.TLSClientConfig
	if config.InsecureSkipVerify || config.KeyLogWriter != nil {
		return nil, apperr.InvalidArgument("secure contract client requires TLS verification without key logging")
	}
	if (config.MinVersion != 0 && config.MinVersion < tls.VersionTLS12) ||
		(config.MaxVersion != 0 && config.MaxVersion < tls.VersionTLS12) {
		return nil, apperr.InvalidArgument("secure contract client requires TLS 1.2 or newer")
	}
	if config.ServerName != "" && !strings.EqualFold(config.ServerName, u.Hostname()) {
		return nil, apperr.InvalidArgument("secure contract TLS server name must match the origin")
	}
	config.ServerName = u.Hostname()
	if config.MinVersion == 0 {
		config.MinVersion = tls.VersionTLS12
	}
	if config.RootCAs != nil {
		config.RootCAs = config.RootCAs.Clone()
	}
	hc.Transport = &serviceTransport{base: transport, origin: origin, audience: opts.Audience, provider: opts.Credentials}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error {
		return apperr.PermissionDenied("secure contract redirects are forbidden")
	}
	return &hc, nil
}

func nilProvider(provider ServiceCredentialProvider) bool {
	if provider == nil {
		return true
	}
	v := reflect.ValueOf(provider)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}

func secureOrigin(u *url.URL) (string, error) {
	if u == nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.Fragment != "" {
		return "", apperr.InvalidArgument("secure contract requests require an HTTPS origin without credentials")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", apperr.InvalidArgument("secure contract origin has an invalid port")
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), strconv.Itoa(n)), nil
}

type serviceTransport struct {
	base     *http.Transport
	origin   string
	audience string
	provider ServiceCredentialProvider
}

func (t *serviceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, apperr.InvalidArgument("secure contract request is required")
	}
	handedOff := false
	defer func() {
		if !handedOff && req.Body != nil {
			_ = req.Body.Close()
		}
	}()
	if len(req.Trailer) != 0 {
		return nil, apperr.InvalidArgument("secure contract requests must not contain trailers")
	}
	for name := range req.Header {
		if strings.EqualFold(name, "Trailer") {
			return nil, apperr.InvalidArgument("secure contract requests must not declare trailers")
		}
	}
	origin, err := secureOrigin(req.URL)
	if err != nil || origin != t.origin {
		return nil, apperr.PermissionDenied("secure contract request origin does not match its binding")
	}
	if req.Host != "" {
		hostOrigin, err := secureOrigin(&url.URL{Scheme: "https", Host: req.Host})
		if err != nil || hostOrigin != t.origin {
			return nil, apperr.PermissionDenied("secure contract Host does not match its binding")
		}
	}
	ctx := Firewall(req.Context())
	if err := ctx.Err(); err != nil {
		return nil, apperr.Unavailable(err)
	}
	meta := callctx.From(ctx)
	credential, err := t.provider.ServiceCredential(ctx, ServiceScope{
		Audience: t.audience, Partition: meta.Partition, TenantID: meta.TenantID,
	})
	if ctx.Err() != nil {
		return nil, apperr.Unavailable(ctx.Err())
	}
	if err != nil {
		// Provider diagnostics may contain credentials. Never attach them to the
		// HTTP error chain (which applications commonly log verbatim).
		if apperr.Is(err, apperr.CodePermissionDenied) {
			return nil, apperr.PermissionDenied("service credential delegation was denied")
		}
		return nil, apperr.Unauthenticated("service credential acquisition failed")
	}
	if !validServiceToken(credential.Token) || !credential.ExpiresAt.After(time.Now()) {
		return nil, apperr.Unauthenticated("service credential is missing, malformed or expired")
	}
	cloned := req.Clone(ctx)
	if cloned.Header == nil {
		cloned.Header = make(http.Header)
	}
	// Header maps may contain noncanonical keys. Remove every spelling rather
	// than relying on Header.Del, which canonicalizes only its argument.
	for name := range cloned.Header {
		switch strings.ToLower(name) {
		case "authorization", "proxy-authorization", "cookie", "x-service-authorization", "x-step-up",
			"x-partition", "x-tenant-id", "x-caller", "x-merchant-id", "x-request-id":
			delete(cloned.Header, name)
		}
	}
	if meta.RequestID != "" {
		cloned.Header.Set(callctx.HeaderRequestID, meta.RequestID)
	}
	cloned.Header.Set(HeaderServiceAuthorization, "Bearer "+credential.Token)
	handedOff = true // The standard transport now owns closing the request body.
	return t.base.RoundTrip(cloned)
}

func (t *serviceTransport) CloseIdleConnections() { t.base.CloseIdleConnections() }

func validServiceToken(token string) bool {
	if token == "" || len(token) > 16*1024 {
		return false
	}
	for _, c := range token {
		if c <= ' ' || c >= 127 {
			return false
		}
	}
	return true
}
