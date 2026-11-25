package authn

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/callctx"
	"github.com/forgeplex/appkit/httpserver"
)

func oidcSignWithHeader(t *testing.T, method jwt.SigningMethod, key any, kid, typ string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	if typ != "" {
		tok.Header["typ"] = typ
	}
	raw, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return raw
}

func oidcSign(t *testing.T, priv ed25519.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	return oidcSignWithHeader(t, jwt.SigningMethodEdDSA, priv, "k-ed", "at+jwt", claims)
}

func rsaJWK(kid string, pub *rsa.PublicKey) map[string]any {
	e := big.NewInt(int64(pub.E)).Bytes()
	return map[string]any{
		"kty": "RSA",
		"use": "sig",
		"alg": "RS256",
		"kid": kid,
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(e),
	}
}

func ecJWK(kid string, pub *ecdsa.PublicKey) map[string]any {
	pad := func(v *big.Int) string {
		buf := make([]byte, 32)
		b := v.Bytes()
		copy(buf[32-len(b):], b)
		return base64.RawURLEncoding.EncodeToString(buf)
	}
	return map[string]any{
		"kty": "EC",
		"crv": "P-256",
		"use": "sig",
		"alg": "ES256",
		"kid": kid,
		"x":   pad(pub.X),
		"y":   pad(pub.Y),
	}
}

