package authn

import (
	"context"
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/callctx"
)

const (
	defaultJWKSCacheTTL   = 5 * time.Minute
	defaultJWKSMinRefresh = 15 * time.Second
	defaultJWKSTimeout    = 5 * time.Second
	maxJWKSBodyBytes      = 1 << 20
	maxOIDCClockSkew      = 30 * time.Second
	minRSABits            = 2048
)

var errUnknownOIDCRealm = errors.New("令牌 iss 不是已配置的 Realm")

// OIDCRealm 描述一个受信任的 SSO Realm（如 forgeplex/sso 的单个身份域）。
// 每个 Realm 拥有独立的 Issuer、Audience 与 JWKS（或静态测试公钥），并可映射
// 到组合根内的一个数据分区（Partition）。
type OIDCRealm struct {
	// Issuer 是该 Realm 签发令牌的精确 iss（例如 "https://demo.sso.example.com/"）。
	Issuer string
	// Audience 是目标 Resource Server（API）的精确 aud 标识。
	Audience string
	// Partition 是该 Realm 绑定的可信数据分区键；非空时验签后焊进 callctx.Meta.Partition。
	Partition string
	// JWKSURL 是该 Realm 的 JWKS 公钥集合地址。StaticKeys 为空且未指定时默认取
	// strings.TrimRight(Issuer, "/") + "/.well-known/jwks.json"。
	JWKSURL string
	// StaticKeys 允许不经网络直接按 kid 查公钥（单测、离线验收或钉死公钥场景）。
	// 支持 *rsa.PublicKey（>=2048 位）、*ecdsa.PublicKey（P-256）与 ed25519.PublicKey。
	StaticKeys map[string]crypto.PublicKey
	// RequireOrg 为 true 时要求令牌携带非空 org_id（映射为 Actor.TenantID 与 callctx.TenantID）。
	RequireOrg bool
}

// OIDCVerifierOptions 配置多 Realm 用户令牌验签器。
type OIDCVerifierOptions struct {
	Realms     []OIDCRealm
	HTTPClient *http.Client
	CacheTTL   time.Duration
	MinRefresh time.Duration
	ClockSkew  time.Duration
	// StepUpACRs 列举可视为已通过 MFA step-up 的 acr 取值（除 amr 含 "mfa" 外）。
	StepUpACRs []string
	// now 仅供单测注入时钟。
	now func() time.Time
}

// OIDCVerifier 是不可变的多 Realm 用户访问令牌验签器；零值无效。
type OIDCVerifier struct {
	realms     map[string]*realmVerifier
	httpClient *http.Client
	cacheTTL   time.Duration
	minRefresh time.Duration
	skew       time.Duration
	stepUpACRs []string
	now        func() time.Time
}

type jwkEntry struct {
	key crypto.PublicKey
	alg string // 对应 JWT alg：RS256 / ES256 / EdDSA
}

type realmVerifier struct {
	issuer     string
	audience   string
	partition  string
	jwksURL    string
	requireOrg bool
	staticKeys map[string]jwkEntry

	mu          sync.RWMutex
	cachedKeys  map[string]jwkEntry
	fetchedAt   time.Time
	lastAttempt time.Time
}

type oidcAccessClaims struct {
	jwt.RegisteredClaims
	OrgID       string   `json:"org_id,omitempty"`
	Permissions []string `json:"permissions,omitempty"`
	GrantType   string   `json:"gty,omitempty"`
	Purpose     string   `json:"purpose,omitempty"`
	AMR         []string `json:"amr,omitempty"`
	ACR         string   `json:"acr,omitempty"`
	AuthTime    *int64   `json:"auth_time,omitempty"`
}

