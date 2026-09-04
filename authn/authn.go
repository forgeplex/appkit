// Package authn 是访问令牌的验签机制件：解析 Bearer 访问令牌与 X-Step-Up
// step-up 证明，验签后注入 appkit.Actor。判定（Require/Check）在框架根包，
// 本包只负责「凭证 → Actor」这一段——验签只写一次，模块经 ctx 只读。
//
// claims 布局是框架与鉴权提供方之间的契约（提供方按此签发，换提供方即换
// 一个按此布局签发的实现，模块与框架零改动）：
//
//	访问令牌（Bearer）：iss（经 WithIssuer 校验）、sub=用户、exp 必填、
//	                   tid=业务租户（可选）、perms=精确权限码快照。
//	step-up（X-Step-Up）：iss、sub（须与访问令牌一致）、exp 必填、
//	                   purpose="step-up"、iat=新鲜度判定依据。
//
// 提供方验「挑战动作」（MFA/TOTP 等）并签发 step-up 令牌；本包验「挑战
// 证明」（签名、iss、sub 一致、purpose、exp）。
//
// 挂载是组合根的部署面决策，通常一行：
//
//	appkit.Middleware(authn.Middleware(pub, "rbac-demo"))
//
// issuer 此处为静态字符串；多分区部署（iss 随分区变化）需要按请求解析
// issuer 的动态变体时再加，纯加法。
package authn

import (
	"crypto/ed25519"
	"errors"
	"net/http"
	"slices"
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
	errEmptySubject = errors.New("令牌 sub 为空")
	errWrongPurpose = errors.New("step-up 证明的 purpose 不是 step-up")
	errNoIssuedAt   = errors.New("step-up 证明缺少 iat")
)

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
func Middleware(pub ed25519.PublicKey, issuer string) func(http.Handler) http.Handler {
	parse := []jwt.ParserOption{
		jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
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
			if _, err := jwt.ParseWithClaims(raw, &ac,
				func(*jwt.Token) (any, error) { return pub, nil }, parse...); err != nil {
				writeInvalid(w, err)
				return
			}
			if ac.Subject == "" {
				writeInvalid(w, errEmptySubject)
				return
			}
			actor := appkit.Actor{
				UserID:   ac.Subject,
				TenantID: ac.TenantID,
				Perms:    ac.Perms,
			}
			if sup := r.Header.Get(HeaderStepUp); sup != "" {
				at, err := parseStepUp(pub, parse, sup, ac.Subject)
				if err != nil {
					writeInvalid(w, err)
					return
				}
				actor.StepUpAt = at
			}
			// 租户身份焊进 callctx：令牌说了算。tid 有值则写入，空值则保持
			// 清空——「无租户令牌 + 伪造的 X-Tenant-Id」不能成立。
			meta := callctx.From(r.Context())
			meta.TenantID = actor.TenantID
			ctx := callctx.With(appkit.WithActor(r.Context(), actor), meta)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// parseStepUp 验证 step-up 证明并返回其 iat（新鲜度判定在 Require 侧）。
// sub 经 jwt.WithSubject 钉死与访问令牌一致——别人的证明不能替我过关。
func parseStepUp(pub ed25519.PublicKey, parse []jwt.ParserOption, raw, subject string) (time.Time, error) {
	var sc stepUpClaims
	if _, err := jwt.ParseWithClaims(raw, &sc,
		func(*jwt.Token) (any, error) { return pub, nil },
		append(slices.Clone(parse), jwt.WithSubject(subject))...); err != nil {
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
