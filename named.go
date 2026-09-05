package appkit

import "fmt"

// ProvideNamed 注册类型 T 的一个具名本地实例。实例由 (T, name) 唯一标识，
// 同类型可注册多个名字，且与 Provide 的无名绑定相互独立。重复注册会 panic，
// 在 Module.Register 内发生时由框架转为启动错误。
//
// name 必须匹配 [a-z][a-z0-9._-]*；空名、大小写或空白别名都不被自动归一化。
// 名字用于组合根的实例选择，不是租户、商户或请求的身份，也不提供按消费方路由。
// 契约实现应使用 ProvideContractNamed，以确保生成的边界 wrapper 被应用。
func ProvideNamed[T any](reg *Registry, name string, ctor func(*Registry) (T, error)) {
	if err := validateBindingName(name); err != nil {
		panic(err)
	}
	key := bindingKey{typ: typeOf[T](), name: name}
	if ctor == nil {
		panic(fmt.Sprintf("appkit: ProvideNamed[%s] 的 ctor 不能为 nil", key))
	}
	if prev, ok := reg.bindings[key]; ok {
		panic(fmt.Sprintf("appkit: %s 已由模块 %q 提供，模块 %q 重复 ProvideNamed", key, prev.module, reg.current))
	}
	reg.bindings[key] = &binding{
		module: reg.current,
		ctor:   func(r *Registry) (any, error) { return ctor(r) },
	}
}

// ProvideValueNamed 注册一个现成的具名值；名字约束与 ProvideNamed 相同。
func ProvideValueNamed[T any](reg *Registry, name string, v T) {
	ProvideNamed(reg, name, func(*Registry) (T, error) { return v, nil })
}

// ProvideContractNamed 注册具名契约实现，并强制应用生成的 wrap。
// 与 ProvideContract 一样，wrap 不能为 nil；ResolveNamed 永远取得包裹后的实现。
// 事务守卫、ctx 防火墙、超时与错误规范化仍由契约仓库生成的 wrapper 提供。
func ProvideContractNamed[T any](reg *Registry, name string, ctor func(*Registry) (T, error), wrap func(T) T) {
	if wrap == nil {
		panic(fmt.Sprintf("appkit: ProvideContractNamed[%s] 的 wrap 不能为 nil（非契约实现请用 ProvideNamed）", typeOf[T]()))
	}
	if ctor == nil {
		panic(fmt.Sprintf("appkit: ProvideContractNamed[%s] 的 ctor 不能为 nil", typeOf[T]()))
	}
	ProvideNamed(reg, name, func(r *Registry) (T, error) {
		v, err := ctor(r)
		if err != nil {
			return v, err
		}
		return wrap(v), nil
	})
}

// ResolveNamed 取 (T, name) 对应的实例：匹配的本地绑定优先，其次是匹配的
// RemoteNamed；绝不回退到其他名字或无名的 Provide/Remote 绑定。
// 构造结果按实例缓存；具名与无名依赖共享启动期解析和循环检测。
func ResolveNamed[T any](reg *Registry, name string) (T, error) {
	var zero T
	if err := validateBindingName(name); err != nil {
		return zero, err
	}
	v, err := reg.resolve(bindingKey{typ: typeOf[T](), name: name})
	if err != nil {
		return zero, err
	}
	if v == nil {
		return zero, nil
	}
	return v.(T), nil
}

// MustResolveNamed 是 ResolveNamed 的 panic 版本，供 Setup 回调内使用。
func MustResolveNamed[T any](reg *Registry, name string) T {
	v, err := ResolveNamed[T](reg, name)
	if err != nil {
		panic(err)
	}
	return v
}

// RemoteNamed 注册一个具名的远程兜底实现。相同 (T, name) 的本地绑定优先；
// 仅在实例被需要时构造远程实现。重复的具名远程绑定在启动期报错。
// name 的约束与 ProvideNamed 相同，非法名字或 nil ctor 会立即 panic。
// 它不改变 Target 的模块选择语义，也不自动为不同消费方选择不同实现。
func RemoteNamed[T any](name string, ctor func(*Registry) (T, error)) Option {
	if err := validateBindingName(name); err != nil {
		panic(err)
	}
	key := bindingKey{typ: typeOf[T](), name: name}
	if ctor == nil {
		panic(fmt.Sprintf("appkit: RemoteNamed[%s] 的 ctor 不能为 nil", key))
	}
	return func(c *appConfig) {
		c.remotes = append(c.remotes, func(reg *Registry) error {
			if _, ok := reg.remotes[key]; ok {
				return fmt.Errorf("appkit: %s 重复 RemoteNamed", key)
			}
			reg.remotes[key] = &binding{
				module: "remote",
				ctor:   func(r *Registry) (any, error) { return ctor(r) },
			}
			return nil
		})
	}
}

func validateBindingName(name string) error {
	valid := len(name) > 0 && name[0] >= 'a' && name[0] <= 'z'
	for i := 1; valid && i < len(name); i++ {
		c := name[i]
		valid = c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
	}
	if !valid {
		return fmt.Errorf("appkit: 实例名 %q 非法：必须匹配 [a-z][a-z0-9._-]*，且不能使用无名绑定", name)
	}
	return nil
}
