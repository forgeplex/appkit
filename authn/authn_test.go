package authn

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/callctx"
)

const testIssuer = "rbac-test"

// env 持有一对 Ed25519 密钥与已挂中间件的探针 handler。
type env struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	h    http.Handler
	// actor 捕获中间件注入的 Actor 与 callctx（探针 handler 读 ctx 转存）。
	actor  appkit.Actor
	hasCtx bool
	meta   callctx.Meta
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{pub: pub, priv: priv}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.actor, e.hasCtx = appkit.ActorFrom(r.Context())
		e.meta = callctx.From(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	e.h = Middleware(pub, testIssuer)(next)
	return e
}

// sign 用测试密钥签一枚 EdDSA JWT。
func sign(t *testing.T, priv ed25519.PrivateKey, claims jwt.Claims) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(priv)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func now() time.Time { return time.Now().Add(-time.Minute) } // 留时钟余量

func accessTok(sub string, perms []string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":   testIssuer,
		"sub":   sub,
		"tid":   "tenant-1",
		"perms": perms,
		"iat":   now().Unix(),
		"exp":   time.Now().Add(time.Hour).Unix(),
	}
}

func stepUpTok(sub string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":     testIssuer,
		"sub":     sub,
		"purpose": "step-up",
		"iat":     now().Unix(),
		"exp":     time.Now().Add(time.Hour).Unix(),
	}
}

// serve 发起请求，返回 (状态码, 规范化错误码)。e.actor 转存注入结果。
func (e *env) serve(t *testing.T, bearer string, stepUp string) (int, string) {
	t.Helper()
	e.actor, e.hasCtx, e.meta = appkit.Actor{}, false, callctx.Meta{}
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if stepUp != "" {
		req.Header.Set(HeaderStepUp, stepUp)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	code := ""
	if rec.Body.Len() > 0 {
		code = apperr.FromProblem(rec.Code, rec.Body.Bytes()).Code()
	}
	return rec.Code, code
}

func TestMiddlewareHappyPath(t *testing.T) {
	e := newEnv(t)
	tok := sign(t, e.priv, accessTok("u1", []string{"files:read", "files:write"}))
	s, c := e.serve(t, tok, "")
	if s != 200 || c != "" {
		t.Fatalf("合法令牌应放行，实际 %d/%s", s, c)
	}
	if !e.hasCtx || e.actor.UserID != "u1" || e.actor.TenantID != "tenant-1" ||
		len(e.actor.Perms) != 2 || e.actor.Perms[0] != "files:read" {
		t.Fatalf("Actor 注入不符: %+v (hasCtx=%v)", e.actor, e.hasCtx)
	}
	if e.meta.TenantID != "tenant-1" {
		t.Fatalf("tid 应同时焊进 callctx，实际 %q", e.meta.TenantID)
	}
}

// TestMiddlewareTenantWeld 锁住租户信任模型：认证请求以令牌为准——
// 有 tid 覆盖入站头带来的值，无 tid 清零（防「无租户令牌 + 伪造的
// X-Tenant-Id」）；未认证请求不动 callctx（内部东西向靠头传播）。
func TestMiddlewareTenantWeld(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	var meta callctx.Meta
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meta = callctx.From(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	// 模拟真实链：httpserver.RequestID 中间件先从头 Extract 进 ctx，
	// authn 在内层覆盖/清零。
	inner := Middleware(pub, testIssuer)(next)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r.WithContext(callctx.Merge(r.Context(), callctx.Extract(r.Header.Get))))
	})
	serve := func(bearer, tenantHeader string) callctx.Meta {
		meta = callctx.Meta{}
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if tenantHeader != "" {
			req.Header.Set(callctx.HeaderTenantID, tenantHeader)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		return meta
	}
	noTid := jwt.MapClaims{
		"iss": testIssuer, "sub": "u1", "exp": time.Now().Add(time.Hour).Unix(),
	}

	if m := serve(sign(t, priv, accessTok("u1", nil)), "forged-t"); m.TenantID != "tenant-1" {
		t.Fatalf("有 tid 应覆盖伪造头值，实际 %q", m.TenantID)
	}
	if m := serve(sign(t, priv, noTid), "forged-t"); m.TenantID != "" {
		t.Fatalf("无 tid 应清零头带来的租户，实际 %q", m.TenantID)
	}
	if m := serve("", "internal-t"); m.TenantID != "internal-t" {
		t.Fatalf("未认证请求头值应存活（东西向形态），实际 %q", m.TenantID)
	}
	if m := serve("", ""); m.TenantID != "" {
		t.Fatalf("无凭证无头应无租户，实际 %q", m.TenantID)
	}
}

