// Package authn 是访问令牌的验签机制件：解析 Bearer 访问令牌与 X-Step-Up
// step-up 证明，验签后注入 appkit.Actor。判定（Require/Check）在框架根包，
// 本包只负责「凭证 → Actor」这一段——验签只写一次，模块经 ctx 只读。
//
// claims 布局是框架与鉴权提供方之间的契约（提供方按此签发，换提供方即换
// 一个按此布局签发的实现，模块与框架零改动）：
//
//	访问令牌（Bearer）：iss（须是已配置的签发方）、sub=用户、exp 必填、
//	                   tid=业务租户（可选）、perms=精确权限码快照。
//	step-up（X-Step-Up）：iss（须与访问令牌同一签发方）、sub（须与访问令牌
//	                   一致）、exp 必填、purpose="step-up"、iat=新鲜度判定依据。
//
// 提供方验「挑战动作」（MFA/TOTP 等）并签发 step-up 令牌；本包验「挑战
// 证明」（签名、iss、sub 一致、purpose、exp）。
//
// 挂载是组合根的部署面决策，通常一行：
//
//	appkit.Middleware(authn.Middleware(pub, "rbac-demo"))
//
// 多分区同进程（每个 rbac 分区一个 iss、可各配一把密钥）用 MultiIssuer，
// 并顺手把签发方所属的分区键焊进 callctx：
//
//	appkit.Middleware(authn.MultiIssuer(map[string]authn.Issuer{
//	    "rbac-a": {Key: pubA, Partition: "a"},
//	    "rbac-b": {Key: pubB, Partition: "b"},
//	}))
package authn

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/callctx"
)

// HeaderStepUp 携带 step-up 证明（另一枚 JWT）。凭证不跨边界传播——每个
// 服务用自己的 Authn 重新验证——因此不进 callctx 白名单。
const HeaderStepUp = "X-Step-Up"

// stepUpPurpose 是 step-up 证明的用途标记，防止普通访问令牌被冒充。
const stepUpPurpose = "step-up"

// 证明畸形的判定性错误（进 401 的 cause，只用于日志归因）。
var (
	errEmptySubject   = errors.New("令牌 sub 为空")
	errWrongPurpose   = errors.New("step-up 证明的 purpose 不是 step-up")
	errNoIssuedAt     = errors.New("step-up 证明缺少 iat")
	errUnknownIssuer  = errors.New("令牌 iss 不是已配置的签发方")
	errIssuerMismatch = errors.New("step-up 证明与访问令牌不是同一签发方")
)

// Issuer 是一个签发方的验签规格。
type Issuer struct {
	// Key 是该签发方的 EdDSA 验签公钥。
	Key ed25519.PublicKey
	// Partition 是该签发方所属的分区键（rbac 分区 "a" 的签发方 iss=rbac-a
	// 对应 "a"）。非空时验签后焊进 callctx.Meta.Partition——认证请求的分区
	// 以令牌为准，入站 X-Partition 头带来的值被覆盖；空则不动 callctx 里的
	// 分区（单分区部署或不用分区的系统）。严格 HTTP 边界已清除 unsigned
	// 分区；如果另有可信服务验证器先重建了分区，空配置也不会覆盖它。
	Partition string
}

// accessClaims 是访问令牌的验签布局。
type accessClaims struct {
	jwt.RegisteredClaims
	TenantID string   `json:"tid,omitempty"`
	Perms    []string `json:"perms"`
}

// stepUpClaims 是 step-up 证明的验签布局。
type stepUpClaims struct {
	jwt.RegisteredClaims
	Purpose string `json:"purpose"`
}

// Middleware 验签并注入 appkit.Actor。语义：
//   - 无 Authorization 头的请求原样放行（不注入 Actor）——判公开与否是
//     Require 的职责，公开端点（探针等）不因本中间件存在而关死；
//   - 除内建 /healthz、/readyz 探针外，有头但验签失败（坏签名、错算法、
//     错 iss、过期、缺 exp、sub 为空）→ 401：凭证无效必须响亮，不能
//     静默降级为匿名；探针始终隐式 Public；
//   - X-Step-Up 同样原则：有头才验，验不过 401（含 sub 与访问令牌不一致、
//     purpose 不是 step-up）；验过则 Actor.StepUpAt = 证明的 iat。
//
// 租户身份在此焊进 callctx：认证请求以令牌 tid 为准。严格安全模式会在
// 更外层先清掉所有 unsigned 身份头与预置 principal，本中间件只从已验签
// token 重建用户与租户身份。
// 域代码与存储层（RLS）统一从 callctx.From(ctx).TenantID 取租户。
//
// 单签发方形态；多签发方（多分区同进程）见 MultiIssuer。
func Middleware(pub ed25519.PublicKey, issuer string) func(http.Handler) http.Handler {
	return MultiIssuer(map[string]Issuer{issuer: {Key: pub}})
}

