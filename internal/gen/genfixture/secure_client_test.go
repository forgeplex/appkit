package genfixture

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/authn"
	"github.com/forgeplex/appkit/callctx"
	"github.com/forgeplex/appkit/contract"
	"github.com/forgeplex/appkit/httpserver"
	"github.com/forgeplex/appkit/tx"
)

type secureSecretKey struct{}

type secureObservation struct {
	meta                 callctx.Meta
	actor, principal, tx bool
	secret               any
}

type secureFixtureService struct {
	*stubService
	seen chan secureObservation
}

func (s *secureFixtureService) Greet(ctx context.Context, req GreetRequest) (GreetReply, error) {
	_, actor := appkit.ActorFrom(ctx)
	_, principal := appkit.ServicePrincipalFrom(ctx)
	s.seen <- secureObservation{meta: callctx.From(ctx), actor: actor, principal: principal, tx: tx.HasTx(ctx), secret: ctx.Value(secureSecretKey{})}
	switch req.Name {
	case "fail":
		return GreetReply{}, apperr.New("SECURE_FIXTURE_FAILED", http.StatusConflict, "fixture failure")
	case "wait":
		<-ctx.Done()
		return GreetReply{}, ctx.Err()
	}
	return GreetReply{Message: "hi " + req.Name}, nil
}

