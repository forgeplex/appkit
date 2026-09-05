package contract_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/callctx"
	"github.com/forgeplex/appkit/contract"
)

func freshProvider() contract.ServiceCredentialProviderFunc {
	return func(context.Context, contract.ServiceScope) (contract.ServiceCredential, error) {
		return contract.ServiceCredential{Token: "service.jwt.token", ExpiresAt: time.Now().Add(time.Minute)}, nil
	}
}

type insecureRoundTripper func(*http.Request) (*http.Response, error)

func (f insecureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestSecureHTTPRejectsUnsafeConfiguration(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	var nilFunc contract.ServiceCredentialProviderFunc
	for _, tc := range []struct {
		name string
		base string
		edit func(*contract.SecureClientOptions)
	}{
		{name: "missing provider", edit: func(o *contract.SecureClientOptions) { o.Credentials = nil }},
		{name: "typed nil provider", edit: func(o *contract.SecureClientOptions) { o.Credentials = nilFunc }},
		{name: "missing audience", edit: func(o *contract.SecureClientOptions) { o.Audience = "" }},
		{name: "http", base: "http://example.test"},
		{name: "userinfo", base: "https://user:secret@example.test"},
		{name: "query", base: "https://example.test?token=secret"},
		{name: "fragment", base: "https://example.test/#secret"},
		{name: "port", base: "https://example.test:70000"},
		{name: "cookie jar", edit: func(o *contract.SecureClientOptions) { o.HTTPClient.Jar = jar }},
		{name: "custom roundtripper", edit: func(o *contract.SecureClientOptions) {
			o.HTTPClient.Transport = insecureRoundTripper(func(*http.Request) (*http.Response, error) { panic("must not call") })
		}},
		{name: "skip TLS verification", edit: func(o *contract.SecureClientOptions) {
			o.HTTPClient.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify = true
		}},
		{name: "wrong TLS server name", edit: func(o *contract.SecureClientOptions) {
			o.HTTPClient.Transport.(*http.Transport).TLSClientConfig.ServerName = "other.test"
		}},
		{name: "weak TLS", edit: func(o *contract.SecureClientOptions) {
			o.HTTPClient.Transport.(*http.Transport).TLSClientConfig.MinVersion = tls.VersionTLS10
		}},
		{name: "TLS key logging", edit: func(o *contract.SecureClientOptions) {
			o.HTTPClient.Transport.(*http.Transport).TLSClientConfig.KeyLogWriter = io.Discard
		}},
		{name: "custom TLS dial", edit: func(o *contract.SecureClientOptions) {
			o.HTTPClient.Transport.(*http.Transport).DialTLSContext = func(context.Context, string, string) (net.Conn, error) { panic("must not call") }
		}},
		{name: "legacy TLS dial", edit: func(o *contract.SecureClientOptions) {
			o.HTTPClient.Transport.(*http.Transport).DialTLS = func(string, string) (net.Conn, error) { panic("must not call") }
		}},
		{name: "custom protocol", edit: func(o *contract.SecureClientOptions) {
			o.HTTPClient.Transport.(*http.Transport).TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{
				"evil": func(string, *tls.Conn) http.RoundTripper { panic("must not call") },
			}
		}},
		{name: "unencrypted HTTP2", edit: func(o *contract.SecureClientOptions) {
			p := &http.Protocols{}
			p.SetUnencryptedHTTP2(true)
			o.HTTPClient.Transport.(*http.Transport).Protocols = p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := tc.base
			if base == "" {
				base = "https://example.test"
			}
			o := contract.SecureClientOptions{Audience: "service", Credentials: freshProvider(),
				HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{}}}}
			if tc.edit != nil {
				tc.edit(&o)
			}
			if _, err := contract.NewSecureHTTPClient(base, o); !apperr.Is(err, apperr.CodeInvalidArgument) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe configuration: %v", err)
			}
		})
	}
}

