package bootstrap

import (
	"context"
	"crypto/ed25519"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/authn"
	"github.com/forgeplex/appkit/callctx"
)

func serviceSecurityFixture(t *testing.T) (SecurityOptions, ed25519.PrivateKey) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	v, err := authn.NewServiceVerifier(authn.ServiceVerifierOptions{
		Audience: "catalog",
		Issuers: map[string]authn.ServiceIssuer{
			"service-ca": {Keys: map[string]ed25519.PublicKey{"key-1": pub}, Subjects: []string{"gateway"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return SecurityOptions{ServiceVerifier: v}, key
}

func TestServiceSecurityModeMatrix(t *testing.T) {
	service, key := serviceSecurityFixture(t)
	pub := key.Public().(ed25519.PublicKey)
	user := Options{Service: "matrix", AuthnPublicKey: pub, AuthnIssuer: "users"}
	plain := Options{Service: "matrix"}
	multi := SecurityOptions{UserIssuers: map[string]authn.Issuer{"users": {Key: pub, Partition: "eu"}}}
	both := multi
	both.ServiceVerifier = service.ServiceVerifier
	for _, tc := range []struct {
		name string
		mode appkit.SecurityMode
		o    Options
		s    SecurityOptions
		want string
	}{
		{"internal configured", appkit.SecurityInternalService, plain, service, ""},
		{"mixed configured", appkit.SecurityMixed, user, service, ""},
		{"mixed multi issuer", appkit.SecurityMixed, plain, both, ""},
		{"user multi issuer", appkit.SecurityUserFacing, plain, multi, ""},
		{"internal missing", appkit.SecurityInternalService, plain, SecurityOptions{}, "服务身份配置"},
		{"internal zero verifier", appkit.SecurityInternalService, plain, SecurityOptions{ServiceVerifier: &authn.ServiceVerifier{}}, "服务身份配置"},
		{"mixed missing user", appkit.SecurityMixed, plain, service, "AuthnPublicKey"},
		{"mixed missing service", appkit.SecurityMixed, user, SecurityOptions{}, "服务身份配置"},
		{"internal ignores no user", appkit.SecurityInternalService, user, service, "冲突"},
		{"internal ignores no multi", appkit.SecurityInternalService, plain, both, "冲突"},
		{"user ignores no service", appkit.SecurityUserFacing, user, service, "冲突"},
		{"disabled ignores no service", appkit.SecurityDisabled, plain, service, "冲突"},
		{"disabled ignores no multi", appkit.SecurityDisabled, plain, multi, "冲突"},
		{"ambiguous users", appkit.SecurityUserFacing, user, multi, "不能同时"},
		{"invalid users", appkit.SecurityUserFacing, plain, SecurityOptions{UserIssuers: map[string]authn.Issuer{"users": {}}}, "有效 Ed25519"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHTTPSecurityWithOptions(tc.o, "dev", tc.mode, "MATRIX", false, tc.s)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestServiceSecurityFailsBeforeInfrastructure(t *testing.T) {
	for _, env := range []string{"staging", "prod"} {
		for _, mode := range []appkit.SecurityMode{appkit.SecurityInternalService, appkit.SecurityMixed} {
			t.Run(env+"/"+string(mode), func(t *testing.T) {
				t.Setenv("SERVICEFAIL_ENV", env)
				t.Setenv("SERVICEFAIL_SECURITY__MODE", string(mode))
				called := false
				err := RunWithSecurity(t.Context(), Options{
					Service: "servicefail",
					Modules: func(Deps) ([]appkit.Module, error) { called = true; return nil, nil },
				}, RunOptions{ConfigFile: filepath.Join(t.TempDir(), "absent.yaml")}, SecurityOptions{ServiceVerifier: &authn.ServiceVerifier{}})
				if err == nil || !strings.Contains(err.Error(), "服务身份配置") || called {
					t.Fatalf("invalid credentials did not fail before infrastructure: err=%v modules=%v", err, called)
				}
			})
		}
	}
}

func TestMigrationDoesNotConstructHTTPVerifiers(t *testing.T) {
	for _, mode := range []appkit.SecurityMode{appkit.SecurityUserFacing, appkit.SecurityInternalService, appkit.SecurityMixed} {
		t.Run(string(mode), func(t *testing.T) {
			t.Setenv("MIGRATIONSEC_SECURITY__MODE", string(mode))
			err := RunWithSecurity(t.Context(), Options{
				Service: "migrationsec", Modules: func(Deps) ([]appkit.Module, error) { return nil, nil },
			}, RunOptions{MigrateOnly: true, ConfigFile: filepath.Join(t.TempDir(), "absent.yaml")}, SecurityOptions{})
			if err == nil || !strings.Contains(err.Error(), "-migrate 需要 database.url") {
				t.Fatalf("HTTP-only credentials must not affect migrations: %v", err)
			}
		})
	}
}

func TestMigrationIgnoresMalformedHTTPOnlyConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.yaml")
	if err := os.WriteFile(path, []byte("env: prod\nsecurity:\n  mode: internal_service\n  service:\n    max_ttl: not-a-duration\n    clock_skew: malformed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err := Run(t.Context(), Options{Service: "migrationmalformed", Modules: func(Deps) ([]appkit.Module, error) { return nil, nil }}, RunOptions{MigrateOnly: true, ConfigFile: path})
	if err == nil || !strings.Contains(err.Error(), "-migrate 需要 database.url") {
		t.Fatalf("HTTP-only configuration must not affect migration path: %v", err)
	}
}

func TestRunWithSecurityServesVerifiedServiceRequests(t *testing.T) {
	for _, mode := range []appkit.SecurityMode{appkit.SecurityInternalService, appkit.SecurityMixed} {
		t.Run(string(mode), func(t *testing.T) {
			security, key := serviceSecurityFixture(t)
			t.Setenv("SERVICERUN_ADDR", "127.0.0.1:0")
			t.Setenv("SERVICERUN_SECURITY__MODE", string(mode))
			t.Setenv("SERVICERUN_LOG__LEVEL", "error")
			listening := make(chan string, 1)
			module := appkit.ModuleFunc("catalog", func(reg *appkit.Registry) error {
				reg.MountInternalService("GET /internal", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					principal, ok := appkit.ServicePrincipalFrom(r.Context())
					if !ok || principal.Subject != "gateway" || callctx.From(r.Context()).Caller != "gateway" {
						http.Error(w, "identity missing", http.StatusInternalServerError)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				}))
				reg.MountPublic("GET /public", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
				if mode == appkit.SecurityMixed {
					reg.MountAuthenticated("GET /user", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
				}
				return nil
			})
			o := Options{
				Service: "servicerun",
				Modules: func(Deps) ([]appkit.Module, error) { return []appkit.Module{module}, nil },
				Minimal: func(Deps) ([]appkit.Module, error) { return []appkit.Module{module}, nil },
				AppOptions: func(Deps) []appkit.Option {
					return []appkit.Option{appkit.HTTPServer(func(s *http.Server) {
						s.BaseContext = func(ln net.Listener) context.Context {
							listening <- "http://" + ln.Addr().String()
							return appkit.WithServicePrincipal(context.Background(), appkit.ServicePrincipal{Subject: "forged"})
						}
					})}
				},
			}
			if mode == appkit.SecurityMixed {
				o.AuthnPublicKey, o.AuthnIssuer = key.Public().(ed25519.PublicKey), "users"
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			finished := false
			t.Cleanup(func() {
				cancel()
				if finished {
					return
				}
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("shutdown: %v", err)
					}
				case <-time.After(10 * time.Second):
					t.Error("bootstrap did not stop")
				}
			})
			configFile := filepath.Join(t.TempDir(), "absent.yaml")
			go func() {
				done <- RunWithSecurity(ctx, o, RunOptions{Minimal: true, ConfigFile: configFile}, security)
			}()
			var base string
			select {
			case base = <-listening:
			case err := <-done:
				finished = true
				t.Fatalf("bootstrap failed before listening: %v", err)
			case <-time.After(15 * time.Second):
				t.Fatal("bootstrap readiness timeout")
			}
			signService := func(audience string) string {
				t.Helper()
				now := time.Now()
				tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
					"iss": "service-ca", "sub": "gateway", "aud": audience,
					"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "purpose": "service",
				})
				tok.Header["kid"], tok.Header["typ"] = "key-1", "appkit-service+jwt"
				raw, err := tok.SignedString(key)
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			valid := signService("catalog")
			client := &http.Client{Timeout: 5 * time.Second}
			defer client.CloseIdleConnections()
			for _, tc := range []struct {
				name, path, token, tenant string
				want                      int
			}{
				{"missing", "/internal", "", "", http.StatusUnauthorized},
				{"valid", "/internal", valid, "", http.StatusNoContent},
				{"bad audience", "/internal", signService("other"), "", http.StatusUnauthorized},
				{"malformed", "/internal", "invalid", "", http.StatusUnauthorized},
				{"forged tenant", "/internal", valid, "forged", http.StatusUnauthorized},
				{"public", "/public", "", "", http.StatusNoContent},
				{"health probe", "/healthz", "invalid", "", http.StatusOK},
			} {
				t.Run(tc.name, func(t *testing.T) {
					req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+tc.path, nil)
					if err != nil {
						t.Fatal(err)
					}
					if tc.token != "" {
						req.Header.Set(authn.HeaderServiceAuthorization, "Bearer "+tc.token)
					}
					if tc.tenant != "" {
						req.Header.Set(callctx.HeaderTenantID, tc.tenant)
					}
					resp, err := client.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					body, _ := io.ReadAll(resp.Body)
					if resp.StatusCode != tc.want {
						t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, tc.want, body)
					}
				})
			}
		})
	}
}
