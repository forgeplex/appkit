package authn

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/callctx"
)

func allowServiceScope(context.Context, appkit.ServicePrincipal, ServiceDelegation) error { return nil }

func serviceFixture(t *testing.T, policy ServiceDelegationPolicy) (*ServiceVerifier, *ServiceSigner, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewServiceVerifier(ServiceVerifierOptions{Audience: "ledger", Issuers: map[string]ServiceIssuer{"services": {Keys: map[string]ed25519.PublicKey{"current": pub}, Subjects: []string{"orders"}}}, AuthorizeDelegation: policy})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServiceSigner(ServiceSignerOptions{Issuer: "services", Subject: "orders", KeyID: "current", Key: priv, AuthorizeDelegation: policy})
	if err != nil {
		t.Fatal(err)
	}
	return v, s, pub, priv
}

func signedService(t *testing.T, key ed25519.PrivateKey, claims jwt.MapClaims, changeHeader func(map[string]any)) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["typ"], tok.Header["kid"] = serviceTokenType, "current"
	if changeHeader != nil {
		changeHeader(tok.Header)
	}
	raw, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func serviceMapClaims() jwt.MapClaims {
	now := time.Now().Add(-time.Second)
	return jwt.MapClaims{"iss": "services", "sub": "orders", "aud": "ledger", "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "purpose": servicePurpose}
}

func TestServiceSignerAndVerifier(t *testing.T) {
	v, signer, _, _ := serviceFixture(t, allowServiceScope)
	scope := ServiceDelegation{TenantID: "tenant", MerchantID: "merchant", Partition: "partition"}
	raw, exp, err := signer.SignWithExpiry(t.Context(), "ledger", scope)
	if err != nil {
		t.Fatal(err)
	}
	p, got, err := v.verify(t.Context(), raw)
	if err != nil || got != scope || p.Subject != "orders" || p.Issuer != "services" || p.KeyID != "current" || p.TenantID != "tenant" || p.MerchantID != "merchant" || !p.ExpiresAt.Equal(exp) || p.ExpiresAt.Sub(p.IssuedAt) != time.Minute {
		t.Fatalf("principal=%+v scope=%+v exp=%v err=%v", p, got, exp, err)
	}
	if err := v.Validate(); err != nil {
		t.Fatal(err)
	}
	var zero ServiceVerifier
	if zero.Validate() == nil || (*ServiceVerifier)(nil).Validate() == nil {
		t.Fatal("unconstructed verifier accepted")
	}
	if _, err := (*ServiceSigner)(nil).Sign(t.Context(), "ledger", scope); err == nil {
		t.Fatal("nil signer accepted")
	}
}

func TestServiceRejectsInvalidClaimsAndHeaders(t *testing.T) {
	v, _, pub, key := serviceFixture(t, allowServiceScope)
	_, other, _ := ed25519.GenerateKey(nil)
	cases := []struct {
		name   string
		claims func(jwt.MapClaims)
		header func(map[string]any)
		key    ed25519.PrivateKey
	}{
		{name: "bad-signature", key: other},
		{name: "missing-issuer", claims: func(c jwt.MapClaims) { delete(c, "iss") }},
		{name: "unknown-issuer", claims: func(c jwt.MapClaims) { c["iss"] = "other" }},
		{name: "empty-subject", claims: func(c jwt.MapClaims) { c["sub"] = "" }},
		{name: "unknown-subject", claims: func(c jwt.MapClaims) { c["sub"] = "admin" }},
		{name: "missing-audience", claims: func(c jwt.MapClaims) { delete(c, "aud") }},
		{name: "wrong-audience", claims: func(c jwt.MapClaims) { c["aud"] = "email" }},
		{name: "multiple-audiences", claims: func(c jwt.MapClaims) { c["aud"] = []string{"ledger", "email"} }},
		{name: "missing-expiry", claims: func(c jwt.MapClaims) { delete(c, "exp") }},
		{name: "expired", claims: func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Minute).Unix() }},
		{name: "missing-iat", claims: func(c jwt.MapClaims) { delete(c, "iat") }},
		{name: "future-iat", claims: func(c jwt.MapClaims) {
			c["iat"] = time.Now().Add(time.Minute).Unix()
			c["exp"] = time.Now().Add(2 * time.Minute).Unix()
		}},
		{name: "iat-after-exp", claims: func(c jwt.MapClaims) { c["iat"] = c["exp"].(int64) + 1 }},
		{name: "zero-lifetime", claims: func(c jwt.MapClaims) { c["exp"] = c["iat"] }},
		{name: "long-lifetime", claims: func(c jwt.MapClaims) { c["exp"] = time.Now().Add(time.Hour).Unix() }},
		{name: "not-yet-valid", claims: func(c jwt.MapClaims) { c["nbf"] = time.Now().Add(time.Minute).Unix() }},
		{name: "missing-purpose", claims: func(c jwt.MapClaims) { delete(c, "purpose") }},
		{name: "user-purpose", claims: func(c jwt.MapClaims) { c["purpose"] = "access" }},
		{name: "step-up-purpose", claims: func(c jwt.MapClaims) { c["purpose"] = "step-up" }},
		{name: "user-permissions", claims: func(c jwt.MapClaims) { c["perms"] = []string{"admin"} }},
		{name: "invalid-delegation", claims: func(c jwt.MapClaims) { c["tid"] = "tenant\nadmin" }},
		{name: "missing-kid", header: func(h map[string]any) { delete(h, "kid") }},
		{name: "unknown-kid", header: func(h map[string]any) { h["kid"] = "other" }},
		{name: "non-string-kid", header: func(h map[string]any) { h["kid"] = []string{"current"} }},
		{name: "missing-type", header: func(h map[string]any) { delete(h, "typ") }},
		{name: "user-type", header: func(h map[string]any) { h["typ"] = "JWT" }},
		{name: "jku", header: func(h map[string]any) { h["jku"] = "http://127.0.0.1:1/keys" }},
		{name: "embedded-jwk", header: func(h map[string]any) { h["jwk"] = map[string]string{"kty": "OKP"} }},
		{name: "critical-extension", header: func(h map[string]any) { h["crit"] = []string{"b64"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims := serviceMapClaims()
			if tc.claims != nil {
				tc.claims(claims)
			}
			priv := tc.key
			if priv == nil {
				priv = key
			}
			if _, _, err := v.verify(t.Context(), signedService(t, priv, claims, tc.header)); err == nil {
				t.Fatal("invalid service token accepted")
			}
		})
	}
	for name, method := range map[string]jwt.SigningMethod{"hmac": jwt.SigningMethodHS256, "none": jwt.SigningMethodNone} {
		t.Run(name, func(t *testing.T) {
			tok := jwt.NewWithClaims(method, serviceMapClaims())
			tok.Header["typ"], tok.Header["kid"] = serviceTokenType, "current"
			var signingKey any = []byte(pub)
			if name == "none" {
				signingKey = jwt.UnsafeAllowNoneSignatureType
			}
			raw, err := tok.SignedString(signingKey)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := v.verify(t.Context(), raw); err == nil {
				t.Fatal("wrong algorithm accepted")
			}
		})
	}
}

