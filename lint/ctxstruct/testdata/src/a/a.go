// ctxstruct 的正反例。
package a

import "context"

// Ctx 是别名：解析后仍是 context.Context，同样报告。
type Ctx = context.Context

// 反例：具名字段。
type Job struct {
	ctx  context.Context // want `struct 字段 ctx 的类型是 context.Context：ctx 只能作为函数参数传递，不要存进 struct`
	name string
}

// 反例：内嵌字段。
type Embedded struct {
	context.Context // want `struct 内嵌 context.Context：ctx 只能作为函数参数传递，不要存进 struct`
}

// 反例：经别名。
type Aliased struct {
	c Ctx // want `struct 字段 c 的类型是 context.Context：ctx 只能作为函数参数传递，不要存进 struct`
}

// 反例：一行多名。
type multi struct {
	a, b context.Context // want `struct 字段 a 的类型是 context.Context` `struct 字段 b 的类型是 context.Context`
}

// 正例：CancelFunc、函数类型字段、函数参数里的 ctx 都不报。
type Worker struct {
	cancel context.CancelFunc
	run    func(ctx context.Context) error
	name   string
}

func Do(ctx context.Context) error {
	_ = ctx
	return nil
}