// NewOIDCVerifier 校验并构造多 Realm 用户令牌验签器。所有配置切片与公钥映射
// 在构造期快照，避免运行期外部修改。
func NewOIDCVerifier(o OIDCVerifierOptions) (*OIDCVerifier, error) {
	if len(o.Realms) == 0 {
		return nil, errors.New("authn: 至少需要配置一个受信任的 OIDC Realm")
	}
	if o.ClockSkew < 0 || o.ClockSkew > maxOIDCClockSkew {
		return nil, errors.New("authn: OIDC ClockSkew 必须在 0..30s 之间")
	}
	cacheTTL := o.CacheTTL
	if cacheTTL == 0 {
		cacheTTL = defaultJWKSCacheTTL
	}
	if cacheTTL < time.Second {
		return nil, errors.New("authn: OIDC CacheTTL 不能小于 1s")
	}
	minRefresh := o.MinRefresh
	if minRefresh == 0 {
		minRefresh = defaultJWKSMinRefresh
	}
	if minRefresh < 0 {
		return nil, errors.New("authn: OIDC MinRefresh 不能为负")
	}
	httpClient := o.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultJWKSTimeout}
	}
	nowFn := o.now
	if nowFn == nil {
		nowFn = time.Now
	}

	realms := make(map[string]*realmVerifier, len(o.Realms))
	for _, r := range o.Realms {
		iss := strings.TrimSpace(r.Issuer)
		aud := strings.TrimSpace(r.Audience)
		part := strings.TrimSpace(r.Partition)
		if iss == "" || iss != r.Issuer {
			return nil, errors.New("authn: OIDC Realm issuer 不能为空或含首尾空白")
		}
		if aud == "" || aud != r.Audience {
			return nil, fmt.Errorf("authn: OIDC Realm %q audience 不能为空或含首尾空白", iss)
		}
		if part != r.Partition {
			return nil, fmt.Errorf("authn: OIDC Realm %q partition 不能含首尾空白", iss)
		}
		if _, exists := realms[iss]; exists {
			return nil, fmt.Errorf("authn: OIDC Realm issuer %q 重复配置", iss)
		}

		rv := &realmVerifier{
			issuer:     iss,
			audience:   aud,
			partition:  part,
			requireOrg: r.RequireOrg,
		}
		if len(r.StaticKeys) > 0 {
			rv.staticKeys = make(map[string]jwkEntry, len(r.StaticKeys))
			for kid, key := range r.StaticKeys {
				if strings.TrimSpace(kid) == "" {
					return nil, fmt.Errorf("authn: OIDC Realm %q StaticKeys 存在空 kid", iss)
				}
				entry, err := classifyPublicKey(key)
				if err != nil {
					return nil, fmt.Errorf("authn: OIDC Realm %q kid %q: %w", iss, kid, err)
				}
				rv.staticKeys[kid] = entry
			}
		} else {
			jwksURL := strings.TrimSpace(r.JWKSURL)
			if jwksURL == "" {
				jwksURL = strings.TrimRight(iss, "/") + "/.well-known/jwks.json"
			}
			parsed, err := url.Parse(jwksURL)
			if err != nil || parsed.Host == "" || parsed.User != nil {
				return nil, fmt.Errorf("authn: OIDC Realm %q jwks_url %q 无效", iss, jwksURL)
			}
			switch parsed.Scheme {
			case "https":
			case "http":
				if !isLoopbackJWKSHost(parsed.Hostname()) {
					return nil, fmt.Errorf("authn: OIDC Realm %q jwks_url %q 必须使用 https（非 loopback 禁止明文 http）", iss, jwksURL)
				}
			default:
				return nil, fmt.Errorf("authn: OIDC Realm %q jwks_url %q 无效", iss, jwksURL)
			}
			rv.jwksURL = parsed.String()
		}
		realms[iss] = rv
	}

	return &OIDCVerifier{
		realms:     realms,
		httpClient: httpClient,
		cacheTTL:   cacheTTL,
		minRefresh: minRefresh,
		skew:       o.ClockSkew,
		stepUpACRs: slices.Clone(o.StepUpACRs),
		now:        nowFn,
	}, nil
}

// Validate 供组合根与 bootstrap 在打开基础设施前校验验签器有效性。
func (v *OIDCVerifier) Validate() error {
	if v == nil || len(v.realms) == 0 || v.httpClient == nil {
		return errors.New("authn: user verifier must be constructed with NewOIDCVerifier")
	}
	return nil
}

// Realms 返回已配置 Realm 的数量（供组合根启动测试检查）。
func (v *OIDCVerifier) Realms() int {
	if v == nil {
		return 0
	}
	return len(v.realms)
}

// PartitionForIssuer 返回已配置 issuer 绑定的可信分区键。
func (v *OIDCVerifier) PartitionForIssuer(issuer string) (string, bool) {
	if v == nil {
		return "", false
	}
	rv, ok := v.realms[issuer]
	if !ok {
		return "", false
	}
	return rv.partition, true
}