// MultiIssuer 是 Middleware 的多签发方形态：按令牌 iss 选验签公钥，iss 不在
// 表里即 401；step-up 证明必须与访问令牌出自同一签发方。签发方带 Partition
// 时验签后把分区键焊进 callctx.Meta.Partition。严格 HTTP 边界先剥离所有
// unsigned 分区/租户头；本中间件只从已验签令牌及组合根配置重建身份。
//
// 表为空、iss 为空、公钥缺失都是装配错误，直接 panic。
func MultiIssuer(issuers map[string]Issuer) func(http.Handler) http.Handler {
	if len(issuers) == 0 {
		panic("authn: MultiIssuer 的签发方表为空")
	}
	for iss, is := range issuers {
		if iss == "" {
			panic("authn: 签发方 iss 为空")
		}
		if len(is.Key) != ed25519.PublicKeySize {
			panic(fmt.Sprintf("authn: 签发方 %q 的公钥不是 Ed25519 公钥", iss))
		}
	}
	parse := []jwt.ParserOption{
		jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
		jwt.WithExpirationRequired(),
	}
	// keyFor 按未验签的 iss 选公钥：签名随后用这把钥验，iss 伪造只会拿到
	// 别家的公钥而验不过。
	keyFor := func(tok *jwt.Token) (any, error) {
		iss, err := tok.Claims.GetIssuer()
		if err != nil {
			return nil, err
		}
		is, ok := issuers[iss]
		if !ok {
			return nil, fmt.Errorf("%w: %q", errUnknownIssuer, iss)
		}
		return is.Key, nil
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 框架探针是隐式 Public：kubelet 不应因误带/陈旧凭证而把存活
			// 检查变成 401。身份边界仍在更外层清除 unsigned 身份头。
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
				next.ServeHTTP(w, r)
				return
			}
			raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			var ac accessClaims
			if _, err := jwt.ParseWithClaims(raw, &ac, keyFor, parse...); err != nil {
				writeInvalid(w, err)
				return
			}
			if ac.Subject == "" {
				writeInvalid(w, errEmptySubject)
				return
			}
			issuer := issuers[ac.Issuer]
			actor := appkit.Actor{
				UserID:   ac.Subject,
				TenantID: ac.TenantID,
				Perms:    ac.Perms,
			}
			if sup := r.Header.Get(HeaderStepUp); sup != "" {
				at, err := parseStepUp(issuer.Key, parse, sup, ac.Issuer, ac.Subject)
				if err != nil {
					writeInvalid(w, err)
					return
				}
				actor.StepUpAt = at
			}
			// 租户身份焊进 callctx：令牌说了算。tid 有值则写入，空值则保持
			// 清空——「无租户令牌 + 伪造的 X-Tenant-Id」不能成立。
			// 严格边界也先清除 unsigned 分区；已配置签发方可重新建立分区。
			meta := callctx.From(r.Context())
			meta.TenantID = actor.TenantID
			if issuer.Partition != "" {
				meta.Partition = issuer.Partition
			}
			ctx := callctx.With(appkit.WithActor(r.Context(), actor), meta)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// parseStepUp 验证 step-up 证明并返回其 iat（新鲜度判定在 Require 侧）。
// iss 钉死与访问令牌同一签发方、sub 经 jwt.WithSubject 钉死与访问令牌
// 一致——别家签的、别人的证明都不能替我过关。
func parseStepUp(pub ed25519.PublicKey, parse []jwt.ParserOption, raw, issuer, subject string) (time.Time, error) {
	var sc stepUpClaims
	opts := append(append([]jwt.ParserOption{}, parse...), jwt.WithIssuer(issuer), jwt.WithSubject(subject))
	if _, err := jwt.ParseWithClaims(raw, &sc,
		func(tok *jwt.Token) (any, error) {
			if iss, _ := tok.Claims.GetIssuer(); iss != issuer {
				return nil, errIssuerMismatch
			}
			return pub, nil
		}, opts...); err != nil {
		return time.Time{}, err
	}
	if sc.Purpose != stepUpPurpose {
		return time.Time{}, errWrongPurpose
	}
	if sc.IssuedAt == nil {
		return time.Time{}, errNoIssuedAt
	}
	return sc.IssuedAt.Time, nil
}

func writeInvalid(w http.ResponseWriter, err error) {
	apperr.WriteProblem(w, apperr.Unauthenticated("invalid or expired credential").WithCause(err))
}