// The fixture uses real TLS termination in front of App.Run's real loopback
// listener, not a bare handler with a handwritten approximation of App guards.
// It does not claim that App.Run itself enables native TLS termination.
func TestSecureGeneratedClientStrictAppAndLocalBoundary(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	policy := func(_ context.Context, p appkit.ServicePrincipal, scope authn.ServiceDelegation) error {
		if p.Subject != "caller-service" || len(p.Audience) != 1 || p.Audience[0] != "greet" ||
			(scope != (authn.ServiceDelegation{}) && scope != (authn.ServiceDelegation{Partition: "partition-1", TenantID: "tenant-1"})) {
			return errors.New("fixture delegation denied")
		}
		return nil
	}
	verifier, err := authn.NewServiceVerifier(authn.ServiceVerifierOptions{Audience: "greet",
		Issuers:             map[string]authn.ServiceIssuer{"services": {Keys: map[string]ed25519.PublicKey{"current": pub}, Subjects: []string{"caller-service"}}},
		AuthorizeDelegation: policy})
	if err != nil {
		t.Fatal(err)
	}
	signer, err := authn.NewServiceSigner(authn.ServiceSignerOptions{Issuer: "services", Subject: "caller-service", KeyID: "current", Key: key, AuthorizeDelegation: policy})
	if err != nil {
		t.Fatal(err)
	}
	service := &secureFixtureService{stubService: &stubService{}, seen: make(chan secureObservation, 32)}
	verified := make(chan appkit.ServicePrincipal, 32)
	srv := startSecureGeneratedApp(t, service, verifier, verified)
	var providerCalls atomic.Int64
	provider := contract.ServiceCredentialProviderFunc(func(ctx context.Context, scope contract.ServiceScope) (contract.ServiceCredential, error) {
		providerCalls.Add(1)
		if ctx.Value(secureSecretKey{}) != nil {
			t.Error("credential provider received a private context value")
		}
		if _, ok := appkit.ActorFrom(ctx); ok {
			t.Error("credential provider received a user actor")
		}
		raw, exp, err := signer.SignWithExpiry(ctx, scope.Audience, authn.ServiceDelegation{Partition: scope.Partition, TenantID: scope.TenantID})
		return contract.ServiceCredential{Token: raw, ExpiresAt: exp}, err
	})
	remote, err := NewSecureClient(srv.URL, contract.SecureClientOptions{Audience: "greet", Credentials: provider, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.hc.CloseIdleConnections)
	meta := callctx.Meta{RequestID: "request-1", Partition: "partition-1", TenantID: "tenant-1", Caller: "caller-service"}
	ctx := callctx.With(context.WithValue(context.Background(), secureSecretKey{}, "user credential must not survive"), meta)
	ctx = appkit.WithActor(ctx, appkit.Actor{UserID: "user-1", TenantID: "tenant-1"})
	for _, binding := range []struct {
		name string
		svc  Service
	}{
		{"local", WrapService(service, 0)}, {"secure-remote", remote},
	} {
		t.Run(binding.name, func(t *testing.T) {
			for _, name := range []string{"Ada", "fail"} {
				reply, err := binding.svc.Greet(ctx, GreetRequest{Name: name})
				if name == "fail" {
					if !apperr.Is(err, "SECURE_FIXTURE_FAILED") {
						t.Fatalf("business error changed: %v", err)
					}
				} else if err != nil || reply.Message != "hi Ada" {
					t.Fatalf("reply=%+v error=%v", reply, err)
				}
				got := <-service.seen
				if got.meta != meta || got.actor || got.principal || got.tx || got.secret != nil {
					t.Fatalf("contract firewall or scope propagation failed: %+v", got)
				}
				if binding.name == "secure-remote" {
					p := <-verified
					if p.Subject != "caller-service" || p.TenantID != meta.TenantID || p.Issuer != "services" {
						t.Fatalf("route was not reached with verified service identity: %+v", p)
					}
				}
			}
			before := providerCalls.Load()
			if _, err := binding.svc.Greet(tx.With(ctx, "fake-tx"), GreetRequest{Name: "Ada"}); !apperr.Is(err, apperr.CodeTxBoundary) {
				t.Fatalf("transaction guard failed: %v", err)
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := binding.svc.Greet(canceled, GreetRequest{Name: "Ada"}); !apperr.Is(err, apperr.CodeUnavailable) {
				t.Fatalf("pre-canceled context passed: %v", err)
			}
			if providerCalls.Load() != before || len(service.seen) != 0 {
				t.Fatal("boundary rejection entered provider or implementation")
			}
			active, cancel := context.WithCancel(ctx)
			finished := make(chan error, 1)
			joined := false
			t.Cleanup(func() {
				cancel()
				if !joined {
					select {
					case <-finished:
					case <-time.After(6 * time.Second):
						t.Error("cancellation fixture did not finish during cleanup")
					}
				}
			})
			go func() {
				_, err := binding.svc.Greet(active, GreetRequest{Name: "wait"})
				finished <- err
			}()
			select {
			case <-service.seen:
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation fixture did not enter implementation")
			}
			cancel()
			select {
			case err := <-finished:
				joined = true
				if !apperr.Is(err, apperr.CodeUnavailable) {
					t.Fatalf("in-flight cancellation not normalized: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("in-flight call ignored cancellation")
			}
			if binding.name == "secure-remote" {
				<-verified
			}
		})
	}
	// A compatibility client has no service credential and must not gain access
	// merely by carrying unsigned tenant/partition/caller headers.
	legacy := NewClient(srv.URL, "caller-service", srv.Client())
	if _, err := legacy.Greet(ctx, GreetRequest{Name: "Ada"}); !apperr.Is(err, apperr.CodeUnauthenticated) {
		t.Fatalf("legacy unsigned client entered an internal route: %v", err)
	}
	before := providerCalls.Load()
	denied := callctx.With(ctx, callctx.Meta{Partition: "other-partition", TenantID: "tenant-1"})
	if _, err := remote.Greet(denied, GreetRequest{Name: "Ada"}); !apperr.Is(err, apperr.CodeUnauthenticated) {
		t.Fatalf("provider delegation denial not fail-closed: %v", err)
	}
	if providerCalls.Load() != before+1 || len(service.seen) != 0 || len(verified) != 0 {
		t.Fatal("authentication denial was retried or reached the service")
	}
}

func startSecureGeneratedApp(t *testing.T, svc Service, verifier *authn.ServiceVerifier, verified chan<- appkit.ServicePrincipal) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	module := appkit.ModuleFunc("secure-generated-fixture", func(reg *appkit.Registry) error {
		handler := NewHTTPHandler(WrapService(svc, 0))
		reg.MountInternalService("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := appkit.ServicePrincipalFrom(r.Context())
			if !ok {
				t.Error("internal route lacked verified service principal")
			}
			verified <- p
			handler.ServeHTTP(w, r)
		}))
		return nil
	})
	listening := make(chan string, 1)
	app := appkit.New([]appkit.Module{module}, appkit.Security(appkit.SecurityInternalService), appkit.HTTPAddr("127.0.0.1:0"),
		appkit.Logger(logger), appkit.ShutdownTimeout(2*time.Second), appkit.Middleware(httpserver.Base(logger)...), appkit.Middleware(verifier.Middleware),
		appkit.HTTPServer(func(server *http.Server) {
			server.BaseContext = func(listener net.Listener) context.Context {
				listening <- "http://" + listener.Addr().String()
				ctx := callctx.With(context.Background(), callctx.Meta{TenantID: "forged", Partition: "forged", Caller: "forged"})
				ctx = appkit.WithActor(ctx, appkit.Actor{UserID: "forged", TenantID: "forged"})
				return appkit.WithServicePrincipal(ctx, appkit.ServicePrincipal{Subject: "forged"})
			}
		}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	joined := false
	t.Cleanup(func() {
		cancel()
		if joined {
			return
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("App shutdown: %v", err)
			}
		case <-time.After(4 * time.Second):
			t.Error("App shutdown exceeded fixture budget")
		}
	})
	select {
	case address := <-listening:
		target, err := url.Parse(address)
		if err != nil {
			t.Fatal(err)
		}
		proxy := httputil.NewSingleHostReverseProxy(target)
		transport := &http.Transport{Proxy: nil}
		proxy.Transport = transport
		t.Cleanup(transport.CloseIdleConnections)
		srv := httptest.NewTLSServer(proxy)
		t.Cleanup(srv.Close)
		return srv
	case err := <-done:
		joined = true
		t.Fatalf("App failed before serving: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("App did not begin serving")
	}
	return nil
}

func TestSecureGeneratedClientAcquiresCredentialsPerRetry(t *testing.T) {
	var hits, issued atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(contract.HeaderServiceAuthorization) == "" {
			t.Error("retry lacked service credential")
		}
		if hits.Add(1) < 3 {
			apperr.WriteProblem(w, apperr.Unavailable(errors.New("retry fixture")))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
	}))
	defer srv.Close()
	provider := contract.ServiceCredentialProviderFunc(func(context.Context, contract.ServiceScope) (contract.ServiceCredential, error) {
		issued.Add(1)
		return contract.ServiceCredential{Token: "fixture.service.credential", ExpiresAt: time.Now().Add(time.Minute)}, nil
	})
	client, err := NewSecureClient(srv.URL, contract.SecureClientOptions{Audience: "greet", Credentials: provider, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer client.hc.CloseIdleConnections()
	if err := client.Ping(context.Background()); err != nil || hits.Load() != 3 || issued.Load() != 3 {
		t.Fatalf("retry cached or skipped credential acquisition: hits=%d issued=%d error=%v", hits.Load(), issued.Load(), err)
	}
	if _, err := NewSecureClient(srv.URL, contract.SecureClientOptions{Audience: "greet", HTTPClient: srv.Client()}); err == nil {
		t.Fatal("generated secure constructor accepted a missing provider")
	}
}
