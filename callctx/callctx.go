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
// 租户的信任模型：Extract 只负责传输，不赋予可信语义。App 的严格安全模式
// 在最外层清除 X-Partition/X-Tenant-Id/X-Caller 与预置身份；随后用户 token 或
// 服务凭证验证器从已验证声明重建 Meta。因而匿名请求即使伪造身份头也不会
// 得到可信租户；Disabled 仅供 dev/test，保留旧的原样传播行为。
//
// 分区键（Partition）是租户之外的第二个数据维度：分区域域按它选 schema，
// 租户域按 TenantID 过滤行，一个请求可以同时带两者（分区 a 里的商户 a1）。
// 信任模型与租户同构：外部入口以令牌签发方所属的分区为准（authn.MultiIssuer
// 按 iss 焊入）；严格 HTTP 入站不会信任 X-Partition，内部调用也须重新验凭证。
//
// 生成的契约 NewSecureClient 通过服务凭证传递已授权的 Partition/TenantID，
// Caller 由接收端验签后的 sub 重建，RequestID 可明文传播。下述 Transport
// 只供 legacy/dev 或明确可信的自定义传播，不是生产服务身份验证器：
//
//	client := &http.Client{Transport: callctx.Transport{Caller: "ledger"}}
//
// 也可以在构造每个请求时手写 callctx.Inject(callctx.From(ctx), req.Header.Set)，
// 但那把「会不会漏」摊到了每一个调用点上，而 Transport 只有装配处一处。
//
// 生成客户端会接好传播层；手写客户端仍须自行接入。Transport 只表达候选
// 元数据，不授权。严格入站不会把这些 unsigned 身份头当作可信数据。
//
// 要验它接没接上，给 apptest.Conform 的每个 Binding 填上 SeenMeta——服务端把
// 「我这次看到了什么」交出来，漏了白名单的那个形态当场就红。填不填仍是自愿的，
// 但填一次就永久有效，而且填的时机是写契约测试时，不是三个月后加第 N 个调用点时。
//
// 本包只依赖标准库。
package callctx

import (
	"context"
	"log/slog"
	"net/http"
)

// Meta 是跨边界传播的元数据。零值可用，字段按需填。
type Meta struct {
	// RequestID 串起一次外部请求在全链路上的所有日志与 span。
	RequestID string
	// Partition 是分区键：分区域域（一套代码、N 份数据分区）据此把事务
	// 路由到对应 schema。与 TenantID 是两个维度——分区决定「落哪个 schema」，
	// 租户决定「落哪些行」；一个字段装两样东西的代价是下游按错的维度查数据
	// （rbac 的分区键经事件 meta 传播、被租户域当业务租户查渠道，就是判例）。
	// 认证请求以令牌签发方所属的分区为准（authn.MultiIssuer 焊入）。
	Partition string
	// TenantID 是多租户下的业务租户标识。下游据此选数据边界（租户域的
	// RLS 行过滤），不可由请求体覆盖。
	TenantID string
	// Caller 是直接调用方的服务名，用于归因与限流。
	// 语义是「谁调的我」而不是「链路最初是谁」——出站 client 应写自己的
	// 服务名覆盖掉 ctx 里的值，别透传上一跳的。链路起点看 RequestID。
	Caller string
}

// HTTP 传播用的请求头名。
const (
	HeaderRequestID = "X-Request-Id"
	HeaderPartition = "X-Partition"
	HeaderTenantID  = "X-Tenant-Id"
	HeaderCaller    = "X-Caller"
)

