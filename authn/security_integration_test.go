package authn

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/callctx"
	"github.com/forgeplex/appkit/httpserver"
)

type strictIdentityObservation struct {
	Meta       callctx.Meta
	Actor      appkit.Actor
	HasActor   bool
	HasService bool
	Headers    callctx.Meta
	Merchant   string
}

// Exercise App.Run's actual listener and middleware ordering, not a hand-built
// approximation of identityBoundary. BaseContext simulates untrusted identity
// installed outside the App; forged HTTP headers independently test wire input.
func TestStrictAppMultiIssuerIdentityIntegration(t *testing.T) {
	pubA, privA, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pubB, privB, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	token := func(key ed25519.PrivateKey, issuer, subject, tenant string, perms []string) string {
		return sign(t, key, jwt.MapClaims{
			"iss": issuer, "sub": subject, "tid": tenant, "perms": perms,
			"partition": "forged-signed-claim", // Partition is deployment mapping, not this claim.
			"exp":       time.Now().Add(time.Hour).Unix(),
		})
	}
	readPerms := []string{"fixture:read"}
	alpha := token(privA, "issuer-a", "user-a", "shared-tenant", readPerms)
	beta := token(privB, "issuer-b", "user-b", "shared-tenant", nil)
	for _, mode := range []appkit.SecurityMode{appkit.SecurityUserFacing, appkit.SecurityMixed} {
		t.Run(string(mode), func(t *testing.T) {
			url := startStrictIdentityApp(t, mode, MultiIssuer(map[string]Issuer{
				"issuer-a": {Key: pubA, Partition: "partition-a"},
				"issuer-b": {Key: pubB, Partition: "partition-b"},
				"flat":     {Key: pubA},
			}))
			transport := &http.Transport{Proxy: nil}
			t.Cleanup(transport.CloseIdleConnections)
			client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
			cases := []struct {
				name, path, bearer, subject, tenant, partition string
				perms                                          []string
				status                                         int
			}{
				{"issuer-a", "/private", alpha, "user-a", "shared-tenant", "partition-a", readPerms, 200},
				{"issuer-b", "/private", beta, "user-b", "shared-tenant", "partition-b", nil, 200},
				{"no-partition", "/private", token(privA, "flat", "user-flat", "", nil), "user-flat", "", "", nil, 200},
				{"no-tenant", "/private", token(privA, "issuer-a", "user-a", "", nil), "user-a", "", "partition-a", nil, 200},
				{"anonymous-public", "/public", "", "", "", "", nil, 200},
				{"authenticated-public", "/public", alpha, "user-a", "shared-tenant", "partition-a", readPerms, 200},
				{"anonymous-private", "/private", "", "", "", "", nil, 401},
				{"anonymous-permission", "/permission", "", "", "", "", nil, 401},
				{"permitted", "/permission", alpha, "user-a", "shared-tenant", "partition-a", readPerms, 200},
				{"denied", "/permission", beta, "", "", "", nil, 403},
				{"wrong-key-for-issuer", "/private", token(privA, "issuer-b", "forged", "forged", readPerms), "", "", "", nil, 401},
				{"unknown-issuer", "/private", token(privA, "unknown", "forged", "forged", readPerms), "", "", "", nil, 401},
				{"invalid-public-credential", "/public", "malformed", "", "", "", nil, 401},
			}
			if mode == appkit.SecurityMixed {
				cases = append(cases, struct {
					name, path, bearer, subject, tenant, partition string
					perms                                          []string
					status                                         int
				}{name: "user-cannot-be-service", path: "/internal", bearer: alpha, status: 401})
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url+tc.path, nil)
					if err != nil {
						t.Fatal(err)
					}
					callctx.Inject(callctx.Meta{RequestID: "trace-wire", Partition: "forged-wire-partition", TenantID: "forged-wire-tenant", Caller: "forged-wire-caller"}, req.Header.Set)
					req.Header.Set("X-Merchant-Id", "forged-wire-merchant")
					if tc.bearer != "" {
						req.Header.Set("Authorization", "Bearer "+tc.bearer)
					}
					resp, err := client.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					data, err := io.ReadAll(resp.Body)
					if err != nil || resp.StatusCode != tc.status {
						t.Fatalf("status=%d want=%d read=%v body=%s", resp.StatusCode, tc.status, err, data)
					}
					if tc.status != http.StatusOK {
						wantCode := apperr.CodeUnauthenticated
						if tc.status == http.StatusForbidden {
							wantCode = apperr.CodePermissionDenied
						}
						if got := apperr.FromProblem(resp.StatusCode, data).Code(); got != wantCode {
							t.Fatalf("problem code=%s want=%s", got, wantCode)
						}
						return
					}
					var got strictIdentityObservation
					if err := json.Unmarshal(data, &got); err != nil {
						t.Fatal(err)
					}
					if got.Meta != (callctx.Meta{RequestID: "trace-wire", Partition: tc.partition, TenantID: tc.tenant}) ||
						got.HasActor != (tc.subject != "") || got.Actor.UserID != tc.subject || got.Actor.TenantID != tc.tenant ||
						!slices.Equal(got.Actor.Perms, tc.perms) || got.HasService {
						t.Fatalf("identity not rebuilt solely from verified input: %+v", got)
					}
					if got.Headers != (callctx.Meta{RequestID: "trace-wire"}) || got.Merchant != "" {
						t.Fatalf("unsigned identity headers survived: %+v", got)
					}
				})
			}
		})
	}
}

