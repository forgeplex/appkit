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
