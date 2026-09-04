package appkit

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/forgeplex/appkit/health"
)

// Registry 收集模块对系统的全部贡献。Register 阶段只声明，装配阶段统一解析。
type Registry struct {
	bindings map[reflect.Type]*binding
	remotes  map[reflect.Type]*binding

	mounts     []mountReg
	setups     []namedHook
	migrations []MigrationSet
	consumers  []ConsumerReg
	health     *health.Registry
	starts     []startHook
	stops      []stopHook

	// 权限码声明与端点绑定：声明只在 Register 阶段收集（registered 置位
	// 后拒绝迟到声明），绑定在 Register/Setup 阶段都可能产生（模块内部
	// mux 通常在 Setup 装配），统一在全部 Setup 之后校验 ⊆ 关系。
	permDecls    map[string]permDeclReg
	permBindings []permBinding
	registered   bool
	// startStages 记录每个模块最近一次 OnStart 的 stage，供 OnStop 定序。
	startStages map[string]int
	// workerErr 承接 Worker 的异常退出，带缓冲避免写侧阻塞。
	workerErr chan error

	// current 是正在 Register/Setup 的模块名，用于归属与报错。
	current string
	// resolving 是解析栈，用于循环依赖检测与报错路径。
	resolving []reflect.Type
}

type binding struct {
	ctor     func(*Registry) (any, error)
	module   string
	resolved bool
	inflight bool
	value    any
}

type mountReg struct {
	pattern    string
	handler    http.Handler
	module     string
	security   routeSecurity
	permission string
}

type namedHook struct {
	fn     HookFunc
	module string
}

type startHook struct {
	stage  int
	seq    int
	fn     HookFunc
	module string
}

type stopHook struct {
	stage  int
	seq    int
	fn     HookFunc
	module string
}

// MigrationSet 是一个模块声明的数据库迁移：fsys 中的 *.sql 按文件名序应用，
// 且只允许操作 schema 名下的对象（appkit check 校验）。
type MigrationSet struct {
	Schema string
	FS     fs.FS
	Module string
}

// ConsumerReg 是一个模块声明的事件消费者。
type ConsumerReg struct {
	Topic   string
	Handler EventHandler
	Module  string
}

func newRegistry() *Registry {
	return &Registry{
		bindings:    make(map[reflect.Type]*binding),
		remotes:     make(map[reflect.Type]*binding),
		health:      health.NewRegistry(),
		startStages: make(map[string]int),
		workerErr:   make(chan error, 1),
		permDecls:   make(map[string]permDeclReg),
	}
}

// Mount 把一个未分类 http.Handler 挂到根路由，仅为 SecurityDisabled 的
// 向后兼容保留；严格模式会在监听前拒绝它。新代码应按暴露面改用
// MountPublic、MountAuthenticated、MountPermission 或 MountInternalService。
// pattern 使用标准库 ServeMux 语法（Go 1.22+，可含方法与通配符）。
// 同一 pattern 重复挂载在启动期报错。
func (r *Registry) Mount(pattern string, h http.Handler) {
	r.mount(pattern, h, routeUnclassified, "")
}

func (r *Registry) mount(pattern string, h http.Handler, security routeSecurity, permission string) {
	if h == nil {
		panic(fmt.Sprintf("路由 %q 的 handler 不能为 nil", pattern))
	}
	r.mounts = append(r.mounts, mountReg{
		pattern: pattern, handler: h, module: r.current,
		security: security, permission: permission,
	})
}

// Setup 注册一个装配回调：在全部模块 Register 完、依赖图解析通过之后、
// OnStart 之前按注册顺序执行。需要 Resolve 依赖的初始化放这里。
func (r *Registry) Setup(fn HookFunc) {
	r.setups = append(r.setups, namedHook{fn: fn, module: r.current})
}

// Migrations 声明本模块的 schema 迁移。
func (r *Registry) Migrations(schema string, fsys fs.FS) {
	r.migrations = append(r.migrations, MigrationSet{Schema: schema, FS: fsys, Module: r.current})
}

// Consumer 声明本模块消费某个 topic 的事件。投递语义见 EventHandler。
// Register 与 Setup 阶段都可登记——框架在全部 Setup 之后统一订阅到 Bus。
func (r *Registry) Consumer(topic string, h EventHandler) {
	r.consumers = append(r.consumers, ConsumerReg{Topic: topic, Handler: h, Module: r.current})
}

// Health 注册一个就绪检查：任一检查失败时 /readyz 返回 503。
func (r *Registry) Health(name string, c health.Checker) {
	r.health.Add(fmt.Sprintf("%s/%s", r.current, name), c)
}

// OnStart 注册启动钩子，stage 见 StageInfra 等常量。
func (r *Registry) OnStart(stage int, fn HookFunc) {
	r.starts = append(r.starts, startHook{stage: stage, seq: len(r.starts), fn: fn, module: r.current})
	r.startStages[r.current] = stage
}

// OnStop 注册关停钩子。执行顺序镜像启动：stage 降序、同 stage 注册逆序。
// stage 取本模块最近一次 OnStart 的 stage（尚无 OnStart 则默认 StageWorker）。
func (r *Registry) OnStop(fn HookFunc) {
	stage, ok := r.startStages[r.current]
	if !ok {
		stage = StageWorker
	}
	r.stops = append(r.stops, stopHook{stage: stage, seq: len(r.stops), fn: fn, module: r.current})
}