func TestSecureHTTPFreshCredentialsFirewallAndRequestIsolation(t *testing.T) {
	type secretKey struct{}
	meta := callctx.Meta{RequestID: "request-1", Partition: "partition-1", TenantID: "tenant-1", Caller: "previous-hop"}
	var calls atomic.Int64
	var hits atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if got := r.Header.Get(contract.HeaderServiceAuthorization); got != "Bearer token-"+strconv.FormatInt(n, 10) {
			t.Errorf("service token = %q", got)
		}
		for _, name := range []string{"Authorization", "X-Step-Up", "Cookie", "Proxy-Authorization", "X-Tenant-Id", "X-Partition", "X-Caller", "X-Merchant-Id"} {
			if value := r.Header.Get(name); value != "" {
				t.Errorf("unexpected unsigned identity %s=%q", name, value)
			}
		}
		if r.Header.Get(callctx.HeaderRequestID) != meta.RequestID {
			t.Error("request id was not propagated from context")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	provider := contract.ServiceCredentialProviderFunc(func(ctx context.Context, scope contract.ServiceScope) (contract.ServiceCredential, error) {
		if ctx.Value(secretKey{}) != nil || scope != (contract.ServiceScope{Audience: "target", Partition: meta.Partition, TenantID: meta.TenantID}) {
			t.Errorf("provider bypassed firewall or trusted headers: scope=%+v secret=%v", scope, ctx.Value(secretKey{}))
		}
		return contract.ServiceCredential{Token: "token-" + strconv.FormatInt(calls.Add(1), 10), ExpiresAt: time.Now().Add(time.Minute)}, nil
	})
	hc, err := contract.NewSecureHTTPClient(srv.URL, contract.SecureClientOptions{Audience: "target", Credentials: provider, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer hc.CloseIdleConnections()
	ctx := callctx.With(context.WithValue(context.Background(), secretKey{}, "user-token"), meta)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Noncanonical keys must be scrubbed as well as ordinary Header.Set values.
	req.Header = http.Header{"authorization": {"Bearer user-secret"}, "Authorization": {"Bearer other-secret"},
		"Cookie": {"session=secret"}, "X-Step-Up": {"user-step-up-proof"}, "Proxy-Authorization": {"secret"}, "X-Service-Authorization": {"stale-token"},
		"X-Partition": {"forged-partition"}, "X-Tenant-Id": {"forged-tenant"}, "X-Merchant-Id": {"forged-merchant"},
		"X-Caller": {"forged-caller"}, "X-Request-Id": {"forged-request"}}
	before := req.Header.Clone()
	for range 2 {
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if calls.Load() != 2 || hits.Load() != 2 || !reflect.DeepEqual(before, req.Header) || req.Context().Value(secretKey{}) != "user-token" {
		t.Fatalf("credential caching or caller mutation: calls=%d hits=%d headers=%v", calls.Load(), hits.Load(), req.Header)
	}
}

func TestSecureHTTPRejectsRedirectAndOriginBeforeCredentials(t *testing.T) {
	var leaked, calls atomic.Int64
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Add(1) }))
	defer target.Close()
	var source *httptest.Server
	source = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/same" {
			http.Redirect(w, r, source.URL+"/destination", http.StatusTemporaryRedirect)
			return
		}
		if r.URL.Path == "/destination" {
			leaked.Add(1)
			return
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	input := source.Client()
	input.CheckRedirect = func(*http.Request, []*http.Request) error {
		t.Error("unsafe caller redirect callback used")
		return nil
	}
	provider := contract.ServiceCredentialProviderFunc(func(context.Context, contract.ServiceScope) (contract.ServiceCredential, error) {
		calls.Add(1)
		return freshProvider()(context.Background(), contract.ServiceScope{})
	})
	hc, err := contract.NewSecureHTTPClient(source.URL, contract.SecureClientOptions{Audience: "target", Credentials: provider, HTTPClient: input})
	if err != nil {
		t.Fatal(err)
	}
	defer hc.CloseIdleConnections()
	for _, url := range []string{source.URL, source.URL + "/same"} {
		if _, err := hc.Get(url); !apperr.Is(err, apperr.CodePermissionDenied) {
			t.Fatalf("redirect not rejected: %v", err)
		}
	}
	before := calls.Load()
	for _, tc := range []struct{ url, host string }{
		{target.URL, ""}, {strings.Replace(source.URL, "https:", "http:", 1), ""}, {source.URL, "evil.test"},
	} {
		req, err := http.NewRequest(http.MethodPost, tc.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = tc.host
		if _, err := hc.Do(req); !apperr.Is(err, apperr.CodePermissionDenied) {
			t.Fatalf("wrong origin/Host not rejected: %v", err)
		}
	}
	if leaked.Load() != 0 || calls.Load() != before {
		t.Fatalf("credential issued/leaked for a rejected destination: calls=%d leaked=%d", calls.Load(), leaked.Load())
	}
}

func TestSecureHTTPCredentialFailureAndCancellation(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	for _, tc := range []struct {
		name  string
		token string
		exp   time.Time
		err   error
	}{
		{name: "provider failure", err: errors.New("secret credential data")},
		{name: "empty token", exp: time.Now().Add(time.Minute)},
		{name: "unknown expiry", token: "token"},
		{name: "expired", token: "token", exp: time.Now().Add(-time.Minute)},
		{name: "header injection", token: "token\r\nInjected: yes", exp: time.Now().Add(time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hc, err := contract.NewSecureHTTPClient(srv.URL, contract.SecureClientOptions{Audience: "target", HTTPClient: srv.Client(),
				Credentials: contract.ServiceCredentialProviderFunc(func(context.Context, contract.ServiceScope) (contract.ServiceCredential, error) {
					return contract.ServiceCredential{Token: tc.token, ExpiresAt: tc.exp}, tc.err
				})})
			if err != nil {
				t.Fatal(err)
			}
			defer hc.CloseIdleConnections()
			if _, err := hc.Get(srv.URL); !apperr.Is(err, apperr.CodeUnauthenticated) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("credential failure leaked or passed: %v", err)
			}
		})
	}
	var calls atomic.Int64
	hc, err := contract.NewSecureHTTPClient(srv.URL, contract.SecureClientOptions{Audience: "target", HTTPClient: srv.Client(),
		Credentials: contract.ServiceCredentialProviderFunc(func(ctx context.Context, _ contract.ServiceScope) (contract.ServiceCredential, error) {
			calls.Add(1)
			<-ctx.Done()
			return contract.ServiceCredential{}, ctx.Err()
		})})
	if err != nil {
		t.Fatal(err)
	}
	defer hc.CloseIdleConnections()
	for _, alreadyCanceled := range []bool{true, false} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		if alreadyCanceled {
			cancel()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = hc.Do(req)
		cancel()
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cancellation identity lost: %v", err)
		}
	}
	if hits.Load() != 0 || calls.Load() != 1 {
		t.Fatalf("invalid credentials reached server/provider: hits=%d calls=%d", hits.Load(), calls.Load())
	}
}

