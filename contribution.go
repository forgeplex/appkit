package appkit

import (
	"fmt"
	"sort"
)

// Contribution 是 Registry 中一个有来源的多实现条目。
// Name 仅用于组合与排序，不提供租户、授权或隔离语义。
type Contribution[T any] struct {
	Name   string
	Module string
	Value  T
}

// Contribute 在当前 Module.Register 中声明类型 T 的一个集合条目。
// 同一 Go 类型的 name 必须唯一；其他类型可以复用相同 name。构造器在启动的
// 统一依赖解析阶段 eager 执行一次，且只能通过 Registry 解析普通 binding。
// 非法名称、nil 构造器、重复键或 Register 阶段之外的调用会 panic；模块 Register
// 中的 panic 会转成启动错误。
func Contribute[T any](reg *Registry, name string, ctor func(*Registry) (T, error)) {
	if reg.registered || !reg.registeringModule {
		panic("appkit: Contribution 只能在 Module.Register 阶段声明")
	}
	if err := validateBindingName(name); err != nil {
		panic(err)
	}
	key := bindingKey{typ: typeOf[T](), name: name}
	if ctor == nil {
		panic(fmt.Sprintf("appkit: Contribute[%s] 的 ctor 不能为 nil", key))
	}
	if prev, ok := reg.contributions[key]; ok {
		panic(fmt.Sprintf("appkit: Contribution %s 已由模块 %q 提供，模块 %q 重复 Contribute", key, prev.module, reg.current))
	}
	reg.contributions[key] = &binding{
		module: reg.current,
		ctor:   func(r *Registry) (any, error) { return ctor(r) },
	}
}

// ResolveContributions 返回类型 T 的已构造条目快照，按 name 升序稳定排序。
// 只能在统一依赖解析完成后调用（例如 Module.Setup）；解析期间不暴露部分集合，
// Contribution 构造器也不能依赖该集合。
func ResolveContributions[T any](reg *Registry) ([]Contribution[T], error) {
	if !reg.contributionsResolved {
		return nil, fmt.Errorf("appkit: ResolveContributions[%s] 只能在启动依赖解析完成后调用", typeOf[T]())
	}
	typ := typeOf[T]()
	keys := make([]bindingKey, 0)
	for key := range reg.contributions {
		if key.typ == typ {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].name < keys[j].name })
	values := make([]Contribution[T], 0, len(keys))
	for _, key := range keys {
		b := reg.contributions[key]
		if !b.resolved {
			return nil, fmt.Errorf("appkit: Contribution %s 尚未构造", key)
		}
		var value T
		if b.value != nil {
			resolved, ok := b.value.(T)
			if !ok {
				return nil, fmt.Errorf("appkit: Contribution %s 的缓存类型不匹配", key)
			}
			value = resolved
		}
		values = append(values, Contribution[T]{Name: key.name, Module: b.module, Value: value})
	}
	return values, nil
}