func TestOIDCVerifierMultiRealmAndJWKS(t *testing.T) {
	rsaKey1, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey2, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	var rotated atomic.Bool
	var fetches atomic.Int32
	jwksA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		keys := []map[string]any{rsaJWK("rsa-1", &rsaKey1.PublicKey)}
		if rotated.Load() {
			keys = append(keys, rsaJWK("rsa-2", &rsaKey2.PublicKey))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	t.Cleanup(jwksA.Close)

	jwksB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{ecJWK("ec-1", &ecKey.PublicKey)}})
	}))
	t.Cleanup(jwksB.Close)

	now := time.Unix(1_760_000_000, 0)
	v, err := NewOIDCVerifier(OIDCVerifierOptions{
		Realms: []OIDCRealm{
			{
				Issuer:     "https://demo.sso.example.com/",
				Audience:   "https://api.psp.example.com",
				Partition:  "demo",
				JWKSURL:    jwksA.URL,
				RequireOrg: true,
			},
			{
				Issuer:    "https://beta.sso.example.com/",
				Audience:  "https://api.psp.example.com",
				Partition: "beta",
				JWKSURL:   jwksB.URL,
			},
		},
		HTTPClient: jwksA.Client(),
		CacheTTL:   5 * time.Minute,
		MinRefresh: 10 * time.Second,
		StepUpACRs: []string{"urn:forgeplex:mfa"},
		now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	authTime := now.Add(-2 * time.Minute).Unix()
	tokA := oidcSignWithHeader(t, jwt.SigningMethodRS256, rsaKey1, "rsa-1", "at+jwt", jwt.MapClaims{
		"iss":         "https://demo.sso.example.com/",
		"sub":         "user-a",
		"aud":         []string{"https://api.psp.example.com", "https://demo.sso.example.com/userinfo"},
		"org_id":      "org-demo-1",
		"permissions": []string{"files:download", "ledger:read"},
		"amr":         []string{"pwd", "mfa"},
		"auth_time":   authTime,
		"iat":         now.Unix(),
		"exp":         now.Add(15 * time.Minute).Unix(),
	})
	tokB := oidcSignWithHeader(t, jwt.SigningMethodES256, ecKey, "ec-1", "at+jwt", jwt.MapClaims{
		"iss":         "https://beta.sso.example.com/",
		"sub":         "user-b",
		"aud":         "https://api.psp.example.com",
		"org_id":      "org-beta-1",
		"permissions": []string{"webhook:read"},
		"acr":         "urn:forgeplex:mfa",
		"auth_time":   authTime,
		"iat":         now.Unix(),
		"exp":         now.Add(15 * time.Minute).Unix(),
	})

	actorA, partA, err := v.Verify(t.Context(), tokA)
	if err != nil {
		t.Fatalf("verify realm A: %v", err)
	}
	if partA != "demo" || actorA.UserID != "user-a" || actorA.TenantID != "org-demo-1" ||
		!slices.Equal(actorA.Perms, []string{"files:download", "ledger:read"}) ||
		!actorA.StepUpAt.Equal(time.Unix(authTime, 0)) {
		t.Fatalf("unexpected actor A: %+v part=%q", actorA, partA)
	}
	// Second verification hits JWKS cache.
	if _, _, err := v.Verify(t.Context(), tokA); err != nil {
		t.Fatalf("cached verify realm A: %v", err)
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("JWKS fetched %d times, want 1", got)
	}

	actorB, partB, err := v.Verify(t.Context(), tokB)
	if err != nil {
		t.Fatalf("verify realm B: %v", err)
	}
	if partB != "beta" || actorB.UserID != "user-b" || actorB.TenantID != "org-beta-1" ||
		!slices.Equal(actorB.Perms, []string{"webhook:read"}) ||
		!actorB.StepUpAt.Equal(time.Unix(authTime, 0)) {
		t.Fatalf("unexpected actor B: %+v part=%q", actorB, partB)
	}

	// Key rotation: unknown kid triggers refresh after MinRefresh.
	rotated.Store(true)
	tokARotated := oidcSignWithHeader(t, jwt.SigningMethodRS256, rsaKey2, "rsa-2", "at+jwt", jwt.MapClaims{
		"iss":    "https://demo.sso.example.com/",
		"sub":    "user-a",
		"aud":    "https://api.psp.example.com",
		"org_id": "org-demo-1",
		"exp":    now.Add(15 * time.Minute).Unix(),
	})
	// Immediately throttled before MinRefresh passes.
	if _, _, err := v.Verify(t.Context(), tokARotated); err == nil {
		t.Fatal("expected throttled refresh for unknown kid within MinRefresh")
	}
	now = now.Add(15 * time.Second)
	if _, _, err := v.Verify(t.Context(), tokARotated); err != nil {
		t.Fatalf("verify rotated key after MinRefresh: %v", err)
	}
	if got := fetches.Load(); got != 2 {
		t.Fatalf("JWKS fetched %d times after rotation, want 2", got)
	}
}

func TestOIDCVerifierRejections(t *testing.T) {
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_760_000_000, 0)
	v, err := NewOIDCVerifier(OIDCVerifierOptions{
		Realms: []OIDCRealm{{
			Issuer:     "https://demo.sso.example.com/",
			Audience:   "https://api.psp.example.com",
			Partition:  "demo",
			StaticKeys: map[string]crypto.PublicKey{"k1": &ecKey.PublicKey},
			RequireOrg: true,
		}},
		now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	makeTok := func(key *ecdsa.PrivateKey, kid, typ string, claims jwt.MapClaims) string {
		return oidcSignWithHeader(t, jwt.SigningMethodES256, key, kid, typ, claims)
	}
	base := func(overrides map[string]any) jwt.MapClaims {
		c := jwt.MapClaims{
			"iss":    "https://demo.sso.example.com/",
			"sub":    "u1",
			"aud":    "https://api.psp.example.com",
			"org_id": "org-1",
			"exp":    now.Add(time.Hour).Unix(),
		}
		for k, val := range overrides {
			if val == nil {
				delete(c, k)
			} else {
				c[k] = val
			}
		}
		return c
	}

	cases := []struct {
		name string
		raw  string
	}{
		{"wrong-key", makeTok(otherKey, "k1", "at+jwt", base(nil))},
		{"unknown-kid", makeTok(ecKey, "k-missing", "at+jwt", base(nil))},
		{"unknown-issuer", makeTok(ecKey, "k1", "at+jwt", base(map[string]any{"iss": "https://evil.example.com/"}))},
		{"wrong-audience", makeTok(ecKey, "k1", "at+jwt", base(map[string]any{"aud": "https://other.example.com"}))},
		{"expired", makeTok(ecKey, "k1", "at+jwt", base(map[string]any{"exp": now.Add(-time.Minute).Unix()}))},
		{"missing-exp", makeTok(ecKey, "k1", "at+jwt", base(map[string]any{"exp": nil}))},
		{"missing-sub", makeTok(ecKey, "k1", "at+jwt", base(map[string]any{"sub": ""}))},
		{"missing-required-org", makeTok(ecKey, "k1", "at+jwt", base(map[string]any{"org_id": nil}))},
		{"machine-token-dash", makeTok(ecKey, "k1", "at+jwt", base(map[string]any{"gty": "client-credentials"}))},
		{"machine-token-underscore", makeTok(ecKey, "k1", "at+jwt", base(map[string]any{"gty": "client_credentials"}))},
		{"service-typ", makeTok(ecKey, "k1", serviceTokenType, base(nil))},
		{"logout-typ", makeTok(ecKey, "k1", "logout+jwt", base(nil))},
		{"service-purpose", makeTok(ecKey, "k1", "JWT", base(map[string]any{"purpose": "service"}))},
		{"hs256-alg-confusion", oidcSignWithHeader(t, jwt.SigningMethodHS256, []byte("secret"), "k1", "at+jwt", base(nil))},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := v.Verify(t.Context(), tc.raw); err == nil {
				t.Fatalf("case %s should have been rejected", tc.name)
			}
		})
	}
}

