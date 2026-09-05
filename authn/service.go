package authn

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/callctx"
)

// HeaderServiceAuthorization carries a Bearer service JWT, independently of the
// user's Authorization header. It must never contain a forwarded user token.
const HeaderServiceAuthorization = "X-Service-Authorization"

const (
	serviceTokenType = "appkit-service+jwt"
	servicePurpose   = "service"
	maxServiceTTL    = 5 * time.Minute
)

// ServiceDelegation is the explicitly authorized data scope of one service call.
// Empty fields do not mean all tenants/merchants/partitions. MerchantID stays in
// ServicePrincipal; it is deliberately not added to callctx's propagation list.
type ServiceDelegation struct {
	TenantID   string
	MerchantID string
	Partition  string
}

// ServiceDelegationPolicy authorizes the complete requested scope after signature
// verification (or before signing). A nil policy denies any nonempty delegation.
// The callback must be concurrency-safe and honor ctx cancellation. It receives a
// detached principal snapshot; changing it cannot change the verified identity.
type ServiceDelegationPolicy func(context.Context, appkit.ServicePrincipal, ServiceDelegation) error

// ServiceIssuer is a static trust entry. Keys are selected by the exact iss+kid
// pair; Subjects is a nonempty allowlist. Neither token-provided URLs nor dynamic
// key lookup are supported. Keep old and new kids during a deliberate rotation.
type ServiceIssuer struct {
	Keys     map[string]ed25519.PublicKey
	Subjects []string
}

// ServiceVerifierOptions configures one recipient. Audience must be explicit.
// MaxTTL defaults to five minutes and may only reduce that limit. ClockSkew is
// optional, capped at 30 seconds, and never increases the allowed exp-iat span.
type ServiceVerifierOptions struct {
	Audience            string
	Issuers             map[string]ServiceIssuer
	MaxTTL              time.Duration
	ClockSkew           time.Duration
	AuthorizeDelegation ServiceDelegationPolicy
}

// ServiceVerifier is immutable after construction; its zero value is invalid.
type ServiceVerifier struct {
	audience string
	issuers  map[string]ServiceIssuer
	maxTTL   time.Duration
	skew     time.Duration
	policy   ServiceDelegationPolicy
}

// NewServiceVerifier validates configuration without panicking and snapshots all
// maps, slices, and key bytes. Subsequent caller changes do not rotate live keys.
func NewServiceVerifier(o ServiceVerifierOptions) (*ServiceVerifier, error) {
	if o.MaxTTL == 0 {
		o.MaxTTL = maxServiceTTL
	}
	if !serviceIdentifier(o.Audience) || len(o.Issuers) == 0 {
		return nil, errors.New("authn: service audience and trusted issuers are required")
	}
	if o.MaxTTL < time.Second || o.MaxTTL > maxServiceTTL || o.ClockSkew < 0 || o.ClockSkew > 30*time.Second {
		return nil, errors.New("authn: service MaxTTL must be 1s..5m and ClockSkew 0..30s")
	}
	v := &ServiceVerifier{audience: o.Audience, issuers: make(map[string]ServiceIssuer, len(o.Issuers)), maxTTL: o.MaxTTL, skew: o.ClockSkew, policy: o.AuthorizeDelegation}
	for name, is := range o.Issuers {
		if !serviceIdentifier(name) || len(is.Keys) == 0 || len(is.Subjects) == 0 {
			return nil, errors.New("authn: every service issuer requires a valid name, keys and subjects")
		}
		copyIssuer := ServiceIssuer{Keys: make(map[string]ed25519.PublicKey, len(is.Keys)), Subjects: slices.Clone(is.Subjects)}
		seen := make(map[string]bool, len(is.Subjects))
		for _, sub := range is.Subjects {
			if !serviceIdentifier(sub) || seen[sub] {
				return nil, errors.New("authn: service subjects must be valid and unique")
			}
			seen[sub] = true
		}
		for kid, key := range is.Keys {
			if !serviceIdentifier(kid) || len(key) != ed25519.PublicKeySize {
				return nil, errors.New("authn: service key requires a valid kid and Ed25519 public key")
			}
			copyIssuer.Keys[kid] = slices.Clone(key)
		}
		v.issuers[name] = copyIssuer
	}
	return v, nil
}