func TestSecureHTTPVerifiesTLSAndCopiesTransport(t *testing.T) {
	var hits, bypass atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); w.WriteHeader(http.StatusNoContent) }))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	defer srv.Close()
	input := srv.Client()
	transport := input.Transport.(*http.Transport)
	transport.RegisterProtocol("https", insecureRoundTripper(func(*http.Request) (*http.Response, error) {
		bypass.Add(1)
		return nil, errors.New("custom protocol must not be copied")
	}))
	hc, err := contract.NewSecureHTTPClient(srv.URL, contract.SecureClientOptions{Audience: "target", Credentials: freshProvider(), HTTPClient: input})
	if err != nil {
		t.Fatal(err)
	}
	defer hc.CloseIdleConnections()
	if input.Transport != transport || transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.ServerName != "" {
		t.Fatal("constructor changed caller transport configuration")
	}
	transport.TLSClientConfig.InsecureSkipVerify = true // Changing the input must not weaken the copy.
	if resp, err := hc.Get(srv.URL); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	if hits.Load() != 1 || bypass.Load() != 0 {
		t.Fatalf("custom registered protocol bypassed TLS: hits=%d bypass=%d", hits.Load(), bypass.Load())
	}
	for _, tc := range []struct {
		name string
		base string
		hc   *http.Client
	}{
		{name: "untrusted certificate", base: srv.URL},
		{name: "hostname mismatch", base: strings.Replace(srv.URL, "127.0.0.1", "localhost", 1), hc: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: transport.TLSClientConfig.RootCAs}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := contract.NewSecureHTTPClient(tc.base, contract.SecureClientOptions{Audience: "target", Credentials: freshProvider(), HTTPClient: tc.hc})
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			if _, err := client.Get(tc.base); err == nil {
				t.Fatal("TLS verification was bypassed")
			}
		})
	}
	if hits.Load() != 1 {
		t.Fatalf("credential sent before TLS verification: hits=%d", hits.Load())
	}
}

