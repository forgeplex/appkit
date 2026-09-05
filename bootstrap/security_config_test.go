package bootstrap

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/authn"
	"github.com/forgeplex/appkit/config"
)

func serviceConfigFixture(t *testing.T) (serviceVerifierConfig, ed25519.PrivateKey) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return serviceVerifierConfig{Audience: "catalog", Issuers: []serviceIssuerConfig{{
		Issuer: "https://issuer.example.test", Subjects: []string{"gateway"},
		Keys: []serviceKeyConfig{{ID: "key.1", PublicKey: base64.StdEncoding.EncodeToString(pub)}},
	}}}, key
}

func TestServicePublicKeyEncodings(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	validPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	for _, raw := range []string{validPEM, base64.StdEncoding.EncodeToString(pub)} {
		got, err := servicePublicKey(raw)
		if err != nil || !pub.Equal(got) {
			t.Fatalf("public key roundtrip: %v", err)
		}
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}))
	for _, raw := range []string{"", "malformed-secret", privatePEM, base64.StdEncoding.EncodeToString(key), validPEM + validPEM, validPEM + "trailing", "prefix\n" + validPEM} {
		_, err := servicePublicKey(raw)
		if err == nil {
			t.Fatal("invalid key was accepted")
		}
		if raw != "" && strings.Contains(err.Error(), raw) {
			t.Fatal("error exposes configuration value")
		}
	}
}

func TestServiceConfigurationRejectsInvalidTrustAndScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*serviceVerifierConfig)
	}{
		{"empty", func(c *serviceVerifierConfig) { *c = serviceVerifierConfig{} }},
		{"duplicate issuer", func(c *serviceVerifierConfig) { c.Issuers = append(c.Issuers, c.Issuers[0]) }},
		{"duplicate kid", func(c *serviceVerifierConfig) { c.Issuers[0].Keys = append(c.Issuers[0].Keys, c.Issuers[0].Keys[0]) }},
		{"no subjects", func(c *serviceVerifierConfig) { c.Issuers[0].Subjects = nil }},
		{"unknown delegation subject", func(c *serviceVerifierConfig) {
			c.Delegations = []serviceDelegationRule{{Issuer: c.Issuers[0].Issuer, Subject: "other", TenantID: "tenant"}}
		}},
		{"wildcard scope", func(c *serviceVerifierConfig) {
			c.Delegations = []serviceDelegationRule{{Issuer: c.Issuers[0].Issuer, Subject: "gateway", TenantID: "*"}}
		}},
		{"invalid scope", func(c *serviceVerifierConfig) {
			c.Delegations = []serviceDelegationRule{{Issuer: c.Issuers[0].Issuer, Subject: "gateway", TenantID: "tenant\nforged"}}
		}},
		{"empty scope", func(c *serviceVerifierConfig) {
			c.Delegations = []serviceDelegationRule{{Issuer: c.Issuers[0].Issuer, Subject: "gateway"}}
		}},
		{"duplicate scope", func(c *serviceVerifierConfig) {
			rule := serviceDelegationRule{Issuer: c.Issuers[0].Issuer, Subject: "gateway", TenantID: "tenant"}
			c.Delegations = []serviceDelegationRule{rule, rule}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _ := serviceConfigFixture(t)
			tc.modify(&cfg)
			if _, err := cfg.verifier(); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
}

func TestConfiguredDelegationMatchesExactTuple(t *testing.T) {
	cfg, key := serviceConfigFixture(t)
	cfg.Delegations = []serviceDelegationRule{{Issuer: cfg.Issuers[0].Issuer, Subject: "gateway", TenantID: "tenant-a", MerchantID: "merchant-a", Partition: "eu"}}
	v, err := cfg.verifier()
	if err != nil {
		t.Fatal(err)
	}
	// This test issuer deliberately signs every requested scope. The recipient's
	// independent allowlist must reject anything other than its exact tuple.
	signer, err := authn.NewServiceSigner(authn.ServiceSignerOptions{Issuer: cfg.Issuers[0].Issuer, Subject: "gateway", KeyID: "key.1", Key: key,
		AuthorizeDelegation: func(context.Context, appkit.ServicePrincipal, authn.ServiceDelegation) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	h := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, tc := range []struct {
		scope authn.ServiceDelegation
		want  int
	}{
		{authn.ServiceDelegation{}, http.StatusNoContent},
		{authn.ServiceDelegation{TenantID: "tenant-a", MerchantID: "merchant-a", Partition: "eu"}, http.StatusNoContent},
		{authn.ServiceDelegation{TenantID: "tenant-b", MerchantID: "merchant-a", Partition: "eu"}, http.StatusUnauthorized},
		{authn.ServiceDelegation{TenantID: "tenant-a", Partition: "eu"}, http.StatusUnauthorized},
		{authn.ServiceDelegation{TenantID: "tenant-a", MerchantID: "merchant-a", Partition: "us"}, http.StatusUnauthorized},
	} {
		raw, err := signer.Sign(t.Context(), cfg.Audience, tc.scope)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodGet, "/internal", nil)
		r.Header.Set(authn.HeaderServiceAuthorization, "Bearer "+raw)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("scope=%+v status=%d want=%d", tc.scope, w.Code, tc.want)
		}
	}
}

func TestRunLoadsServiceConfigBeforeModuleAssembly(t *testing.T) {
	cfg, _ := serviceConfigFixture(t)
	path := filepath.Join(t.TempDir(), "service.yaml")
	yaml := fmt.Sprintf(`env: dev
security:
  mode: internal_service
  service:
    audience: catalog
    max_ttl: 2m
    clock_skew: 5s
    issuers:
      - issuer: https://issuer.example.test
        subjects: [gateway]
        keys:
          - id: key.1
            public_key: %s
    delegations:
      - issuer: https://issuer.example.test
        subject: gateway
        tenant_id: tenant-a
        partition: eu
`, cfg.Issuers[0].Keys[0].PublicKey)
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load[runtimeSecurityConfig](config.Options{Files: []string{path}})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Security.Service == nil || loaded.Security.Service.MaxTTL != 2*time.Minute || loaded.Security.Service.ClockSkew != 5*time.Second || loaded.Security.Service.Issuers[0].Issuer != "https://issuer.example.test" || loaded.Security.Service.Issuers[0].Keys[0].ID != "key.1" {
		t.Fatal("service configuration mapping drifted")
	}
	assembled := errors.New("expected assembly sentinel")
	o := Options{Service: "serviceload", Modules: func(Deps) ([]appkit.Module, error) { return nil, nil }, Minimal: func(Deps) ([]appkit.Module, error) { return nil, assembled }}
	err = Run(t.Context(), o, RunOptions{Minimal: true, ConfigFile: path})
	if !errors.Is(err, assembled) {
		t.Fatalf("configured verifier did not reach module assembly: %v", err)
	}
	service, _ := serviceSecurityFixture(t)
	err = RunWithSecurity(t.Context(), o, RunOptions{Minimal: true, ConfigFile: path}, service)
	if err == nil || !strings.Contains(err.Error(), "不能同时配置") {
		t.Fatalf("ambiguous verifier accepted: %v", err)
	}
}