// Middleware 验签 Bearer 访问令牌并注入 appkit.Actor 与 callctx.Meta：
//   - /healthz、/readyz 探针直接放行；
//   - 无 Authorization 头（或非 Bearer）匿名放行，由路由守卫决定是否 401；
//   - 有 Bearer 头但验签失败（非法签名、未知 iss/kid、错 aud、过期、机器令牌、
//     缺 sub、缺必需 org_id、与服务委托冲突）一律返回 401，不降级匿名。
func (v *OIDCVerifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := v.Validate(); err != nil {
			writeInvalid(w, err)
			return
		}
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		values := headerValues(r.Header, "Authorization")
		if len(values) > 1 {
			writeInvalid(w, errors.New("multiple user authorization headers"))
			return
		}
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		actor, partition, err := v.Verify(r.Context(), raw)
		if err != nil {
			writeInvalid(w, err)
			return
		}
		if scope, ok := r.Context().Value(serviceScopeKey{}).(ServiceDelegation); ok &&
			(scope.TenantID != actor.TenantID || scope.Partition != partition) {
			writeInvalid(w, errors.New("user and service identity scopes conflict"))
			return
		}
		meta := callctx.From(r.Context())
		meta.TenantID = actor.TenantID
		if partition != "" {
			meta.Partition = partition
		}
		ctx := callctx.With(appkit.WithActor(r.Context(), actor), meta)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Verify 独立校验一枚 SSO 访问令牌，返回重建后的 appkit.Actor 与可信分区键。
func (v *OIDCVerifier) Verify(ctx context.Context, raw string) (appkit.Actor, string, error) {
	if err := v.Validate(); err != nil {
		return appkit.Actor{}, "", err
	}
	if strings.TrimSpace(raw) == "" {
		return appkit.Actor{}, "", errors.New("empty bearer token")
	}
	var claims oidcAccessClaims
	var matched *realmVerifier
	parseOpts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{
			jwt.SigningMethodRS256.Alg(),
			jwt.SigningMethodES256.Alg(),
			jwt.SigningMethodEdDSA.Alg(),
		}),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(v.now),
	}
	if v.skew > 0 {
		parseOpts = append(parseOpts, jwt.WithLeeway(v.skew))
	}

	_, err := jwt.ParseWithClaims(raw, &claims, func(tok *jwt.Token) (any, error) {
		if typ, _ := tok.Header["typ"].(string); typ != "" {
			if typ == serviceTokenType {
				return nil, errors.New("service JWT cannot authenticate a user")
			}
			if !strings.EqualFold(typ, "at+jwt") && !strings.EqualFold(typ, "JWT") {
				return nil, fmt.Errorf("unsupported user token typ %q", typ)
			}
		}
		iss, err := tok.Claims.GetIssuer()
		if err != nil {
			return nil, err
		}
		rv, ok := v.realms[iss]
		if !ok {
			return nil, fmt.Errorf("%w: %q", errUnknownOIDCRealm, iss)
		}
		matched = rv

		kid, _ := tok.Header["kid"].(string)
		alg, _ := tok.Header["alg"].(string)
		entry, err := v.resolveKey(ctx, rv, kid)
		if err != nil {
			return nil, err
		}
		if entry.alg != alg {
			return nil, fmt.Errorf("key algorithm %s does not match token alg %s", entry.alg, alg)
		}
		return entry.key, nil
	}, parseOpts...)
	if err != nil {
		return appkit.Actor{}, "", err
	}
	if matched == nil {
		return appkit.Actor{}, "", errUnknownOIDCRealm
	}
	if !slices.Contains(claims.Audience, matched.audience) {
		return appkit.Actor{}, "", fmt.Errorf("token audience does not include %q", matched.audience)
	}
	if claims.Subject == "" {
		return appkit.Actor{}, "", errEmptySubject
	}
	if claims.Purpose != "" && claims.Purpose != "access" {
		return appkit.Actor{}, "", errors.New("wrong user access token purpose")
	}
	if isMachineGrant(claims.GrantType) {
		return appkit.Actor{}, "", errors.New("machine client_credentials token cannot authenticate a user")
	}
	if matched.requireOrg && claims.OrgID == "" {
		return appkit.Actor{}, "", errors.New("token missing required org_id claim")
	}

	actor := appkit.Actor{
		UserID:   claims.Subject,
		TenantID: claims.OrgID,
		Perms:    slices.Clone(claims.Permissions),
	}
	now := v.now()
	if claims.AuthTime != nil {
		if *claims.AuthTime <= 0 {
			return appkit.Actor{}, "", errors.New("token auth_time must be positive")
		}
		authTime := time.Unix(*claims.AuthTime, 0)
		if authTime.After(now.Add(v.skew)) {
			return appkit.Actor{}, "", errors.New("token auth_time cannot be in the future")
		}
		if v.isStepUp(claims.AMR, claims.ACR) {
			if authTime.After(now) {
				authTime = now
			}
			actor.StepUpAt = authTime
		}
	}
	return actor, matched.partition, nil
}

func isLoopbackJWKSHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	host, _, _ = strings.Cut(host, "%")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isMachineGrant(gty string) bool {
	return gty == "client-credentials" || gty == "client_credentials"
}

func (v *OIDCVerifier) isStepUp(amr []string, acr string) bool {
	if slices.Contains(amr, "mfa") {
		return true
	}
	return acr != "" && slices.Contains(v.stepUpACRs, acr)
}

func (v *OIDCVerifier) resolveKey(ctx context.Context, rv *realmVerifier, kid string) (jwkEntry, error) {
	if len(rv.staticKeys) > 0 {
		if kid == "" {
			if len(rv.staticKeys) == 1 {
				for _, entry := range rv.staticKeys {
					return entry, nil
				}
			}
			return jwkEntry{}, errors.New("missing kid header")
		}
		entry, ok := rv.staticKeys[kid]
		if !ok {
			return jwkEntry{}, fmt.Errorf("unknown kid %q", kid)
		}
		return entry, nil
	}
	if kid == "" {
		return jwkEntry{}, errors.New("missing kid header")
	}

	now := v.now()
	rv.mu.RLock()
	entry, ok := rv.cachedKeys[kid]
	fresh := !rv.fetchedAt.IsZero() && now.Sub(rv.fetchedAt) < v.cacheTTL
	rv.mu.RUnlock()
	if ok && fresh {
		return entry, nil
	}

	if err := v.refreshJWKS(ctx, rv, !ok); err != nil {
		// 若缓存仍有该 kid，网络抖动时不立刻中断已验证密钥（直到下次刷新成功）。
		if ok {
			return entry, nil
		}
		return jwkEntry{}, err
	}

	rv.mu.RLock()
	entry, ok = rv.cachedKeys[kid]
	rv.mu.RUnlock()
	if !ok {
		return jwkEntry{}, fmt.Errorf("unknown kid %q after JWKS refresh", kid)
	}
	return entry, nil
}

