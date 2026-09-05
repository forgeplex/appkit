// Package tx 是事务边界的接口面，零驱动依赖。
//
// 业务包只 import 本包：service 层用 Transactor.Do 划定事务边界，
// 事务句柄由实现方（appkit/pgtx）经 With 藏进 context，业务代码永远见不到它。
// contract 边界用 HasTx/Strip 实现「事务内禁跨模块调用」的运行时守卫与 ctx 防火墙。
package tx

import "context"

// Transactor 划定一个事务边界：fn 内经 repository 接口的读写都在同一事务中，
// fn 返回错误或 panic 则回滚。嵌套调用 Do 由实现方以 savepoint 语义支持。
type Transactor interface {
	Do(ctx context.Context, fn func(ctx context.Context) error) error
}

type ctxKey struct{}

// With 由 Transactor 实现方调用：把事务句柄藏进 ctx。业务代码不得调用。
func With(ctx context.Context, handle any) context.Context {
	return context.WithValue(ctx, ctxKey{}, handle)
}

// Value 由数据层实现方调用：取出当前事务句柄（无事务时为 nil）。
func Value(ctx context.Context) any {
	return ctx.Value(ctxKey{})
}

// HasTx 报告 ctx 是否处于事务中。contract 边界以此实现运行时守卫。
func HasTx(ctx context.Context) bool {
	return Value(ctx) != nil
}

// Strip 返回一个不携带事务句柄的 ctx（其余值、deadline、取消传播保留）。
func Strip(ctx context.Context) context.Context {
	if !HasTx(ctx) {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, nil)
}

type readAllKey struct{}

// WithReadAllTenants 标记：由此 ctx 开启的事务以「读全部租户」模式运行——
// 租户域的 RLS 对 SELECT 放开全部行，写入仍只能落在当前租户（写永远要
// 显式目标，"写全部"不存在）。用途是跨租户的管理面读路径：运营看全部
// 商户的订单、全局搜索、总览看板。
//
// 这是进程内的 ctx 值，不进 callctx 白名单：contract 防火墙会剥掉它，
// 事件 meta 不带它——「读全部」不跨边界传播，下游域不会因上游持有该
// 权限而连带放开。调用方须先过权限门（reg.Require 跨租户码）再打标记，
// 且标记必须在最外层 Do 之前打：嵌套事务内切模式会让 SET LOCAL 延续到
// 外层事务结束，实现方对此报错拒绝（见 pgtx）。
func WithReadAllTenants(ctx context.Context) context.Context {
	return context.WithValue(ctx, readAllKey{}, true)
}

// ReadsAllTenants 报告 ctx 是否带「读全部租户」标记。由 Transactor 实现方
// 在开启事务时读取；业务代码不需要它。
func ReadsAllTenants(ctx context.Context) bool {
	v, _ := ctx.Value(readAllKey{}).(bool)
	return v
}
