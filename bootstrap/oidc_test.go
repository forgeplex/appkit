package bootstrap

import (
	"context"
	"crypto"
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

func testOIDCVerifier(t *testing.T) (*authn.OIDCVerifier, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	v, err := authn.NewOIDCVerifier(authn.OIDCVerifierOptions{
		Realms: []authn.OIDCRealm{{
			Issuer:     "https://demo.sso.example.com/",
			Audience:   "https://api.example.com",
			Partition:  "demo",
			StaticKeys: map[string]crypto.PublicKey{"k1": pub},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return v, priv
}

func signTestOIDCToken(t *testing.T, priv ed25519.PrivateKey, sub, orgID string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
		"iss":    "https://demo.sso.example.com/",
		"sub":    sub,
		"aud":    "https://api.example.com",
		"org_id": orgID,
		"exp":    time.Now().Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = "k1"
	tok.Header["typ"] = "at+jwt"
	raw, err := tok.SignedString(priv)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestOIDCSecurityModeMatrix(t *testing.T) {
	service, key := serviceSecurityFixture(t)
	pub := key.Public().(ed25519.PublicKey)
	verifier, _ := testOIDCVerifier(t)

	plain := Options{Service: "oidcmatrix"}
	legacySingle := Options{Service: "oidcmatrix", AuthnPublicKey: pub, AuthnIssuer: "users"}
	user := SecurityOptions{UserVerifier: verifier}
	both := SecurityOptions{ServiceVerifier: service.ServiceVerifier, UserVerifier: verifier}
	withIssuers := SecurityOptions{
		UserVerifier: verifier,
		UserIssuers:  map[string]authn.Issuer{"users": {Key: pub, Partition: "eu"}},
	}

	for _, tc := range []struct {
		name string
		mode appkit.SecurityMode
		o    Options
		s    SecurityOptions
		want string
	}{
		{"user configured", appkit.SecurityUserFacing, plain, user, ""},
		{"mixed configured", appkit.SecurityMixed, plain, both, ""},
		{"invalid zero verifier", appkit.SecurityUserFacing, plain, SecurityOptions{UserVerifier: &authn.OIDCVerifier{}}, "UserVerifier"},
		{"disabled rejects UserVerifier", appkit.SecurityDisabled, plain, user, "冲突"},
		{"internal_service rejects UserVerifier", appkit.SecurityInternalService, plain, both, "冲突"},
		{"UserVerifier conflicts with UserIssuers", appkit.SecurityUserFacing, plain, withIssuers, "不能同时"},
		{"UserVerifier conflicts with AuthnPublicKey", appkit.SecurityUserFacing, legacySingle, user, "不能同时"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHTTPSecurityWithOptions(tc.o, "dev", tc.mode, "OIDCMATRIX", false, tc.s)
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

func TestDeclarativeOIDCConfigAndConflicts(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "oidc.yaml")
	if err := os.WriteFile(cfgFile, []byte(`env: prod
security:
  mode: user_facing
  oidc:
    realms:
      - issuer: https://demo.sso.example.com/
        audience: https://api.example.com
        partition: demo
      - issuer: https://beta.sso.example.com/
        audience: https://api.example.com
        partition: beta
`), 0600); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), Options{
		Service: "bpoidc",
		Modules: func(Deps) ([]appkit.Module, error) { return nil, nil },
	}, RunOptions{ConfigFile: cfgFile})
	// 安全校验通过后会停在缺少 database.url
	if err == nil || !strings.Contains(err.Error(), "未配置 database.url") {
		t.Fatalf("合法 security.oidc 应通过安全校验，实际 %v", err)
	}

	// security.oidc 与 SecurityOptions.UserVerifier 互斥
	v, _ := testOIDCVerifier(t)
	err = RunWithSecurity(context.Background(), Options{
		Service: "bpoidc",
		Modules: func(Deps) ([]appkit.Module, error) { return nil, nil },
	}, RunOptions{ConfigFile: cfgFile}, SecurityOptions{UserVerifier: v})
	if err == nil || !strings.Contains(err.Error(), "不能同时配置") {
		t.Fatalf("security.oidc 与 UserVerifier 并存应报错，实际 %v", err)
	}

	// 缺少 audience 的声明式 OIDC 配置应在启动期 fail closed
	badFile := filepath.Join(t.TempDir(), "oidc-bad.yaml")
	if err := os.WriteFile(badFile, []byte(`env: prod
security:
  mode: user_facing
  oidc:
    realms:
      - issuer: https://demo.sso.example.com/
`), 0600); err != nil {
		t.Fatal(err)
	}
	err = Run(context.Background(), Options{
		Service: "bpoidc",
		Modules: func(Deps) ([]appkit.Module, error) { return nil, nil },
	}, RunOptions{ConfigFile: badFile})
	if err == nil || !strings.Contains(err.Error(), "security.oidc 配置无效") {
		t.Fatalf("缺 audience 的 security.oidc 应启动报错，实际 %v", err)
	}
}

func TestRunWithSecurityServesOIDCRequests(t *testing.T) {
	verifier, userPriv := testOIDCVerifier(t)
	service, _ := serviceSecurityFixture(t)
	for _, tc := range []struct {
		name     string
		mode     appkit.SecurityMode
		security SecurityOptions
	}{
		{"user_facing", appkit.SecurityUserFacing, SecurityOptions{UserVerifier: verifier}},
		{"mixed", appkit.SecurityMixed, SecurityOptions{UserVerifier: verifier, ServiceVerifier: service.ServiceVerifier}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OIDCRUN_ADDR", "127.0.0.1:0")
			t.Setenv("OIDCRUN_SECURITY__MODE", string(tc.mode))
			t.Setenv("OIDCRUN_LOG__LEVEL", "error")
			listening := make(chan string, 1)
			module := appkit.ModuleFunc("oidcmod", func(reg *appkit.Registry) error {
				reg.MountAuthenticated("GET /me", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					actor, ok := appkit.ActorFrom(r.Context())
					meta := callctx.From(r.Context())
					if !ok || actor.UserID != "u-1" || meta.Partition != "demo" || meta.TenantID != "org-1" {
						http.Error(w, "unexpected identity", http.StatusInternalServerError)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				}))
				return nil
			})
			o := Options{
				Service: "oidcrun",
				Modules: func(Deps) ([]appkit.Module, error) { return []appkit.Module{module}, nil },
				Minimal: func(Deps) ([]appkit.Module, error) { return []appkit.Module{module}, nil },
				AppOptions: func(Deps) []appkit.Option {
					return []appkit.Option{appkit.HTTPServer(func(s *http.Server) {
						s.BaseContext = func(ln net.Listener) context.Context {
							listening <- "http://" + ln.Addr().String()
							return context.Background()
						}
					})}
				},
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
				done <- RunWithSecurity(ctx, o, RunOptions{Minimal: true, ConfigFile: configFile}, tc.security)
			}()
			var base string
			select {
			case base = <-listening:
			case err := <-done:
				finished = true
				t.Fatalf("bootstrap failed: %v", err)
			case <-time.After(15 * time.Second):
				t.Fatal("bootstrap readiness timeout")
			}

			client := &http.Client{Timeout: 5 * time.Second}
			defer client.CloseIdleConnections()

			resp, err := client.Get(base + "/me")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("anonymous /me status = %d, want 401", resp.StatusCode)
			}

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/me", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+signTestOIDCToken(t, userPriv, "u-1", "org-1"))
			resp, err = client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("authenticated /me status = %d, want 204 body=%s", resp.StatusCode, body)
			}
		})
	}
}

func TestCoreProfileSupportsOIDCVerifierAndConfig(t *testing.T) {
	// 1. 声明式 security.oidc 与 UserVerifier 互斥
	cfgFile := filepath.Join(t.TempDir(), "core-oidc.yaml")
	if err := os.WriteFile(cfgFile, []byte(`env: dev
security:
  mode: user_facing
  oidc:
    realms:
      - issuer: https://demo.sso.example.com/
        audience: https://api.example.com
        partition: demo
`), 0600); err != nil {
		t.Fatal(err)
	}
	v, userPriv := testOIDCVerifier(t)
	opts := profileTestOptions(t)
	opts.ConfigFile = cfgFile
	opts.Capabilities = Capabilities{BusinessHTTP: true}
	opts.Security = SecurityOptions{UserVerifier: v}
	if _, err := NewCore(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "不能同时配置") {
		t.Fatalf("NewCore 应拒绝同时配置 security.oidc 与 UserVerifier，实际 %v", err)
	}

	// 2. 合法 security.oidc 声明式加载可通过 NewCore 校验并启动
	opts.Security = SecurityOptions{}
	coreCfg, err := NewCore(t.Context(), opts)
	if err != nil {
		t.Fatalf("NewCore with security.oidc: %v", err)
	}
	if err := coreCfg.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 3. Programmatic UserVerifier 经 Core.Start 挂载到 BusinessHTTP
	absentCfg := filepath.Join(t.TempDir(), "absent.yaml")
	if err := osWriteFile(absentCfg, []byte("env: dev\nsecurity:\n  mode: user_facing\n")); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	mod := profileTestModule{
		name: "oidc-core",
		register: func(reg *appkit.Registry) error {
			reg.MountAuthenticated("GET /core-me", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				actor, ok := appkit.ActorFrom(r.Context())
				if !ok || actor.UserID != "core-user" || callctx.From(r.Context()).Partition != "demo" {
					http.Error(w, "missing identity", http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			return nil
		},
	}
	coreOpts := ProfileOptions{
		Service:      "oidccore",
		ConfigFile:   absentCfg,
		DefaultAddr:  addr,
		Capabilities: Capabilities{BusinessHTTP: true},
		Security:     SecurityOptions{UserVerifier: v},
		Modules:      func(ProfileDeps) ([]appkit.Module, error) { return []appkit.Module{mod}, nil },
	}
	core, err := NewCore(t.Context(), coreOpts)
	if err != nil {
		t.Fatalf("NewCore with UserVerifier: %v", err)
	}
	running, err := core.Start(t.Context())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := running.Shutdown(shutdownCtx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/core-me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+signTestOIDCToken(t, userPriv, "core-user", "org-core"))
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("Core /core-me status = %d, want 204", resp.StatusCode)
	}
}

func TestDeclarativeOIDCRejectsLoopbackHTTPOutsideDev(t *testing.T) {
	writeCfg := func(env string) string {
		p := filepath.Join(t.TempDir(), env+".yaml")
		body := "env: " + env + `
security:
  mode: user_facing
  oidc:
    realms:
      - issuer: http://127.0.0.1:8081/
        audience: https://api.example.com
        partition: demo
`
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	for _, env := range []string{"prod", "staging"} {
		t.Run("reject/"+env, func(t *testing.T) {
			err := Run(context.Background(), Options{
				Service: "bpoidcenv",
				Modules: func(Deps) ([]appkit.Module, error) { return nil, nil },
			}, RunOptions{ConfigFile: writeCfg(env)})
			if err == nil || !strings.Contains(err.Error(), "必须使用 https") {
				t.Fatalf("env=%s 应拒绝明文 http JWKS，实际 %v", env, err)
			}
		})
	}

	t.Run("accept/dev", func(t *testing.T) {
		err := Run(context.Background(), Options{
			Service: "bpoidcenv",
			Modules: func(Deps) ([]appkit.Module, error) { return nil, nil },
		}, RunOptions{ConfigFile: writeCfg("dev")})
		if err == nil || !strings.Contains(err.Error(), "未配置 database.url") {
			t.Fatalf("env=dev 应允许 loopback http JWKS 并继续到 database.url 检查，实际 %v", err)
		}
	})
}