type observedBody struct {
	io.Reader
	closed bool
}

func (b *observedBody) Close() error { b.closed = true; return nil }

func TestSecureHTTPRejectsCredentialTrailersBeforeSend(t *testing.T) {
	var hits, issued atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body) // Force real HTTP trailer decoding.
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	hc, err := contract.NewSecureHTTPClient(srv.URL, contract.SecureClientOptions{Audience: "target", HTTPClient: srv.Client(),
		Credentials: contract.ServiceCredentialProviderFunc(func(context.Context, contract.ServiceScope) (contract.ServiceCredential, error) {
			issued.Add(1)
			return freshProvider()(context.Background(), contract.ServiceScope{})
		})})
	if err != nil {
		t.Fatal(err)
	}
	defer hc.CloseIdleConnections()
	for _, tc := range []struct {
		name    string
		trailer http.Header
		header  http.Header
	}{
		{name: "user and service trailers", trailer: http.Header{"Authorization": {"Bearer user-secret"}, "X-Service-Authorization": {"Bearer upstream-secret"}, "X-Step-Up": {"mfa-secret"}}},
		{name: "noncanonical trailers", trailer: http.Header{"authorization": {"Bearer user-secret"}, "cookie": {"session=secret"}}},
		{name: "declared trailers", header: http.Header{"Trailer": {"Authorization, X-Service-Authorization"}}},
		{name: "noncanonical declaration", header: http.Header{"trailer": {"Cookie"}}},
		{name: "integrity trailer also fails explicitly", trailer: http.Header{"Digest": {"not silently discarded"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &observedBody{Reader: strings.NewReader("body")}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, body)
			if err != nil {
				t.Fatal(err)
			}
			req.ContentLength = -1
			req.TransferEncoding = []string{"chunked"}
			req.Header, req.Trailer = tc.header.Clone(), tc.trailer.Clone()
			beforeHeader, beforeTrailer := req.Header.Clone(), req.Trailer.Clone()
			if _, err := hc.Do(req); !apperr.Is(err, apperr.CodeInvalidArgument) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("trailers accepted or error leaked a credential: %v", err)
			}
			if !body.closed || !reflect.DeepEqual(beforeHeader, req.Header) || !reflect.DeepEqual(beforeTrailer, req.Trailer) {
				t.Fatal("rejection failed to close body or changed the caller's request")
			}
		})
	}
	if hits.Load() != 0 || issued.Load() != 0 {
		t.Fatalf("trailer-bearing request reached server/provider: hits=%d issued=%d", hits.Load(), issued.Load())
	}
}
