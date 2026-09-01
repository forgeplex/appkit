package callctx

import (
	"context"
	"net/netip"
)

// ClientIP 是「请求内」的请求元数据：与 Meta 不同，它刻意不进跨边界白名单。
// 信任解析（对端在可信代理网段内才信转发头）只在 HTTP 边缘做一次——
// httpserver.ClientIP 中间件；模块内经 ClientIP(ctx) 读取，不可能重算、
// 也不可能被伪造头骗到。防火墙剥掉它与剥掉其他 ctx 值一样是有意为之：
// 下游域看到的「对端 IP」是上一跳的地址，语义与「原始客户端 IP」不同；
// 真有跨边界需求（审计链路要把起点 IP 带进异步事件）时业务可自行写入
// Event.Meta。

type clientIPKey struct{}

// WithClientIP 把已解析的客户端 IP 存进 ctx。由入站中间件调用一次，
// 业务代码只读不写。
func WithClientIP(ctx context.Context, ip netip.Addr) context.Context {
	return context.WithValue(ctx, clientIPKey{}, ip)
}

// ClientIP 返回 ctx 里的客户端 IP；未经 httpserver.ClientIP 中间件处理
// （或解析不出）时返回零值，调用方用 IsValid() 判断。
func ClientIP(ctx context.Context) netip.Addr {
	ip, _ := ctx.Value(clientIPKey{}).(netip.Addr)
	return ip
}