func TestServiceRejectsAmbiguousJSON(t *testing.T) {
	v, _, _, key := serviceFixture(t, nil)
	valid := signedService(t, key, serviceMapClaims(), nil)
	parts := strings.Split(valid, ".")
	for name, head := range map[string]string{
		"duplicate-kid":     `{"alg":"EdDSA","typ":"appkit-service+jwt","kid":"old","kid":"current"}`,
		"escaped-duplicate": `{"alg":"EdDSA","typ":"appkit-service+jwt","kid":"current","\u006bid":"current"}`,
		"trailing-json":     `{"alg":"EdDSA","typ":"appkit-service+jwt","kid":"current"}{}`,
	} {
		t.Run(name, func(t *testing.T) {
			input := base64.RawURLEncoding.EncodeToString([]byte(head)) + "." + parts[1]
			raw := input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(input)))
			if _, _, err := v.verify(t.Context(), raw); err == nil {
				t.Fatal("ambiguous JWT accepted")
			}
		})
	}
	body, _ := base64.RawURLEncoding.DecodeString(parts[1])
	duplicate := strings.TrimSuffix(string(body), "}") + `,"sub":"orders"}`
	input := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(duplicate))
	raw := input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(input)))
	if _, _, err := v.verify(t.Context(), raw); err == nil {
		t.Fatal("duplicate claim accepted")
	}
}

