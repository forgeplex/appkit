package bootstrap

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/authn"
)

// Configuration uses lists instead of maps keyed by issuer URLs: koanf's dot
// path delimiter must not reinterpret dots inside issuer names or key IDs.
// Only public keys are accepted. Private credentials belong to the caller, not
// the receiving service's verifier configuration.
type serviceVerifierConfig struct {
	Audience    string                  `koanf:"audience"`
	MaxTTL      time.Duration           `koanf:"max_ttl"`
	ClockSkew   time.Duration           `koanf:"clock_skew"`
	Issuers     []serviceIssuerConfig   `koanf:"issuers"`
	Delegations []serviceDelegationRule `koanf:"delegations"`
}

type serviceIssuerConfig struct {
	Issuer   string             `koanf:"issuer"`
	Subjects []string           `koanf:"subjects"`
	Keys     []serviceKeyConfig `koanf:"keys"`
}

type serviceKeyConfig struct {
	ID        string `koanf:"id"`
	PublicKey string `koanf:"public_key"`
}

type serviceDelegationRule struct {
	Issuer     string `koanf:"issuer"`
	Subject    string `koanf:"subject"`
	TenantID   string `koanf:"tenant_id"`
	MerchantID string `koanf:"merchant_id"`
	Partition  string `koanf:"partition"`
}

func (c serviceVerifierConfig) verifier() (*authn.ServiceVerifier, error) {
	issuers := make(map[string]authn.ServiceIssuer, len(c.Issuers))
	for _, issuer := range c.Issuers {
		if _, exists := issuers[issuer.Issuer]; exists {
			return nil, errors.New("duplicate service issuer")
		}
		keys := make(map[string]ed25519.PublicKey, len(issuer.Keys))
		for _, key := range issuer.Keys {
			if _, exists := keys[key.ID]; exists {
				return nil, errors.New("duplicate service key ID")
			}
			pub, err := servicePublicKey(key.PublicKey)
			if err != nil {
				return nil, err
			}
			keys[key.ID] = pub
		}
		issuers[issuer.Issuer] = authn.ServiceIssuer{Keys: keys, Subjects: issuer.Subjects}
	}
	allowed := make(map[serviceDelegationRule]struct{}, len(c.Delegations))
	for _, rule := range c.Delegations {
		issuer, ok := issuers[rule.Issuer]
		if !ok || !slices.Contains(issuer.Subjects, rule.Subject) {
			return nil, errors.New("delegation rule must name a configured issuer and subject")
		}
		for _, id := range []string{rule.TenantID, rule.MerchantID, rule.Partition} {
			if len(id) > 256 || !utf8.ValidString(id) || strings.IndexFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
				return nil, errors.New("delegation rule contains an invalid scope identifier")
			}
			if strings.ContainsAny(id, "*?[]") {
				return nil, errors.New("delegation rules are exact tuples, not wildcard patterns")
			}
		}
		if rule.TenantID == "" && rule.MerchantID == "" && rule.Partition == "" {
			return nil, errors.New("empty delegation rule is unnecessary; subjects already allow unscoped identity")
		}
		if _, exists := allowed[rule]; exists {
			return nil, errors.New("duplicate delegation rule")
		}
		allowed[rule] = struct{}{}
	}
	policy := func(ctx context.Context, p appkit.ServicePrincipal, scope authn.ServiceDelegation) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if scope == (authn.ServiceDelegation{}) {
			return nil
		}
		if _, ok := allowed[serviceDelegationRule{Issuer: p.Issuer, Subject: p.Subject, TenantID: scope.TenantID, MerchantID: scope.MerchantID, Partition: scope.Partition}]; !ok {
			return errors.New("service delegation is not in the recipient allowlist")
		}
		return nil
	}
	return authn.NewServiceVerifier(authn.ServiceVerifierOptions{
		Audience: c.Audience, Issuers: issuers, MaxTTL: c.MaxTTL, ClockSkew: c.ClockSkew, AuthorizeDelegation: policy,
	})
}

func servicePublicKey(raw string) (ed25519.PublicKey, error) {
	raw = strings.TrimSpace(raw)
	if block, rest := pem.Decode([]byte(raw)); block != nil {
		if !strings.HasPrefix(raw, "-----BEGIN PUBLIC KEY-----") || block.Type != "PUBLIC KEY" || len(strings.TrimSpace(string(rest))) != 0 || len(block.Headers) != 0 {
			return nil, errors.New("service public_key must be one PKIX PUBLIC KEY PEM block")
		}
		key, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, errors.New("service public_key is not valid PKIX")
		}
		pub, ok := key.(ed25519.PublicKey)
		if !ok || len(pub) != ed25519.PublicKeySize {
			return nil, errors.New("service public_key must be Ed25519")
		}
		return pub, nil
	}
	pub, err := base64.StdEncoding.Strict().DecodeString(raw)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("service public_key must be base64 Ed25519 or PKIX PUBLIC KEY PEM")
	}
	return ed25519.PublicKey(pub), nil
}
