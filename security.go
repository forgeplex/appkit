package appkit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/callctx"
)

// SecurityMode 声明进程 HTTP 面的身份边界。零值不是安全模式：每个会监听
// HTTP 的 App 都必须显式选择，避免漏配时退化为匿名服务。
type SecurityMode string

const (
	SecurityUnspecified     SecurityMode = ""
	SecurityDisabled        SecurityMode = "disabled"
	SecurityUserFacing      SecurityMode = "user_facing"
	SecurityInternalService SecurityMode = "internal_service"
	SecurityMixed           SecurityMode = "mixed"
)

// Security 设置 HTTP 安全模式。SecurityDisabled 只用于 dev/test；bootstrap
// 会结合 env 强制这一限制。直接构造 App 的调用方也必须显式选择模式。
func Security(mode SecurityMode) Option {
	return func(c *appConfig) { c.securityMode = mode }
}

func validateSecurityMode(mode SecurityMode) error {
	switch mode {
	case SecurityDisabled, SecurityUserFacing, SecurityInternalService, SecurityMixed:
		return nil
	case SecurityUnspecified:
		return errors.New("appkit: 未声明 HTTP 安全模式——请显式注入 appkit.Security；仅 dev/test 可选择 SecurityDisabled")
	default:
		return fmt.Errorf("appkit: 未知 HTTP 安全模式 %q", mode)
	}
}

type routeSecurity string

const (
	routeUnclassified    routeSecurity = "unclassified"
	routePublic          routeSecurity = "public"
	routeAuthenticated   routeSecurity = "authenticated"
	routePermission      routeSecurity = "permission"
	routeInternalService routeSecurity = "internal_service"
)

// ServicePrincipal 是验签后的服务身份与委托快照。只有服务凭证验证中间件应
// 注入它；网络输入本身永远不能直接构造可信 principal。
//
// TenantID/MerchantID 是服务凭证中已签名且已授权的委托范围，不是 unsigned
// X-Tenant-Id/X-Merchant-Id 请求头。服务 JWT 的具体签发与验证在后续机制件中
// 完成，本结构先固定路由授权所需的可信落点。
type ServicePrincipal struct {
	Subject    string
	Issuer     string
	Audience   []string
	KeyID      string
	IssuedAt   time.Time
	ExpiresAt  time.Time
	TenantID   string
	MerchantID string
}

type servicePrincipalKey struct{}

// WithServicePrincipal 把已验证的服务身份放进 ctx。正常路径仅供服务凭证
// 验证中间件使用；测试可用它构造授权场景。
func WithServicePrincipal(ctx context.Context, p ServicePrincipal) context.Context {
	p.Audience = slices.Clone(p.Audience)
	return context.WithValue(ctx, servicePrincipalKey{}, p)
}

// ServicePrincipalFrom 读取已验证服务身份。Subject 为空视为未认证。
func ServicePrincipalFrom(ctx context.Context) (ServicePrincipal, bool) {
	p, ok := ctx.Value(servicePrincipalKey{}).(ServicePrincipal)
	p.Audience = slices.Clone(p.Audience)
	return p, ok && p.Subject != ""
}

// MountPublic 挂载无需身份的公开路由。公开是显式安全决策，不是“忘了包
// 鉴权”的默认结果。
func (r *Registry) MountPublic(pattern string, h http.Handler) {
	r.mount(pattern, h, routePublic, "")
}

// MountAuthenticated 挂载只要求有效用户身份、无需具体权限码的路由。
func (r *Registry) MountAuthenticated(pattern string, h http.Handler) {
	if h == nil {
		panic(fmt.Sprintf("路由 %q 的 handler 不能为 nil", pattern))
	}
	r.mount(pattern, requireUser(h), routeAuthenticated, "")
}

// MountPermission 挂载要求指定权限码的用户路由；code 必须已由某模块声明。
func (r *Registry) MountPermission(pattern, code string, h http.Handler) {
	if h == nil {
		panic(fmt.Sprintf("路由 %q 的 handler 不能为 nil", pattern))
	}
	r.mount(pattern, r.Require(code, h), routePermission, code)
}

// MountInternalService 挂载只接受已验证服务身份的内部路由。
func (r *Registry) MountInternalService(pattern string, h http.Handler) {
	if h == nil {
		panic(fmt.Sprintf("路由 %q 的 handler 不能为 nil", pattern))
	}
	r.mount(pattern, requireService(h), routeInternalService, "")
}

func requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		actor, ok := ActorFrom(req.Context())
		if !ok || actor.UserID == "" {
			apperr.WriteProblem(w, apperr.Unauthenticated("authentication required"))
			return
		}
		next.ServeHTTP(w, req)
	})
}

func requireService(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if _, ok := ServicePrincipalFrom(req.Context()); !ok {
			apperr.WriteProblem(w, apperr.Unauthenticated("service authentication required"))
			return
		}
		next.ServeHTTP(w, req)
	})
}

func (r *Registry) validateRouteSecurity(mode SecurityMode, pprof bool) error {
	if mode == SecurityDisabled {
		return nil
	}
	var problems []string
	for _, m := range r.mounts {
		prefix := fmt.Sprintf("模块 %q 的路由 %q", m.module, m.pattern)
		switch {
		case m.security == routeUnclassified:
			problems = append(problems, prefix+" 未声明安全分类——使用 MountPublic/MountAuthenticated/MountPermission/MountInternalService")
		case mode == SecurityUserFacing && m.security == routeInternalService:
			problems = append(problems, prefix+" 声明为 InternalService，但当前模式是 user_facing（需要 mixed 或 internal_service）")
		case mode == SecurityInternalService && (m.security == routeAuthenticated || m.security == routePermission):
			problems = append(problems, prefix+" 是用户身份路由，但当前模式是 internal_service（需要 mixed 或 user_facing）")
		}
	}
	if pprof && mode == SecurityUserFacing {
		problems = append(problems, "pprof 只允许 internal_service/mixed 模式并按 InternalService 保护；user_facing 模式禁止挂载")
	}
	if len(problems) > 0 {
		return errors.New("appkit: HTTP 路由安全配置不完整：\n  " + strings.Join(problems, "\n  "))
	}
	return nil
}

// identityBoundary 是所有严格模式的最外层信任边界：网络请求自带的可信
// ctx 与 unsigned 身份头一律清空，后续验签中间件只能从凭证重建身份。
// RequestID 保留；它用于追踪，不授予数据或调用权限。
func identityBoundary(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		meta := callctx.From(req.Context())
		meta.Partition = ""
		meta.TenantID = ""
		meta.Caller = ""
		ctx := callctx.With(req.Context(), meta)
		// 遮蔽可能由边界外代码带入的主体；后续验签器只能重新注入新值。
		ctx = clearActor(ctx)
		ctx = WithServicePrincipal(ctx, ServicePrincipal{})

		clean := req.Clone(ctx)
		clean.Header.Del(callctx.HeaderPartition)
		clean.Header.Del(callctx.HeaderTenantID)
		clean.Header.Del(callctx.HeaderCaller)
		// Merchant header 会在 merchant principal 正式落地前就按同一信任规则
		// 清除，避免调用方抢先把它当成可信输入。
		clean.Header.Del("X-Merchant-Id")
		next.ServeHTTP(w, clean)
	})
}