// HealthRegistry 暴露给框架内部（httpserver 装配）使用。
func (r *Registry) HealthRegistry() *health.Registry { return r.health }

// MigrationSets 返回全部已声明迁移（供框架迁移 runner 与 appkit check 使用）。
func (r *Registry) MigrationSets() []MigrationSet { return r.migrations }

// Consumers 返回全部已声明消费者（供 Bus 装配使用）。
func (r *Registry) Consumers() []ConsumerReg { return r.consumers }

// Provide 注册 T 的本地实现（惰性构造，装配阶段统一实例化并缓存）。
// 同一类型重复 Provide 在启动期报错——契约实现必须唯一。
func Provide[T any](reg *Registry, ctor func(*Registry) (T, error)) {
	t := typeOf[T]()
	if prev, ok := reg.bindings[t]; ok {
		panic(fmt.Sprintf("appkit: %s 已由模块 %q 提供，模块 %q 重复 Provide", t, prev.module, reg.current))
	}
	reg.bindings[t] = &binding{
		module: reg.current,
		ctor:   func(r *Registry) (any, error) { return ctor(r) },
	}
}

// ProvideValue 注册一个现成的值。
func ProvideValue[T any](reg *Registry, v T) {
	Provide(reg, func(*Registry) (T, error) { return v, nil })
}

// ProvideContract 注册契约 T 的本地实现，并在 Provide 处强制应用 wrap——
// wrap 是合约仓库生成的拦截 wrapper（每个方法体内经 contract.Call 获得
// 事务守卫、ctx 防火墙、超时与错误规范化）。经此注册后，Resolve 拿到的
// 永远是已包裹的实现，裸实现进不了 registry（DESIGN §5.3）。
// 非契约类型（模块内部依赖）用 Provide/ProvideValue。
func ProvideContract[T any](reg *Registry, ctor func(*Registry) (T, error), wrap func(T) T) {
	if wrap == nil {
		panic(fmt.Sprintf("appkit: ProvideContract[%s] 的 wrap 不能为 nil（非契约实现请用 Provide）", typeOf[T]()))
	}
	Provide(reg, func(r *Registry) (T, error) {
		v, err := ctor(r)
		if err != nil {
			return v, err
		}
		return wrap(v), nil
	})
}

// Resolve 取 T 的实现：先找本地 Provide，找不到再落到 Remote 绑定；
// 都没有则报错并列出需要它的模块。循环依赖在这里被检测并报出完整路径。
func Resolve[T any](reg *Registry) (T, error) {
	var zero T
	v, err := reg.resolve(typeOf[T]())
	if err != nil {
		return zero, err
	}
	return v.(T), nil
}

// MustResolve 是 Resolve 的 panic 版本，供 Setup 回调内使用
// （Setup 里的 panic 会被装配阶段捕获为启动错误）。
func MustResolve[T any](reg *Registry) T {
	v, err := Resolve[T](reg)
	if err != nil {
		panic(err)
	}
	return v
}

func (r *Registry) resolve(t reflect.Type) (any, error) {
	b, ok := r.bindings[t]
	if !ok {
		b, ok = r.remotes[t]
	}
	if !ok {
		return nil, fmt.Errorf("appkit: 没有 %s 的实现（模块 %q 需要它）：目标模块不在 -target 集内且未注册 Remote 绑定", t, r.current)
	}
	if b.resolved {
		return b.value, nil
	}
	if b.inflight {
		return nil, fmt.Errorf("appkit: 依赖循环：%s", r.cyclePath(t))
	}
	b.inflight = true
	r.resolving = append(r.resolving, t)
	v, err := b.ctor(r)
	r.resolving = r.resolving[:len(r.resolving)-1]
	b.inflight = false
	if err != nil {
		return nil, fmt.Errorf("appkit: 构造 %s（模块 %q）失败: %w", t, b.module, err)
	}
	b.value = v
	b.resolved = true
	return v, nil
}

func (r *Registry) cyclePath(t reflect.Type) string {
	var b strings.Builder
	for _, s := range r.resolving {
		b.WriteString(s.String())
		b.WriteString(" → ")
	}
	b.WriteString(t.String())
	return b.String()
}

// resolveAll 强制实例化全部本地绑定：缺依赖、循环依赖、构造失败都在启动期暴露。
func (r *Registry) resolveAll() error {
	types := make([]reflect.Type, 0, len(r.bindings))
	for t := range r.bindings {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool { return types[i].String() < types[j].String() })
	for _, t := range types {
		if _, err := r.resolve(t); err != nil {
			return err
		}
	}
	return nil
}

func (r *Registry) runSetups(ctx context.Context) (err error) {
	for _, s := range r.setups {
		if e := r.runSetup(ctx, s); e != nil {
			return e
		}
	}
	return nil
}

func (r *Registry) runSetup(ctx context.Context, s namedHook) (err error) {
	r.current = s.module
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("appkit: 模块 %q 的 Setup panic: %v", s.module, p)
		}
	}()
	if err := s.fn(ctx); err != nil {
		return fmt.Errorf("appkit: 模块 %q 的 Setup 失败: %w", s.module, err)
	}
	return nil
}

func typeOf[T any]() reflect.Type {
	return reflect.TypeFor[T]()
}