func (v *OIDCVerifier) refreshJWKS(ctx context.Context, rv *realmVerifier, missingKey bool) error {
	rv.mu.Lock()
	defer rv.mu.Unlock()

	now := v.now()
	if !missingKey && !rv.fetchedAt.IsZero() && now.Sub(rv.fetchedAt) < v.cacheTTL {
		return nil
	}
	if !rv.lastAttempt.IsZero() && now.Sub(rv.lastAttempt) < v.minRefresh {
		return errors.New("JWKS refresh throttled")
	}
	rv.lastAttempt = now

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rv.jwksURL, nil)
	if err != nil {
		return fmt.Errorf("build JWKS request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch JWKS %s: %w", rv.jwksURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch JWKS %s: status %d", rv.jwksURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBodyBytes+1))
	if err != nil {
		return fmt.Errorf("read JWKS %s: %w", rv.jwksURL, err)
	}
	if len(body) > maxJWKSBodyBytes {
		return fmt.Errorf("JWKS response from %s exceeds %d bytes", rv.jwksURL, maxJWKSBodyBytes)
	}
	keys, err := parseJWKS(body)
	if err != nil {
		return fmt.Errorf("parse JWKS %s: %w", rv.jwksURL, err)
	}
	if len(keys) == 0 {
		return fmt.Errorf("JWKS %s contains no supported signing keys", rv.jwksURL)
	}
	rv.cachedKeys = keys
	rv.fetchedAt = v.now()
	return nil
}

type jwksDocument struct {
	Keys []jwkRaw `json:"keys"`
}

type jwkRaw struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func parseJWKS(data []byte) (map[string]jwkEntry, error) {
	var doc jwksDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	out := make(map[string]jwkEntry, len(doc.Keys))
	for _, raw := range doc.Keys {
		if raw.Kid == "" || (raw.Use != "" && raw.Use != "sig") {
			continue
		}
		entry, err := parseJWK(raw)
		if err != nil {
			continue
		}
		if raw.Alg != "" && raw.Alg != entry.alg {
			continue
		}
		out[raw.Kid] = entry
	}
	return out, nil
}

func parseJWK(raw jwkRaw) (jwkEntry, error) {
	switch raw.Kty {
	case "RSA":
		nBytes, err := base64.RawURLEncoding.DecodeString(raw.N)
		if err != nil || len(nBytes) == 0 {
			return jwkEntry{}, errors.New("invalid RSA n")
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(raw.E)
		if err != nil || len(eBytes) == 0 || len(eBytes) > 4 {
			return jwkEntry{}, errors.New("invalid RSA e")
		}
		n := new(big.Int).SetBytes(nBytes)
		var e int
		for _, b := range eBytes {
			e = (e << 8) | int(b)
		}
		pub := &rsa.PublicKey{N: n, E: e}
		return classifyPublicKey(pub)
	case "EC":
		if raw.Crv != "P-256" {
			return jwkEntry{}, errors.New("unsupported EC curve")
		}
		xBytes, err := base64.RawURLEncoding.DecodeString(raw.X)
		if err != nil || len(xBytes) == 0 || len(xBytes) > 32 {
			return jwkEntry{}, errors.New("invalid EC x")
		}
		yBytes, err := base64.RawURLEncoding.DecodeString(raw.Y)
		if err != nil || len(yBytes) == 0 || len(yBytes) > 32 {
			return jwkEntry{}, errors.New("invalid EC y")
		}
		uncompressed := make([]byte, 65)
		uncompressed[0] = 0x04
		copy(uncompressed[1+(32-len(xBytes)):33], xBytes)
		copy(uncompressed[33+(32-len(yBytes)):65], yBytes)
		if _, err := ecdh.P256().NewPublicKey(uncompressed); err != nil {
			return jwkEntry{}, errors.New("invalid P-256 point")
		}
		pub := &ecdsa.PublicKey{
			Curve: elliptic.P256(),
			X:     new(big.Int).SetBytes(xBytes),
			Y:     new(big.Int).SetBytes(yBytes),
		}
		return classifyPublicKey(pub)
	case "OKP":
		if raw.Crv != "Ed25519" {
			return jwkEntry{}, errors.New("unsupported OKP curve")
		}
		xBytes, err := base64.RawURLEncoding.DecodeString(raw.X)
		if err != nil || len(xBytes) != ed25519.PublicKeySize {
			return jwkEntry{}, errors.New("invalid Ed25519 x")
		}
		return classifyPublicKey(ed25519.PublicKey(xBytes))
	default:
		return jwkEntry{}, errors.New("unsupported kty")
	}
}

func classifyPublicKey(key crypto.PublicKey) (jwkEntry, error) {
	switch k := key.(type) {
	case *rsa.PublicKey:
		if k == nil || k.N == nil || k.N.BitLen() < minRSABits || k.E < 3 {
			return jwkEntry{}, fmt.Errorf("RSA 公钥无效或位数不足 %d", minRSABits)
		}
		return jwkEntry{key: k, alg: jwt.SigningMethodRS256.Alg()}, nil
	case *ecdsa.PublicKey:
		if k == nil || k.Curve != elliptic.P256() || k.X == nil || k.Y == nil {
			return jwkEntry{}, errors.New("仅支持 P-256 ECDSA 公钥")
		}
		if _, err := k.ECDH(); err != nil {
			return jwkEntry{}, fmt.Errorf("ECDSA P-256 公钥点无效: %w", err)
		}
		return jwkEntry{key: k, alg: jwt.SigningMethodES256.Alg()}, nil
	case ed25519.PublicKey:
		if len(k) != ed25519.PublicKeySize {
			return jwkEntry{}, errors.New("Ed25519 公钥长度无效")
		}
		return jwkEntry{key: slices.Clone(k), alg: jwt.SigningMethodEdDSA.Alg()}, nil
	default:
		return jwkEntry{}, errors.New("不支持的公钥类型（仅支持 RSA/ECDSA-P256/Ed25519）")
	}
}
