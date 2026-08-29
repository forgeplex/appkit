// Package contract 实现跨模块契约调用的边界语义。
//
// 无论对端是进程内实现还是远程 client，每次契约方法调用都必须经过 Call：
// 由合约仓库生成的 wrapper/client 在方法体内调用它。Call 保证两种部署形态下
// 语义一致（ServiceWeaver 教训的反向落地——不隐藏网络边界，而是让进程内调用
// 也表现得像一次可失败的远程调用）：
//
//  1. 运行时守卫：事务内发起契约调用直接失败（apperr.CodeTxBoundary）。
//     静态分析对这条规则只能提高绕过成本，运行时守卫在任何测试里都会暴露。
//  2. ctx 防火墙：剥离事务句柄与请求作用域值，只保留取消/超时传播与
//     OpenTelemetry span——与跨网络传播的信息一致。
//  3. 超时：每次调用有独立超时（默认 5s），进程内实现同样受约束。
//  4. 错误规范化：任何错误折叠为 *apperr.Error，错误身份 = 错误码。
package contract

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/tx"
)

// DefaultTimeout 是单次契约调用的默认超时。
const DefaultTimeout = 5 * time.Second

var errTxBoundary = apperr.New(apperr.CodeTxBoundary, 500,
	"事务内禁止跨模块调用：先提交本地事务，一致性走 outbox 事件或事后补偿")

// firewalled 只向上游透传取消/超时，不透传任何 ctx 值。
// span 由 Call 显式重新注入。
type firewalled struct{ context.Context }

func (firewalled) Value(any) any { return nil }

// Firewall 返回剥离了全部 ctx 值（含事务句柄）的 ctx，保留取消/超时与当前 span。
func Firewall(ctx context.Context) context.Context {
	span := trace.SpanFromContext(ctx)
	return trace.ContextWithSpan(firewalled{ctx}, span)
}

// Call 执行一次契约调用。system/method 用于 span 命名与错误归属，
// 形如 Call(ctx, "ledger", "PostEntries", 0, func(ctx) (T, error) { ... })。
// timeout 为 0 时取 DefaultTimeout。
func Call[T any](ctx context.Context, system, method string, timeout time.Duration, fn func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	if tx.HasTx(ctx) {
		return zero, errTxBoundary.WithDetail("system", system).WithDetail("method", method)
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	cctx, cancel := context.WithTimeout(Firewall(ctx), timeout)
	defer cancel()

	tracer := otel.Tracer("appkit/contract")
	cctx, span := tracer.Start(cctx, system+"."+method,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("appkit.contract.system", system),
			attribute.String("appkit.contract.method", method),
		))
	defer span.End()

	v, err := fn(cctx)
	if err != nil {
		e := normalize(cctx, err)
		span.SetStatus(codes.Error, e.Code())
		return zero, e
	}
	span.SetStatus(codes.Ok, "")
	return v, nil
}

// normalize 把任意错误折叠为 *apperr.Error；超时/取消归为 CodeUnavailable，
// 让调用方在两种部署形态下拿到同一种可重试信号。
func normalize(ctx context.Context, err error) *apperr.Error {
	if ctx.Err() != nil {
		return apperr.Unavailable(err)
	}
	return apperr.From(err)
}