func TestServiceDelegationAndCancellation(t *testing.T) {
	v, signer, _, key := serviceFixture(t, nil)
	for _, scope := range []ServiceDelegation{{TenantID: "t"}, {MerchantID: "m"}, {Partition: "p"}} {
		if _, err := signer.Sign(t.Context(), "ledger", scope); err == nil {
			t.Fatalf("signer accepted unauthorised scope %+v", scope)
		}
		claims := serviceMapClaims()
		claims["tid"], claims["mid"], claims["partition"] = scope.TenantID, scope.MerchantID, scope.Partition
		if _, _, err := v.verify(t.Context(), signedService(t, key, claims, nil)); err == nil {
			t.Fatalf("verifier accepted unauthorised scope %+v", scope)
		}
	}
	ctx := callctx.With(t.Context(), callctx.Meta{TenantID: "untrusted", Partition: "untrusted"})
	raw, err := signer.Sign(ctx, "ledger", ServiceDelegation{})
	if err != nil {
		t.Fatal(err)
	}
	_, scope, err := v.verify(t.Context(), raw)
	if err != nil || scope != (ServiceDelegation{}) {
		t.Fatalf("signer inferred context delegation: %+v, %v", scope, err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := signer.Sign(canceled, "ledger", ServiceDelegation{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("sign cancellation=%v", err)
	}
	if _, _, err := v.verify(canceled, raw); !errors.Is(err, context.Canceled) {
		t.Fatalf("verify cancellation=%v", err)
	}
	deny := func(context.Context, appkit.ServicePrincipal, ServiceDelegation) error {
		return errors.New("PRIVATE credential detail")
	}
	v, signer, _, key = serviceFixture(t, deny)
	if _, err := signer.Sign(t.Context(), "ledger", ServiceDelegation{}); err == nil || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("policy failure leaked/ignored: %v", err)
	}
	if _, _, err := v.verify(t.Context(), signedService(t, key, serviceMapClaims(), nil)); err == nil || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("policy failure leaked/ignored: %v", err)
	}
	policyCtx, policyCancel := context.WithCancel(t.Context())
	defer policyCancel()
	_, signer, _, _ = serviceFixture(t, func(ctx context.Context, _ appkit.ServicePrincipal, _ ServiceDelegation) error {
		policyCancel()
		return ctx.Err()
	})
	if _, err := signer.Sign(policyCtx, "ledger", ServiceDelegation{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("policy cancellation lost identity: %v", err)
	}
}

func TestServiceRotationAndImmutableConfiguration(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	nextPub, nextKey, _ := ed25519.GenerateKey(nil)
	options := ServiceVerifierOptions{Audience: "ledger", Issuers: map[string]ServiceIssuer{"services": {Keys: map[string]ed25519.PublicKey{"current": pub, "next": nextPub}, Subjects: []string{"orders"}}}}
	v, err := NewServiceVerifier(options)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewServiceSigner(ServiceSignerOptions{Issuer: "services", Subject: "orders", KeyID: "current", Key: key})
	if err != nil {
		t.Fatal(err)
	}
	current := signedService(t, key, serviceMapClaims(), nil)
	next := signedService(t, nextKey, serviceMapClaims(), func(h map[string]any) { h["kid"] = "next" })
	// Mutate caller-owned keys, maps and subjects concurrently with verification.
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 50 {
			pub[i%len(pub)]++
			key[i%len(key)]++
			options.Issuers["services"].Subjects[0] = "attacker"
			options.Issuers["services"].Keys["current"] = nil
		}
		delete(options.Issuers, "services")
	})
	for range 8 {
		wg.Go(func() {
			for range 10 {
				for _, raw := range []string{current, next} {
					if _, _, err := v.verify(t.Context(), raw); err != nil {
						t.Errorf("snapshot verification: %v", err)
					}
				}
				raw, err := signer.Sign(t.Context(), "ledger", ServiceDelegation{})
				if err != nil {
					t.Error(err)
				} else if _, _, err := v.verify(t.Context(), raw); err != nil {
					t.Errorf("snapshot signer: %v", err)
				}
			}
		})
	}
	wg.Wait()
	rotated, err := NewServiceVerifier(ServiceVerifierOptions{Audience: "ledger", Issuers: map[string]ServiceIssuer{"services": {Keys: map[string]ed25519.PublicKey{"next": nextPub}, Subjects: []string{"orders"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := rotated.verify(t.Context(), current); err == nil {
		t.Fatal("removed old key still trusted")
	}
	if _, _, err := rotated.verify(t.Context(), next); err != nil {
		t.Fatal(err)
	}
}

func TestServiceConfigurationErrors(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	for _, edit := range []func(*ServiceVerifierOptions){
		func(o *ServiceVerifierOptions) { o.Audience = "" },
		func(o *ServiceVerifierOptions) { o.Issuers = nil },
		func(o *ServiceVerifierOptions) { o.MaxTTL = time.Hour },
		func(o *ServiceVerifierOptions) { o.MaxTTL = -time.Second },
		func(o *ServiceVerifierOptions) { o.ClockSkew = time.Minute },
		func(o *ServiceVerifierOptions) { o.ClockSkew = -time.Second },
		func(o *ServiceVerifierOptions) {
			o.Issuers["services"] = ServiceIssuer{Keys: map[string]ed25519.PublicKey{"k": pub}}
		},
		func(o *ServiceVerifierOptions) {
			o.Issuers["services"] = ServiceIssuer{Keys: map[string]ed25519.PublicKey{"k": pub}, Subjects: []string{"orders", "orders"}}
		},
		func(o *ServiceVerifierOptions) {
			o.Issuers["services"] = ServiceIssuer{Keys: map[string]ed25519.PublicKey{"": pub}, Subjects: []string{"orders"}}
		},
		func(o *ServiceVerifierOptions) {
			o.Issuers["services"] = ServiceIssuer{Keys: map[string]ed25519.PublicKey{"k": {}}, Subjects: []string{"orders"}}
		},
	} {
		o := ServiceVerifierOptions{Audience: "ledger", Issuers: map[string]ServiceIssuer{"services": {Keys: map[string]ed25519.PublicKey{"k": pub}, Subjects: []string{"orders"}}}}
		edit(&o)
		if _, err := NewServiceVerifier(o); err == nil {
			t.Fatalf("invalid verifier options accepted: %+v", o)
		}
	}
	for _, edit := range []func(*ServiceSignerOptions){
		func(o *ServiceSignerOptions) { o.Issuer = "" }, func(o *ServiceSignerOptions) { o.Subject = "" },
		func(o *ServiceSignerOptions) { o.KeyID = "" }, func(o *ServiceSignerOptions) { o.Key = nil },
		func(o *ServiceSignerOptions) { o.TTL = time.Hour }, func(o *ServiceSignerOptions) { o.TTL = time.Millisecond },
		func(o *ServiceSignerOptions) { o.Key = append(ed25519.PrivateKey(nil), key...); o.Key[40]++ },
	} {
		o := ServiceSignerOptions{Issuer: "services", Subject: "orders", KeyID: "current", Key: key}
		edit(&o)
		if _, err := NewServiceSigner(o); err == nil {
			t.Fatal("invalid signer options accepted")
		}
	}
}

func TestServiceHeaderShapeAndSkewBounds(t *testing.T) {
	v, signer, pub, key := serviceFixture(t, nil)
	raw, err := signer.Sign(t.Context(), "ledger", ServiceDelegation{})
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range map[string][]string{
		"empty": {""}, "basic": {"Basic abc"}, "empty-bearer": {"Bearer "},
		"duplicate":    {"Bearer " + raw, "Bearer " + raw},
		"comma-joined": {"Bearer " + raw + ", Bearer " + raw},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/public", nil)
			req.Header[HeaderServiceAuthorization] = values
			rec := httptest.NewRecorder()
			v.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid header reached handler") })).ServeHTTP(rec, req)
			if rec.Code != 401 {
				t.Fatalf("invalid header status=%d", rec.Code)
			}
		})
	}
	v, err = NewServiceVerifier(ServiceVerifierOptions{Audience: "ledger", MaxTTL: time.Minute, ClockSkew: 10 * time.Second, Issuers: map[string]ServiceIssuer{"services": {Keys: map[string]ed25519.PublicKey{"current": pub}, Subjects: []string{"orders"}}}})
	if err != nil {
		t.Fatal(err)
	}
	claims := serviceMapClaims()
	claims["iat"], claims["exp"] = time.Now().Add(2*time.Second).Unix(), time.Now().Add(32*time.Second).Unix()
	if _, _, err := v.verify(t.Context(), signedService(t, key, claims, nil)); err != nil {
		t.Fatalf("configured small clock skew rejected: %v", err)
	}
	claims["exp"] = claims["iat"].(int64) + 65
	if _, _, err := v.verify(t.Context(), signedService(t, key, claims, nil)); err == nil {
		t.Fatal("clock skew enlarged MaxTTL")
	}
}

func TestServicePolicyCannotMutateVerifiedPrincipal(t *testing.T) {
	policy := func(_ context.Context, p appkit.ServicePrincipal, _ ServiceDelegation) error {
		p.Audience[0] = "attacker"
		p.Subject = "attacker"
		return nil
	}
	v, signer, _, _ := serviceFixture(t, policy)
	raw, err := signer.Sign(t.Context(), "ledger", ServiceDelegation{TenantID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := v.verify(t.Context(), raw)
	if err != nil || p.Subject != "orders" || len(p.Audience) != 1 || p.Audience[0] != "ledger" {
		t.Fatalf("policy changed principal: %+v err=%v", p, err)
	}
}

func TestUserIssuerSnapshotAndCrossTokenRejection(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	issuers := map[string]Issuer{"services": {Key: pub, Partition: "p"}}
	called := false
	h := MultiIssuer(issuers)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if callctx.From(r.Context()).Partition != "p" {
			t.Error("issuer partition changed after construction")
		}
	}))
	userClaims := jwt.MapClaims{"iss": "services", "sub": "user", "exp": time.Now().Add(time.Hour).Unix()}
	user := sign(t, key, userClaims)
	for i := range pub {
		pub[i]++
	}
	issuers["services"] = Issuer{Partition: "attacker"}
	for _, tc := range []struct {
		name, raw string
		status    int
	}{
		{"user-snapshot", user, 200},
		{"service-token", signedService(t, key, serviceMapClaims(), nil), 401},
		{"service-with-user-type", signedService(t, key, serviceMapClaims(), func(h map[string]any) { h["typ"] = "JWT" }), 401},
		{"stepup-as-access", sign(t, key, jwt.MapClaims{"iss": "services", "sub": "user", "purpose": "step-up", "exp": time.Now().Add(time.Hour).Unix()}), 401},
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

func TestServiceMiddlewareMixedAndHeaderConflicts(t *testing.T) {
	v, signer, pub, key := serviceFixture(t, allowServiceScope)
	scope := ServiceDelegation{TenantID: "tenant", Partition: "p", MerchantID: "merchant"}
	raw, err := signer.Sign(t.Context(), "ledger", scope)
	if err != nil {
		t.Fatal(err)
	}
	user := sign(t, key, jwt.MapClaims{"iss": "services", "sub": "user", "tid": "tenant", "exp": time.Now().Add(time.Hour).Unix()})
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
			u := MultiIssuer(map[string]Issuer{"services": {Key: pub, Partition: "p"}})
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

func TestStrictServiceAppLoopback(t *testing.T) {
	v, signer, _, _ := serviceFixture(t, allowServiceScope)
	raw, err := signer.Sign(t.Context(), "ledger", ServiceDelegation{TenantID: "tenant", Partition: "p", MerchantID: "merchant"})
	if err != nil {
		t.Fatal(err)
	}
	url := startStrictIdentityApp(t, appkit.SecurityMixed, v.Middleware)
	transport := &http.Transport{Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	for _, tc := range []struct {
		name, path, token string
		head              http.Header
		want              int
	}{
		{name: "signed", path: "/internal", token: raw, want: 200},
		{name: "signed-matching-wire", path: "/internal", token: raw, head: http.Header{"X-Tenant-Id": {"tenant"}, "X-Partition": {"p"}, "X-Caller": {"orders"}, "X-Merchant-Id": {"merchant"}}, want: 200},
		{name: "boundary-preserves-conflict-evidence", path: "/internal", token: raw, head: http.Header{"X-Partition": {"forged"}}, want: 401},
		{name: "boundary-preserves-duplicate-evidence", path: "/internal", token: raw, head: http.Header{"X-Caller": {"orders", "orders"}}, want: 401},
		{name: "unsigned-only", path: "/internal", head: http.Header{"X-Caller": {"orders"}}, want: 401},
		{name: "service-not-user", path: "/private", token: raw, want: 401},
		{name: "invalid-public", path: "/public", token: "malformed", want: 401},
		{name: "probe", path: "/healthz", token: "malformed", want: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.head != nil {
				req.Header = tc.head.Clone()
			}
			if tc.token != "" {
				req.Header.Set(HeaderServiceAuthorization, "Bearer "+tc.token)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != tc.want {
				t.Fatalf("status=%d want=%d err=%v body=%s", resp.StatusCode, tc.want, err, body)
			}
			if tc.want == 200 && tc.path == "/internal" {
				var got strictIdentityObservation
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatal(err)
				}
				if !got.HasService || got.HasActor || got.Meta.Caller != "orders" || got.Meta.Partition != "p" || got.Meta.TenantID != "tenant" || got.Headers.Caller != "" || got.Headers.Partition != "" || got.Merchant != "" {
					t.Fatalf("strict service identity=%+v", got)
				}
			}
		})
	}
}