// 事件传播用的 appkit.Event.Meta 键名。带前缀是为了与业务自己写的 meta 分开。
const (
	KeyRequestID = "appkit.request_id"
	KeyPartition = "appkit.partition"
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
// 用于逐段补齐：入站中间件先填 request id，认证中间件再填 tenant
// （认证路径的租户覆盖/清零在 authn 里精确设置，不经 Merge——空 tid
// 也要能清掉头带来的伪造值）。
func Merge(ctx context.Context, m Meta) context.Context {
	cur := From(ctx)
	if m.RequestID != "" {
		cur.RequestID = m.RequestID
	}
	if m.Partition != "" {
		cur.Partition = m.Partition
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
	attrs := make([]slog.Attr, 0, 4)
	if m.RequestID != "" {
		attrs = append(attrs, slog.String("request_id", m.RequestID))
	}
	if m.Partition != "" {
		attrs = append(attrs, slog.String("partition", m.Partition))
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
	setNonEmpty(set, HeaderPartition, m.Partition)
	setNonEmpty(set, HeaderTenantID, m.TenantID)
	setNonEmpty(set, HeaderCaller, m.Caller)
}

// Extract 经 get 从 HTTP 请求头读入。入站中间件用：
//
//	m := callctx.Extract(r.Header.Get)
func Extract(get func(key string) string) Meta {
	return Meta{
		RequestID: get(HeaderRequestID),
		Partition: get(HeaderPartition),
		TenantID:  get(HeaderTenantID),
		Caller:    get(HeaderCaller),
	}
}

// Transport 是出站 HTTP 的 http.RoundTripper：自动把 ctx 里的白名单写进请求头。
// 装配一次，此后该 client 发出的每个请求都带上：
//
//	client := &http.Client{Transport: callctx.Transport{Caller: "ledger"}}
//
// 这与「在每个调用点手写 Inject」的差别不在省几行，而在漏点的数量：手写时
// 每一个新增的调用点都是一次机会，接 Transport 则整个 client 只有装配处一处。
// 而 callctx 漏掉的失效是静默的——进程内绑定拿得到 tenant、远程 client 拿不到，
// 只在真拆分部署那天暴露，所以漏点越少越好。
//
// 零值可用（Base 走 http.DefaultTransport，Caller 透传 ctx 里的值），但多数
// 时候你该显式填 Caller，见该字段说明。
type Transport struct {
	// Base 是被包裹的 RoundTripper，nil 表示 http.DefaultTransport。
	Base http.RoundTripper

	// Caller 非空时覆盖写出的 Meta.Caller，填本服务的名字。
	//
	// Meta.Caller 的语义是「谁调的我」，所以出站时正确的值是自己，而不是把
	// 上一跳的名字透传下去。留空即透传——下游会把这次调用归因到错误的服务上，
	// 按 caller 做的限流与配额也就跟着错。链路起点看 RequestID，不看 Caller。
	Caller string
}

// RoundTrip 实现 http.RoundTripper。
func (t Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	m := From(req.Context())
	if t.Caller != "" {
		m.Caller = t.Caller
	}
	if m.IsZero() {
		return base.RoundTrip(req)
	}
	// RoundTripper 的约定是不得改动传入的请求（net/http 会重试、会并发复用
	// 同一个 *Request），所以改头之前先克隆。
	r := req.Clone(req.Context())
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	Inject(m, r.Header.Set)
	return base.RoundTrip(r)
}

// ToMap 把非空字段写进事件 meta（dst 为 nil 时按需新建），返回结果 map。
// 保留 dst 里的既有键——业务自己的 meta 不会被覆盖。
func ToMap(m Meta, dst map[string]string) map[string]string {
	if m.IsZero() {
		return dst
	}
	if dst == nil {
		dst = make(map[string]string, 4)
	}
	setNonEmpty(func(k, v string) { dst[k] = v }, KeyRequestID, m.RequestID)
	setNonEmpty(func(k, v string) { dst[k] = v }, KeyPartition, m.Partition)
	setNonEmpty(func(k, v string) { dst[k] = v }, KeyTenantID, m.TenantID)
	setNonEmpty(func(k, v string) { dst[k] = v }, KeyCaller, m.Caller)
	return dst
}

// FromMap 从事件 meta 还原元数据。
func FromMap(src map[string]string) Meta {
	return Meta{
		RequestID: src[KeyRequestID],
		Partition: src[KeyPartition],
		TenantID:  src[KeyTenantID],
		Caller:    src[KeyCaller],
	}
}

func setNonEmpty(set func(key, value string), key, value string) {
	if value != "" {
		set(key, value)
	}
}