// Validate permits composition roots to fail before migrations, Setup or listen.
func (v *ServiceVerifier) Validate() error {
	if v == nil || v.audience == "" || len(v.issuers) == 0 || v.maxTTL < time.Second {
		return errors.New("authn: service verifier must be constructed with NewServiceVerifier")
	}
	return nil
}

type serviceClaims struct {
	jwt.RegisteredClaims
	Purpose    string `json:"purpose"`
	TenantID   string `json:"tid,omitempty"`
	MerchantID string `json:"mid,omitempty"`
	Partition  string `json:"partition,omitempty"`
}

type serviceScopeKey struct{}

// Middleware verifies a present service credential, then injects ServicePrincipal
// and callctx.Caller from signed sub. Missing credentials remain anonymous; route
// guards decide whether authentication is required. Invalid credentials fail even
// on public application routes, except the built-in health/readiness probes.
// In mixed mode install user authentication first. Both credentials must agree on
// the exact tenant and partition, including empty scope. No user Actor is created.
func (v *ServiceVerifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := v.Validate(); err != nil {
			writeInvalid(w, err)
			return
		}
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		values := headerValues(r.Header, HeaderServiceAuthorization)
		if len(values) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
			writeInvalid(w, errors.New("invalid service authorization header"))
			return
		}
		principal, scope, err := v.verify(r.Context(), strings.TrimPrefix(values[0], "Bearer "))
		if err != nil {
			writeInvalid(w, err)
			return
		}
		if err := checkServiceHeaders(r.Header, principal, scope); err != nil {
			writeInvalid(w, err)
			return
		}
		if err := checkServiceHeaders(appkit.UntrustedIdentityHeadersFrom(r.Context()), principal, scope); err != nil {
			writeInvalid(w, err)
			return
		}
		meta := callctx.From(r.Context())
		if actor, ok := appkit.ActorFrom(r.Context()); ok && (actor.TenantID != scope.TenantID || meta.Partition != scope.Partition) {
			writeInvalid(w, errors.New("user and service identity scopes conflict"))
			return
		}
		meta.Caller, meta.TenantID, meta.Partition = principal.Subject, scope.TenantID, scope.Partition
		ctx := callctx.With(appkit.WithServicePrincipal(r.Context(), principal), meta)
		ctx = context.WithValue(ctx, serviceScopeKey{}, scope)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (v *ServiceVerifier) verify(ctx context.Context, raw string) (appkit.ServicePrincipal, ServiceDelegation, error) {
	var zero appkit.ServicePrincipal
	var noScope ServiceDelegation
	if err := ctx.Err(); err != nil {
		return zero, noScope, err
	}
	if err := checkServiceEncoding(raw); err != nil {
		return zero, noScope, err
	}
	var claims serviceClaims
	var kid string
	_, err := jwt.ParseWithClaims(raw, &claims, func(tok *jwt.Token) (any, error) {
		if tok.Header["typ"] != serviceTokenType {
			return nil, errors.New("wrong service token type")
		}
		kid, _ = tok.Header["kid"].(string)
		is, ok := v.issuers[claims.Issuer]
		if !ok || kid == "" || is.Keys[kid] == nil {
			return nil, errors.New("unknown service issuer or key")
		}
		return is.Keys[kid], nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithAudience(v.audience), jwt.WithLeeway(v.skew))
	if err != nil {
		return zero, noScope, err
	}
	if claims.Purpose != servicePurpose || !serviceIdentifier(claims.Subject) || !slices.Contains(v.issuers[claims.Issuer].Subjects, claims.Subject) {
		return zero, noScope, errors.New("invalid service purpose or subject")
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != v.audience || claims.IssuedAt == nil || claims.ExpiresAt == nil {
		return zero, noScope, errors.New("service token requires one exact audience, iat and exp")
	}
	span := claims.ExpiresAt.Sub(claims.IssuedAt.Time)
	if span <= 0 || span > v.maxTTL {
		return zero, noScope, errors.New("service token lifetime exceeds policy")
	}
	scope := ServiceDelegation{TenantID: claims.TenantID, MerchantID: claims.MerchantID, Partition: claims.Partition}
	p := appkit.ServicePrincipal{Subject: claims.Subject, Issuer: claims.Issuer, Audience: slices.Clone(claims.Audience), KeyID: kid, IssuedAt: claims.IssuedAt.Time, ExpiresAt: claims.ExpiresAt.Time, TenantID: scope.TenantID, MerchantID: scope.MerchantID}
	if err := authorizeServiceScope(ctx, v.policy, p, scope); err != nil {
		return zero, noScope, err
	}
	return p, scope, nil
}

func authorizeServiceScope(ctx context.Context, policy ServiceDelegationPolicy, p appkit.ServicePrincipal, scope ServiceDelegation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, id := range []string{scope.TenantID, scope.MerchantID, scope.Partition} {
		if id != "" && !serviceIdentifier(id) {
			return errors.New("invalid service delegation scope")
		}
	}
	if scope != (ServiceDelegation{}) && policy == nil {
		return errors.New("service delegation requires an explicit authorization policy")
	}
	if policy != nil {
		p.Audience = slices.Clone(p.Audience)
		if err := policy(ctx, p, scope); err != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			// Policy errors may contain credentials or private policy details.
			return errors.New("service delegation denied")
		}
	}
	return ctx.Err()
}

func checkServiceHeaders(h http.Header, p appkit.ServicePrincipal, scope ServiceDelegation) error {
	for name, want := range map[string]string{callctx.HeaderCaller: p.Subject, callctx.HeaderTenantID: scope.TenantID, callctx.HeaderPartition: scope.Partition, "X-Merchant-Id": scope.MerchantID} {
		values := headerValues(h, name)
		if len(values) > 1 || (len(values) == 1 && values[0] != want) {
			return errors.New("unsigned identity header conflicts with service credential")
		}
	}
	return nil
}

func headerValues(h http.Header, name string) []string {
	var values []string
	for key, val := range h {
		if strings.EqualFold(key, name) {
			values = append(values, val...)
		}
	}
	return values
}

func serviceIdentifier(s string) bool {
	return s != "" && len(s) <= 256 && utf8.ValidString(s) && strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

// checkServiceEncoding rejects ambiguous JSON, extension headers (including
// jku/jwk/x5u/crit), and unknown claims before parsing. There is no network path.
func checkServiceEncoding(raw string) error {
	if len(raw) == 0 || len(raw) > 16*1024 {
		return errors.New("invalid service token size")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return errors.New("invalid service JWT encoding")
	}
	for i, allowed := range []string{"alg typ kid", "iss sub aud exp iat nbf jti purpose tid mid partition"} {
		data, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil || !utf8.Valid(data) {
			return errors.New("invalid service JWT JSON encoding")
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		tok, err := dec.Token()
		if err != nil || tok != json.Delim('{') {
			return errors.New("service JWT must contain JSON objects")
		}
		seen := make(map[string]bool)
		for dec.More() {
			key, err := dec.Token()
			name, ok := key.(string)
			if err != nil || !ok || seen[name] || !slices.Contains(strings.Fields(allowed), name) {
				return errors.New("duplicate or unsupported service JWT member")
			}
			seen[name] = true
			var value json.RawMessage
			if err := dec.Decode(&value); err != nil {
				return errors.New("invalid service JWT member")
			}
		}
		if _, err := dec.Token(); err != nil {
			return errors.New("invalid service JWT object")
		}
		if _, err := dec.Token(); err != io.EOF {
			return errors.New("trailing service JWT data")
		}
	}
	return nil
}

// ServiceSignerOptions describes a fixed service identity. The constructor copies
// Key. TTL defaults to one minute and must be an integral second in 1s..5m.
type ServiceSignerOptions struct {
	Issuer              string
	Subject             string
	KeyID               string
	Key                 ed25519.PrivateKey
	TTL                 time.Duration
	AuthorizeDelegation ServiceDelegationPolicy
}

// ServiceSigner signs only explicitly supplied audience and delegation. It does
// not infer authorization from an Actor, incoming credential, header or callctx.
type ServiceSigner struct {
	issuer, subject, kid string
	key                  ed25519.PrivateKey
	ttl                  time.Duration
	policy               ServiceDelegationPolicy
}

// NewServiceSigner validates the fixed identity, lifetime and key without
// panicking. Callers retain ownership of their private-key buffer.
func NewServiceSigner(o ServiceSignerOptions) (*ServiceSigner, error) {
	if o.TTL == 0 {
		o.TTL = time.Minute
	}
	if !serviceIdentifier(o.Issuer) || !serviceIdentifier(o.Subject) || !serviceIdentifier(o.KeyID) || len(o.Key) != ed25519.PrivateKeySize {
		return nil, errors.New("authn: service signer requires issuer, subject, kid and Ed25519 private key")
	}
	if o.TTL < time.Second || o.TTL > maxServiceTTL || o.TTL%time.Second != 0 {
		return nil, errors.New("authn: service signer TTL must be integral seconds in 1s..5m")
	}
	if !bytes.Equal(ed25519.NewKeyFromSeed(o.Key.Seed()), o.Key) {
		return nil, errors.New("authn: malformed Ed25519 private key")
	}
	return &ServiceSigner{issuer: o.Issuer, subject: o.Subject, kid: o.KeyID, key: slices.Clone(o.Key), ttl: o.TTL, policy: o.AuthorizeDelegation}, nil
}

// Sign returns a compact service JWT. Use SignWithExpiry when a transport must
// check freshness without interpreting the token or caching past its expiry.
func (s *ServiceSigner) Sign(ctx context.Context, audience string, scope ServiceDelegation) (string, error) {
	raw, _, err := s.SignWithExpiry(ctx, audience, scope)
	return raw, err
}

// SignWithExpiry returns the exact signed expiry, not a separately sampled clock.
func (s *ServiceSigner) SignWithExpiry(ctx context.Context, audience string, scope ServiceDelegation) (string, time.Time, error) {
	if s == nil || len(s.key) != ed25519.PrivateKeySize {
		return "", time.Time{}, errors.New("authn: service signer must be constructed with NewServiceSigner")
	}
	if !serviceIdentifier(audience) {
		return "", time.Time{}, errors.New("authn: service token audience is required")
	}
	now := time.Now().Truncate(time.Second)
	exp := now.Add(s.ttl)
	p := appkit.ServicePrincipal{Issuer: s.issuer, Subject: s.subject, KeyID: s.kid, Audience: []string{audience}, IssuedAt: now, ExpiresAt: exp, TenantID: scope.TenantID, MerchantID: scope.MerchantID}
	if err := authorizeServiceScope(ctx, s.policy, p, scope); err != nil {
		return "", time.Time{}, err
	}
	claims := serviceClaims{RegisteredClaims: jwt.RegisteredClaims{Issuer: s.issuer, Subject: s.subject, Audience: jwt.ClaimStrings{audience}, IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(exp)}, Purpose: servicePurpose, TenantID: scope.TenantID, MerchantID: scope.MerchantID, Partition: scope.Partition}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["typ"], tok.Header["kid"] = serviceTokenType, s.kid
	raw, err := tok.SignedString(s.key)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("authn: sign service token: %w", err)
	}
	return raw, exp, nil
}
