// Package callctx 定义「跨调用边界存活」的元数据白名单。
//
// contract 的 ctx 防火墙会剥掉 ctx 里的一切值——这是对的，进程内调用不该
// 比跨网络调用能多传任何东西。但有一小撮元数据在真实的跨网络调用里本来
// 就会传：request id 让一次请求在所有服务的日志里串得起来，租户 id 决定
// 下游查哪份数据，caller 让下游知道是谁在调。它们必须穿过边界，且必须是
// 穷举的白名单——所以这里是 struct 的具名字段而不是 map：能穿过防火墙的
// 东西由框架决定，业务代码不能自己往里塞（那等于把防火墙拆了）。
//
// 三条传播路径里，框架自动接好的是两条半：
//   - HTTP 入站：httpserver.RequestID 中间件从请求头 Extract 进 ctx；
//   - 契约调用（进程内）：contract.Firewall 在剥值之后把 Meta 重新放回；
//   - 事件：outbox 发布时 ToMap 快照进 Event.Meta，relay 投递前 FromMap
//     还原进 ctx——异步链路上 request id 同样连得起来。
//
// 剩下的半条是出站 HTTP：合约仓库的 client 是手写的（appkit gen 只生成
// events/errors/wrap，不生成 client），必须自己在构造请求时调一次
//
//	callctx.Inject(callctx.From(ctx), req.Header.Set)
//
// 漏了它，进程内绑定拿得到 tenant、远程 client 拿不到——恰好是「部署形态是
// 启动参数」失效的形态，而且只在真拆分部署那天暴露。这一条目前没有机检
// （apptest.Conform 比对错误码与返回值，但看不见 client 的请求头）。
//
// 本包只依赖标准库。
package callctx

import (
	"context"
	"log/slog"
)

// Meta 是跨边界传播的元数据。零值可用，字段按需填。
type Meta struct {
	// RequestID 串起一次外部请求在全链路上的所有日志与 span。
	RequestID string
	// TenantID 是多租户下的租户标识。下游据此选数据边界，不可由请求体覆盖。
	TenantID string
	// Caller 是直接调用方的服务名，用于归因与限流。
	// 语义是「谁调的我」而不是「链路最初是谁」——出站 client 应写自己的
	// 服务名覆盖掉 ctx 里的值，别透传上一跳的。链路起点看 RequestID。
	Caller string
}

// HTTP 传播用的请求头名。
const (
	HeaderRequestID = "X-Request-Id"
	HeaderTenantID  = "X-Tenant-Id"
	HeaderCaller    = "X-Caller"
)

// 事件传播用的 appkit.Event.Meta 键名。带前缀是为了与业务自己写的 meta 分开。
const (
	KeyRequestID = "appkit.request_id"
	KeyTenantID  = "appkit.tenant_id"
	KeyCaller    = "appkit.caller"
)

// IsZero 报告 m 是否不含任何元数据。
func (m Meta) IsZero() bool { return m == Meta{} }

type metaKey struct{}

// With 把 m 放进 ctx，整体替换已有值。
func With(ctx context.Context, m Meta) context.Context {
	return context.WithValue(ctx, metaKey{}, m)
}

// From 取出 ctx 里的元数据；没有时返回零值（调用方不必判空）。
func From(ctx context.Context) Meta {
	m, _ := ctx.Value(metaKey{}).(Meta)
	return m
}

// Merge 用 m 的非空字段覆盖 ctx 里已有的，空字段保持原值。
// 用于逐段补齐：入站中间件先填 request id，认证中间件再填 tenant。
func Merge(ctx context.Context, m Meta) context.Context {
	cur := From(ctx)
	if m.RequestID != "" {
		cur.RequestID = m.RequestID
	}
	if m.TenantID != "" {
		cur.TenantID = m.TenantID
	}
	if m.Caller != "" {
		cur.Caller = m.Caller
	}
	return With(ctx, cur)
}

// LogAttrs 返回非空字段的 slog 属性，便于把元数据统一带进日志。
func (m Meta) LogAttrs() []slog.Attr {
	attrs := make([]slog.Attr, 0, 3)
	if m.RequestID != "" {
		attrs = append(attrs, slog.String("request_id", m.RequestID))
	}
	if m.TenantID != "" {
		attrs = append(attrs, slog.String("tenant_id", m.TenantID))
	}
	if m.Caller != "" {
		attrs = append(attrs, slog.String("caller", m.Caller))
	}
	return attrs
}

// Inject 把非空字段经 set 写出到 HTTP 请求头。出站 client 用：
//
//	callctx.Inject(callctx.From(ctx), req.Header.Set)
func Inject(m Meta, set func(key, value string)) {
	setNonEmpty(set, HeaderRequestID, m.RequestID)
	setNonEmpty(set, HeaderTenantID, m.TenantID)
	setNonEmpty(set, HeaderCaller, m.Caller)
}

// Extract 经 get 从 HTTP 请求头读入。入站中间件用：
//
//	m := callctx.Extract(r.Header.Get)
func Extract(get func(key string) string) Meta {
	return Meta{
		RequestID: get(HeaderRequestID),
		TenantID:  get(HeaderTenantID),
		Caller:    get(HeaderCaller),
	}
}

// ToMap 把非空字段写进事件 meta（dst 为 nil 时按需新建），返回结果 map。
// 保留 dst 里的既有键——业务自己的 meta 不会被覆盖。
func ToMap(m Meta, dst map[string]string) map[string]string {
	if m.IsZero() {
		return dst
	}
	if dst == nil {
		dst = make(map[string]string, 3)
	}
	setNonEmpty(func(k, v string) { dst[k] = v }, KeyRequestID, m.RequestID)
	setNonEmpty(func(k, v string) { dst[k] = v }, KeyTenantID, m.TenantID)
	setNonEmpty(func(k, v string) { dst[k] = v }, KeyCaller, m.Caller)
	return dst
}

// FromMap 从事件 meta 还原元数据。
func FromMap(src map[string]string) Meta {
	return Meta{
		RequestID: src[KeyRequestID],
		TenantID:  src[KeyTenantID],
		Caller:    src[KeyCaller],
	}
}

func setNonEmpty(set func(key, value string), key, value string) {
	if value != "" {
		set(key, value)
	}
}