func startStrictIdentityApp(t *testing.T, mode appkit.SecurityMode, verify func(http.Handler) http.Handler) string {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	observe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strictIdentityObservation{Meta: callctx.From(r.Context()), Headers: callctx.Extract(r.Header.Get), Merchant: r.Header.Get("X-Merchant-Id")}
		got.Actor, got.HasActor = appkit.ActorFrom(r.Context())
		_, got.HasService = appkit.ServicePrincipalFrom(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(got)
	})
	module := appkit.ModuleFunc("identity-fixture", func(reg *appkit.Registry) error {
		reg.Permissions(appkit.PermissionDecl{Code: "fixture:read"})
		reg.MountPublic("GET /public", observe)
		reg.MountAuthenticated("GET /private", observe)
		reg.MountPermission("GET /permission", "fixture:read", observe)
		if mode == appkit.SecurityMixed {
			reg.MountInternalService("GET /internal", observe)
		}
		return nil
	})
	cleanBeforeVerify := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			meta := callctx.From(r.Context())
			_, actor := appkit.ActorFrom(r.Context())
			_, service := appkit.ServicePrincipalFrom(r.Context())
			if actor || service || meta.Partition != "" || meta.TenantID != "" || meta.Caller != "" ||
				r.Header.Get(callctx.HeaderPartition) != "" || r.Header.Get(callctx.HeaderTenantID) != "" ||
				r.Header.Get(callctx.HeaderCaller) != "" || r.Header.Get("X-Merchant-Id") != "" {
				http.Error(w, "identity survived outside verifier", http.StatusInternalServerError)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	listening := make(chan string, 1)
	app := appkit.New([]appkit.Module{module}, appkit.Security(mode), appkit.HTTPAddr("127.0.0.1:0"),
		appkit.Logger(log), appkit.ShutdownTimeout(2*time.Second),
		appkit.Middleware(httpserver.Base(log)...), appkit.Middleware(cleanBeforeVerify, verify),
		appkit.HTTPServer(func(server *http.Server) {
			server.BaseContext = func(ln net.Listener) context.Context {
				listening <- "http://" + ln.Addr().String()
				ctx := callctx.With(context.Background(), callctx.Meta{RequestID: "trace-context", Partition: "forged-context-partition", TenantID: "forged-context-tenant", Caller: "forged-context-caller"})
				ctx = appkit.WithActor(ctx, appkit.Actor{UserID: "forged-context-user", TenantID: "forged-context-tenant", Perms: []string{"fixture:read"}})
				return appkit.WithServicePrincipal(ctx, appkit.ServicePrincipal{Subject: "forged-context-service"})
			}
		}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	finished := false
	t.Cleanup(func() {
		cancel()
		if finished {
			return
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("App shutdown: %v", err)
			}
		case <-time.After(4 * time.Second):
			t.Error("App did not stop within its shutdown budget")
		}
	})
	select {
	case url := <-listening:
		return url
	case err := <-done:
		finished = true
		t.Fatalf("App failed before listening: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("App did not start within budget")
	}
	return ""
}