func TestMiddlewareStepUp(t *testing.T) {
	e := newEnv(t)
	tok := sign(t, e.priv, accessTok("u1", []string{"roles:delete"}))
	su := sign(t, e.priv, stepUpTok("u1"))
	if s, _ := e.serve(t, tok, su); s != 200 {
		t.Fatalf("合法 step-up 应放行，实际 %d", s)
	}
	if e.actor.StepUpAt.IsZero() {
		t.Fatal("StepUpAt 应取证明的 iat")
	}
	if want := now().Unix(); e.actor.StepUpAt.Unix() != want {
		t.Fatalf("StepUpAt 应等于 iat=%d，实际 %d", want, e.actor.StepUpAt.Unix())
	}

	// sub 不一致：别人的证明不能替我过关。
	foreign := sign(t, e.priv, stepUpTok("u2"))
	if s, c := e.serve(t, tok, foreign); s != 401 || c != apperr.CodeUnauthenticated {
		t.Fatalf("sub 不一致的证明应 401，实际 %d/%s", s, c)
	}

	// purpose 不是 step-up：访问令牌冒充证明。
	impersonate := sign(t, e.priv, accessTok("u1", nil))
	if s, c := e.serve(t, tok, impersonate); s != 401 || c != apperr.CodeUnauthenticated {
		t.Fatalf("purpose 不符应 401，实际 %d/%s", s, c)
	}
}

func TestMiddlewareNoCredentialPassesThrough(t *testing.T) {
	e := newEnv(t)
	if s, _ := e.serve(t, "", ""); s != 200 {
		t.Fatalf("无凭证应放行（判公开与否是 Require 的职责），实际 %d", s)
	}
	if e.hasCtx {
		t.Fatal("无凭证不应注入 Actor")
	}
}

func TestMiddlewareRejectsInvalid(t *testing.T) {
	e := newEnv(t)

	// 坏签名：另一把私钥签的令牌。
	_, otherPriv, _ := ed25519.GenerateKey(nil)
	forged := sign(t, otherPriv, accessTok("u1", []string{"files:read"}))
	if s, c := e.serve(t, forged, ""); s != 401 || c != apperr.CodeUnauthenticated {
		t.Fatalf("坏签名应 401，实际 %d/%s", s, c)
	}

	// 错 iss。
	wrongIss := jwt.MapClaims{"iss": "rbac-other", "sub": "u1", "exp": time.Now().Add(time.Hour).Unix()}
	if s, _ := e.serve(t, sign(t, e.priv, wrongIss), ""); s != 401 {
		t.Fatalf("错 iss 应 401，实际 %d", s)
	}

	// 过期。
	expired := jwt.MapClaims{"iss": testIssuer, "sub": "u1", "exp": time.Now().Add(-time.Hour).Unix()}
	if s, _ := e.serve(t, sign(t, e.priv, expired), ""); s != 401 {
		t.Fatalf("过期应 401，实际 %d", s)
	}

	// 缺 exp：不设过期时间的令牌必须拒收。
	noExp := jwt.MapClaims{"iss": testIssuer, "sub": "u1"}
	if s, _ := e.serve(t, sign(t, e.priv, noExp), ""); s != 401 {
		t.Fatalf("缺 exp 应 401，实际 %d", s)
	}

	// sub 为空。
	noSub := jwt.MapClaims{"iss": testIssuer, "exp": time.Now().Add(time.Hour).Unix()}
	if s, _ := e.serve(t, sign(t, e.priv, noSub), ""); s != 401 {
		t.Fatalf("空 sub 应 401，实际 %d", s)
	}

	// 算法混淆（经典攻击）：把公钥字节当 HMAC 密钥签 HS256 令牌——
	// 若验签面不看算法、拿公钥当 HMAC secret 验，攻击即成立。
	// WithValidMethods 白名单必须在解析阶段就拒绝。
	confused, err := jwt.NewWithClaims(jwt.SigningMethodHS256, accessTok("u1", nil)).
		SignedString([]byte(e.pub))
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := e.serve(t, confused, ""); s != 401 {
		t.Fatalf("HS256 混淆令牌应 401，实际 %d", s)
	}

	// 非Bearer 形态（如 Basic）视同无凭证放行。
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 200 || e.hasCtx {
		t.Fatalf("非 Bearer 凭证应视同匿名放行，实际 %d", rec.Code)
	}
}