func TestOIDCMiddlewareBehavior(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewOIDCVerifier(OIDCVerifierOptions{
		Realms: []OIDCRealm{{
			Issuer:     "https://demo.sso.example.com/",
			Audience:   "https://api.psp.example.com",
			Partition:  "demo",
			StaticKeys: map[string]crypto.PublicKey{"k-ed": pub},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var gotMeta callctx.Meta
	var gotActor appkit.Actor
	var hasActor bool
	h := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMeta = callctx.From(r.Context())
		gotActor, hasActor = appkit.ActorFrom(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	// Anonymous request passes through with no actor.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/public", nil))
	if rec.Code != http.StatusNoContent || hasActor {
		t.Fatalf("anonymous request: code=%d hasActor=%v", rec.Code, hasActor)
	}

	// Health probe bypasses bad Authorization header.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Authorization", "Bearer broken")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("healthz with bad bearer: code=%d", rec.Code)
	}

	// Multiple Authorization headers rejected.
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Add("Authorization", "Bearer a")
	req.Header.Add("Authorization", "Bearer b")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("multiple headers: code=%d", rec.Code)
	}

	// Valid token populates Actor and callctx.
	raw := oidcSign(t, priv, jwt.MapClaims{
		"iss":         "https://demo.sso.example.com/",
		"sub":         "user-1",
		"aud":         "https://api.psp.example.com",
		"org_id":      "org-1",
		"permissions": []string{"ledger:read"},
		"exp":         time.Now().Add(time.Hour).Unix(),
	})
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || !hasActor || gotActor.UserID != "user-1" || gotMeta.Partition != "demo" || gotMeta.TenantID != "org-1" {
		t.Fatalf("valid token: code=%d hasActor=%v actor=%+v meta=%+v", rec.Code, hasActor, gotActor, gotMeta)
	}
}

func TestStrictAppMultiRealmIdentityIntegration(t *testing.T) {
	keyA, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const audience = "https://api.psp.example.com"
	token := func(key *ecdsa.PrivateKey, issuer, subject, orgID string, perms []string) string {
		return oidcSignWithHeader(t, jwt.SigningMethodES256, key, "k1", "at+jwt", jwt.MapClaims{
			"iss":         issuer,
			"sub":         subject,
			"aud":         audience,
			"org_id":      orgID,
			"permissions": perms,
			"partition":   "forged-signed-claim",
			"exp":         time.Now().Add(time.Hour).Unix(),
		})
	}
	readPerms := []string{"fixture:read"}
	alpha := token(keyA, "https://a.sso.example.com/", "user-a", "shared-tenant", readPerms)
	beta := token(keyB, "https://b.sso.example.com/", "user-b", "shared-tenant", nil)
	verifier, err := NewOIDCVerifier(OIDCVerifierOptions{
		Realms: []OIDCRealm{
			{Issuer: "https://a.sso.example.com/", Audience: audience, Partition: "partition-a", StaticKeys: map[string]crypto.PublicKey{"k1": &keyA.PublicKey}},
			{Issuer: "https://b.sso.example.com/", Audience: audience, Partition: "partition-b", StaticKeys: map[string]crypto.PublicKey{"k1": &keyB.PublicKey}},
			{Issuer: "https://flat.sso.example.com/", Audience: audience, StaticKeys: map[string]crypto.PublicKey{"k1": &keyA.PublicKey}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []appkit.SecurityMode{appkit.SecurityUserFacing, appkit.SecurityMixed} {
		t.Run(string(mode), func(t *testing.T) {
			url := startStrictIdentityApp(t, mode, verifier.Middleware)
			transport := &http.Transport{Proxy: nil}
			t.Cleanup(transport.CloseIdleConnections)
			client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
			cases := []struct {
				name, path, bearer, subject, tenant, partition string
				perms                                          []string
				status                                         int
			}{
				{"realm-a", "/private", alpha, "user-a", "shared-tenant", "partition-a", readPerms, 200},
				{"realm-b", "/private", beta, "user-b", "shared-tenant", "partition-b", nil, 200},
				{"no-partition", "/private", token(keyA, "https://flat.sso.example.com/", "user-flat", "", nil), "user-flat", "", "", nil, 200},
				{"no-tenant", "/private", token(keyA, "https://a.sso.example.com/", "user-a", "", nil), "user-a", "", "partition-a", nil, 200},
				{"anonymous-public", "/public", "", "", "", "", nil, 200},
				{"authenticated-public", "/public", alpha, "user-a", "shared-tenant", "partition-a", readPerms, 200},
				{"anonymous-private", "/private", "", "", "", "", nil, 401},
				{"anonymous-permission", "/permission", "", "", "", "", nil, 401},
				{"permitted", "/permission", alpha, "user-a", "shared-tenant", "partition-a", readPerms, 200},
				{"denied", "/permission", beta, "", "", "", nil, 403},
				{"wrong-key-for-realm", "/private", token(keyA, "https://b.sso.example.com/", "forged", "forged", readPerms), "", "", "", nil, 401},
				{"unknown-realm", "/private", token(keyA, "https://unknown.sso.example.com/", "forged", "forged", readPerms), "", "", "", nil, 401},
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

func TestOIDCVerifierSnapshotAndCrossTokenRejection(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	realms := []OIDCRealm{{
		Issuer:     "services",
		Audience:   "ledger",
		Partition:  "p",
		StaticKeys: map[string]crypto.PublicKey{"k-ed": pub, "k1": pub},
	}}
	u, err := NewOIDCVerifier(OIDCVerifierOptions{Realms: realms})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	h := u.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if callctx.From(r.Context()).Partition != "p" {
			t.Error("issuer partition changed after construction")
		}
	}))
	userClaims := jwt.MapClaims{"iss": "services", "sub": "user", "aud": "ledger", "exp": time.Now().Add(time.Hour).Unix()}
	user := oidcSign(t, key, userClaims)
	for i := range pub {
		pub[i]++
	}
	realms[0].Partition = "attacker"
	for _, tc := range []struct {
		name, raw string
		status    int
	}{
		{"user-snapshot", user, 200},
		{"service-token", signedService(t, key, serviceMapClaims(), nil), 401},
		{"service-with-user-type", signedService(t, key, serviceMapClaims(), func(h map[string]any) { h["typ"] = "JWT" }), 401},
		{"machine-as-user", oidcSign(t, key, jwt.MapClaims{"iss": "services", "sub": "user", "aud": "ledger", "gty": "client-credentials", "exp": time.Now().Add(time.Hour).Unix()}), 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called = false
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set("Authorization", "Bearer "+tc.raw)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status || called != (tc.status == 200) {
				t.Fatalf("status=%d called=%v body=%s", rec.Code, called, rec.Body)
			}
		})
	}
}

func TestOIDCServiceMiddlewareMixedAndHeaderConflicts(t *testing.T) {
	v, signer, pub, key := serviceFixture(t, allowServiceScope)
	scope := ServiceDelegation{TenantID: "tenant", Partition: "p", MerchantID: "merchant"}
	raw, err := signer.Sign(t.Context(), "ledger", scope)
	if err != nil {
		t.Fatal(err)
	}
	user := oidcSign(t, key, jwt.MapClaims{"iss": "services", "sub": "user", "aud": "ledger", "org_id": "tenant", "exp": time.Now().Add(time.Hour).Unix()})
	uv, err := NewOIDCVerifier(OIDCVerifierOptions{Realms: []OIDCRealm{{
		Issuer:     "services",
		Audience:   "ledger",
		Partition:  "p",
		StaticKeys: map[string]crypto.PublicKey{"k-ed": pub, "k1": pub},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range []string{"user-first", "service-first"} {
		t.Run(order, func(t *testing.T) {
			called := false
			var meta callctx.Meta
			var hasActor bool
			var p appkit.ServicePrincipal
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				meta = callctx.From(r.Context())
				_, hasActor = appkit.ActorFrom(r.Context())
				p, _ = appkit.ServicePrincipalFrom(r.Context())
			})
			u := uv.Middleware
			h := u(v.Middleware(next))
			if order == "service-first" {
				h = v.Middleware(u(next))
			}
			for _, tc := range []struct {
				name, service, user string
				head                http.Header
				want                int
			}{
				{name: "service-only", service: raw, want: 200},
				{name: "mixed", service: raw, user: user, want: 200},
				{name: "unsigned-match", service: raw, head: http.Header{"X-Caller": {"orders"}, "X-Tenant-Id": {"tenant"}, "X-Partition": {"p"}, "X-Merchant-Id": {"merchant"}}, want: 200},
				{name: "caller-conflict", service: raw, head: http.Header{"X-Caller": {"forged"}}, want: 401},
				{name: "tenant-conflict", service: raw, head: http.Header{"X-Tenant-Id": {"forged"}}, want: 401},
				{name: "partition-conflict", service: raw, head: http.Header{"X-Partition": {"forged"}}, want: 401},
				{name: "merchant-conflict", service: raw, head: http.Header{"X-Merchant-Id": {"forged"}}, want: 401},
				{name: "duplicate-hint", service: raw, head: http.Header{"X-Caller": {"orders", "orders"}}, want: 401},
				{name: "user-as-service", service: user, want: 401},
				{name: "service-as-user", user: raw, want: 401},
				{name: "invalid-present", service: "malformed", want: 401},
			} {
				t.Run(tc.name, func(t *testing.T) {
					called, hasActor, meta, p = false, false, callctx.Meta{}, appkit.ServicePrincipal{}
					req := httptest.NewRequest(http.MethodGet, "/x", nil)
					if tc.head != nil {
						req.Header = tc.head.Clone()
					}
					if tc.service != "" {
						req.Header.Set(HeaderServiceAuthorization, "Bearer "+tc.service)
					}
					if tc.user != "" {
						req.Header.Set("Authorization", "Bearer "+tc.user)
					}
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					if rec.Code != tc.want || called != (tc.want == 200) {
						t.Fatalf("status=%d called=%v body=%s", rec.Code, called, rec.Body)
					}
					if tc.want == 200 && (meta.Caller != "orders" || meta.TenantID != "tenant" || meta.Partition != "p" || hasActor != (tc.user != "") || p.Subject != "orders") {
						t.Fatalf("scope/principal mismatch: %+v %+v actor=%v", meta, p, hasActor)
					}
				})
			}
			for _, badScope := range []ServiceDelegation{{}, {TenantID: "other", Partition: "p"}, {TenantID: "tenant"}, {TenantID: "tenant", Partition: "other"}} {
				token, _ := signer.Sign(t.Context(), "ledger", badScope)
				req := httptest.NewRequest(http.MethodGet, "/x", nil)
				req.Header.Set("Authorization", "Bearer "+user)
				req.Header.Set(HeaderServiceAuthorization, "Bearer "+token)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != 401 {
					t.Fatalf("scope conflict accepted %+v: %d", badScope, rec.Code)
				}
			}
		})
	}
}

func TestOIDCVerifierRejectsNonLoopbackHTTPJWKS(t *testing.T) {
	rejected := []struct {
		name  string
		realm OIDCRealm
	}{
		{
			name: "explicit-public-http",
			realm: OIDCRealm{
				Issuer:   "https://sso.example.com/",
				Audience: "https://api.psp.example.com",
				JWKSURL:  "http://sso.example.com/.well-known/jwks.json",
			},
		},
		{
			name: "derived-public-http",
			realm: OIDCRealm{
				Issuer:   "http://sso.example.com/",
				Audience: "https://api.psp.example.com",
			},
		},
		{
			name: "private-network-http",
			realm: OIDCRealm{
				Issuer:   "https://sso.example.com/",
				Audience: "https://api.psp.example.com",
				JWKSURL:  "http://10.0.0.5:8081/.well-known/jwks.json",
			},
		},
		{
			name: "userinfo-in-https-jwks",
			realm: OIDCRealm{
				Issuer:   "https://sso.example.com/",
				Audience: "https://api.psp.example.com",
				JWKSURL:  "https://user:pass@sso.example.com/.well-known/jwks.json",
			},
		},
	}
	for _, tc := range rejected {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			if _, err := NewOIDCVerifier(OIDCVerifierOptions{Realms: []OIDCRealm{tc.realm}}); err == nil {
				t.Fatalf("expected error for %+v", tc.realm)
			}
		})
	}

	accepted := []struct {
		name  string
		realm OIDCRealm
	}{
		{
			name: "loopback-ipv4",
			realm: OIDCRealm{
				Issuer:   "http://127.0.0.1:8081/",
				Audience: "https://api.psp.example.com",
				JWKSURL:  "http://127.0.0.1:8081/.well-known/jwks.json",
			},
		},
		{
			name: "loopback-ipv6",
			realm: OIDCRealm{
				Issuer:   "https://demo.sso.example.com/",
				Audience: "https://api.psp.example.com",
				JWKSURL:  "http://[::1]:8081/.well-known/jwks.json",
			},
		},
		{
			name: "loopback-localhost",
			realm: OIDCRealm{
				Issuer:   "https://demo.sso.example.com/",
				Audience: "https://api.psp.example.com",
				JWKSURL:  "http://localhost:8081/.well-known/jwks.json",
			},
		},
		{
			name: "https-public",
			realm: OIDCRealm{
				Issuer:   "https://sso.example.com/",
				Audience: "https://api.psp.example.com",
				JWKSURL:  "https://sso.example.com/.well-known/jwks.json",
			},
		},
	}
	for _, tc := range accepted {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			if _, err := NewOIDCVerifier(OIDCVerifierOptions{Realms: []OIDCRealm{tc.realm}}); err != nil {
				t.Fatalf("unexpected error for %+v: %v", tc.realm, err)
			}
		})
	}
}

func TestOIDCStepUpAuthTimeAndChallengeEnforcement(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Unix(1_760_000_000, 0)
	vUnit, err := NewOIDCVerifier(OIDCVerifierOptions{
		Realms: []OIDCRealm{{
			Issuer:     "https://demo.sso.example.com/",
			Audience:   "https://api.psp.example.com",
			Partition:  "demo",
			StaticKeys: map[string]crypto.PublicKey{"k-ed": pub},
		}},
		ClockSkew: 5 * time.Second,
		now:       func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatal(err)
	}

	signAuthTime := func(baseTime time.Time, authTime int64, amr []string) string {
		claims := jwt.MapClaims{
			"iss":         "https://demo.sso.example.com/",
			"sub":         "user-1",
			"aud":         "https://api.psp.example.com",
			"org_id":      "org-1",
			"permissions": []string{"fixture:read", "fixture:sensitive"},
			"auth_time":   authTime,
			"exp":         baseTime.Add(time.Hour).Unix(),
		}
		if len(amr) > 0 {
			claims["amr"] = amr
		}
		return oidcSign(t, priv, claims)
	}

	if _, _, err := vUnit.Verify(t.Context(), signAuthTime(fixedNow, fixedNow.Add(10*time.Second).Unix(), []string{"mfa"})); err == nil {
		t.Fatal("expected future auth_time beyond skew to be rejected")
	}
	if _, _, err := vUnit.Verify(t.Context(), signAuthTime(fixedNow, 0, []string{"mfa"})); err == nil {
		t.Fatal("expected non-positive auth_time to be rejected")
	}
	actorSkew, _, err := vUnit.Verify(t.Context(), signAuthTime(fixedNow, fixedNow.Add(3*time.Second).Unix(), []string{"mfa"}))
	if err != nil {
		t.Fatalf("auth_time within skew rejected: %v", err)
	}
	if !actorSkew.StepUpAt.Equal(fixedNow) {
		t.Fatalf("StepUpAt = %v, want clamped %v", actorSkew.StepUpAt, fixedNow)
	}

	// End-to-end Challenge route check against appkit.Registry.Require.
	verifier, err := NewOIDCVerifier(OIDCVerifierOptions{
		Realms: []OIDCRealm{{
			Issuer:     "https://demo.sso.example.com/",
			Audience:   "https://api.psp.example.com",
			Partition:  "demo",
			StaticKeys: map[string]crypto.PublicKey{"k-ed": pub},
		}},
		ClockSkew: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	okHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	module := appkit.ModuleFunc("challenge-fixture", func(reg *appkit.Registry) error {
		reg.Permissions(
			appkit.PermissionDecl{Code: "fixture:read"},
			appkit.PermissionDecl{Code: "fixture:sensitive", Challenge: true},
		)
		reg.MountPermission("GET /normal", "fixture:read", okHandler)
		reg.MountPermission("GET /challenge", "fixture:sensitive", okHandler)
		return nil
	})
	listening := make(chan string, 1)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := appkit.New(
		[]appkit.Module{module},
		appkit.Security(appkit.SecurityUserFacing),
		appkit.HTTPAddr("127.0.0.1:0"),
		appkit.Logger(log),
		appkit.ShutdownTimeout(2*time.Second),
		appkit.Middleware(httpserver.Base(log)...),
		appkit.Middleware(verifier.Middleware),
		appkit.HTTPServer(func(s *http.Server) {
			s.BaseContext = func(ln net.Listener) context.Context {
				listening <- "http://" + ln.Addr().String()
				return context.Background()
			}
		}),
	)
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
			t.Error("App did not stop within shutdown budget")
		}
	})

	var baseURL string
	select {
	case baseURL = <-listening:
	case err := <-done:
		finished = true
		t.Fatalf("App failed before listening: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("App did not start within budget")
	}

	client := &http.Client{Timeout: 2 * time.Second}
	now := time.Now()
	doReq := func(path, raw string) (int, string) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+raw)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		code := ""
		if len(body) > 0 {
			code = apperr.FromProblem(resp.StatusCode, body).Code()
		}
		return resp.StatusCode, code
	}

	freshMFA := signAuthTime(now, now.Add(-time.Minute).Unix(), []string{"pwd", "mfa"})
	futureMFA := signAuthTime(now, now.Add(2*time.Minute).Unix(), []string{"mfa"})
	staleMFA := signAuthTime(now, now.Add(-10*time.Minute).Unix(), []string{"mfa"})
	freshPwdOnly := signAuthTime(now, now.Add(-time.Minute).Unix(), []string{"pwd"})

	for _, tc := range []struct {
		name     string
		path     string
		token    string
		wantCode int
		wantErr  string
	}{
		{"fresh-mfa-normal", "/normal", freshMFA, http.StatusNoContent, ""},
		{"fresh-mfa-challenge", "/challenge", freshMFA, http.StatusNoContent, ""},
		{"future-auth-time-normal", "/normal", futureMFA, http.StatusUnauthorized, apperr.CodeUnauthenticated},
		{"future-auth-time-challenge", "/challenge", futureMFA, http.StatusUnauthorized, apperr.CodeUnauthenticated},
		{"stale-mfa-normal", "/normal", staleMFA, http.StatusNoContent, ""},
		{"stale-mfa-challenge", "/challenge", staleMFA, http.StatusForbidden, apperr.CodeStepUpRequired},
		{"no-mfa-amr-normal", "/normal", freshPwdOnly, http.StatusNoContent, ""},
		{"no-mfa-amr-challenge", "/challenge", freshPwdOnly, http.StatusForbidden, apperr.CodeStepUpRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, errCode := doReq(tc.path, tc.token)
			if status != tc.wantCode || (tc.wantErr != "" && errCode != tc.wantErr) {
				t.Fatalf("status=%d (want %d), code=%q (want %q)", status, tc.wantCode, errCode, tc.wantErr)
			}
		})
	}
}
