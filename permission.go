package appkit

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/forgeplex/appkit/apperr"
)

// stepUpMaxAge 是 step-up 证明的新鲜度窗口：Challenge 码的判定要求证明
// 在此窗口内签发，超出须重新挑战。按码配置窗口是加法，留给真实需求。
const stepUpMaxAge = 5 * time.Minute

// PermissionDecl 是一个权限码的声明。模块在 Register 阶段经
// Registry.Permissions 声明本域的码；框架汇总成全应用目录（见
// Registry.PermissionDecls），鉴权提供方（如 rbac）据此把目录同步落库。
//
// 框架只拥有码的「存在性」与判定形态；匹配语义（通配展开、读↔写归一）
// 归提供方——提供方签发令牌时把角色权限解析成精确码快照，框架的判定
// 永远是集合包含。
type PermissionDecl struct {
	// Code 全应用唯一（重复声明在启动期报错）。格式约定 resource:action
	// （如 "files:read"），细则由提供方定义，框架只拒绝空码。
	Code string
	// Name 是管理界面展示名。
	Name string
	// Category 是分组（通常是域名），供管理界面与审计归类。
	Category string
	// Description 补充说明，可空。
	Description string
	// Challenge 为 true 时，除快照含码外还要求新鲜的 step-up 证明
	// （高危操作：删除角色、重置他人 MFA 等）。challenge 的挑战动作由
	// 提供方完成（MFA 等）；框架只验证明——见 authn 包。
	Challenge bool
}

// permDeclReg 是声明的内部记录，带模块归属供报错。
type permDeclReg struct {
	decl   PermissionDecl
	module string
}

// permBinding 是一次端点绑码的内部记录，带模块归属供报错。
type permBinding struct {
	code   string
	module string
}

// Actor 是请求主体的最小形态：身份 + 精确权限码集合 + step-up 证明时刻。
// 由 authn 中间件验签后注入 ctx；Registry.Require 与 Check 据此判定。
// UserID/TenantID 为 string——根包 API 面不出现第三方类型（与 Event.ID
// 同风格），需要 uuid 的模块自行 parse。
type Actor struct {
	// UserID 是用户身份（访问令牌 sub）。空串视同未认证。
	UserID string
	// TenantID 是业务租户（访问令牌 tid，提供方可选填）。
	// 注意与 callctx.Meta.TenantID（分区键，跨边界传播）是两个维度。
	TenantID string
	// Perms 是精确权限码快照（访问令牌 perms）。
	Perms []string
	// StepUpAt 是最近一次已验证 step-up 证明的签发时刻；零值 = 无证明。
	StepUpAt time.Time
}

type actorKey struct{}

// WithActor 把请求主体放进 ctx。正常路径只有 authn 中间件需要它；
// 测试里用它构造判定场景。
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom 取请求主体；第二个返回值报告是否已认证（authn 已验签注入）。
func ActorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok
}

// Check 报告当前请求主体是否持有 code——service 层的代码内判定
// （HTTP 边界用 Registry.Require）。未认证一律 false。
func Check(ctx context.Context, code string) bool {
	a, ok := ActorFrom(ctx)
	return ok && slices.Contains(a.Perms, code)
}

// Permissions 声明本模块的权限码，仅可在 Register 阶段调用——汇总在
// Setup 期即被提供方消费（目录同步落库），迟到声明会破坏时序。
// 空码与重复码（无论同模块还是跨模块）直接 panic，装配期曝光。
func (r *Registry) Permissions(decls ...PermissionDecl) {
	if r.registered {
		panic(fmt.Sprintf("appkit: 模块 %q 在 Register 阶段之后调用 Permissions——"+
			"权限码声明必须在 Register 阶段完成（Setup 期汇总即被提供方消费）", r.current))
	}
	for _, d := range decls {
		if d.Code == "" {
			panic(fmt.Sprintf("appkit: 模块 %q 声明了空权限码", r.current))
		}
		if prev, ok := r.permDecls[d.Code]; ok {
			panic(fmt.Sprintf("appkit: 权限码 %q 重复声明（模块 %q 与 %q）——权限码属主唯一",
				d.Code, prev.module, r.current))
		}
		r.permDecls[d.Code] = permDeclReg{decl: d, module: r.current}
	}
}

// PermissionDecls 返回全部模块已声明的权限码（按码排序，输出稳定），
// 供鉴权提供方在 Setup 阶段读取并同步目录落库。
func (r *Registry) PermissionDecls() []PermissionDecl {
	out := make([]PermissionDecl, 0, len(r.permDecls))
	for _, d := range r.permDecls {
		out = append(out, d.decl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// Require 把端点与权限码绑定：返回的 handler 先判定后放行。
// Register 与 Setup 阶段都可调用（模块内部 mux 通常在 Setup 装配）。
// 绑定未声明的码在启动期报错（全部 Setup 之后统一校验）；
// 跨模块绑码合法（如 gateway 绑别域的码），校验只查「已声明」。
//
// 判定语义（框架统一执行，模块不再各自实现）：
//   - 未认证（无 Actor）→ 401 UNAUTHENTICATED；
//   - 快照不含码 → 403 PERMISSION_DENIED（detail 带 required=码）；
//   - 码带 Challenge 且 step-up 证明不新鲜 → 403 STEP_UP_REQUIRED，
//     客户端完成挑战（提供方的 step-up 端点）后带 X-Step-Up 头重试。
//
// 错误响应直接写 RFC 9457 problem（apperr.WriteProblem）：401/403 不记
// 日志，与 httpserver.WriteError 的口径一致。
func (r *Registry) Require(code string, h http.Handler) http.Handler {
	r.permBindings = append(r.permBindings, permBinding{code: code, module: r.current})
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		actor, ok := ActorFrom(req.Context())
		if !ok || actor.UserID == "" {
			apperr.WriteProblem(w, apperr.Unauthenticated("authentication required"))
			return
		}
		if !slices.Contains(actor.Perms, code) {
			apperr.WriteProblem(w, apperr.PermissionDenied("permission denied").WithDetail("required", code))
			return
		}
		if d, ok := r.permDecls[code]; ok && d.decl.Challenge &&
			time.Since(actor.StepUpAt) > stepUpMaxAge {
			apperr.WriteProblem(w, apperr.StepUpRequired("step-up authentication required").WithDetail("required", code))
			return
		}
		h.ServeHTTP(w, req)
	})
}

// validatePermBindings 校验全部绑定 ⊆ 已声明码。在全部 Setup 之后、
// HTTP 监听之前调用——模块内部 mux 在 Setup 期绑的码同样被覆盖。
func (r *Registry) validatePermBindings() error {
	for _, b := range r.permBindings {
		if _, ok := r.permDecls[b.code]; !ok {
			return fmt.Errorf("appkit: 模块 %q 绑定了未声明的权限码 %q——在 Register 阶段用 reg.Permissions 声明",
				b.module, b.code)
		}
	}
	return nil
}
