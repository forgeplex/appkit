package appkit

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// App 是组合根：装配一组 Module，按 target 决定哪些本地实例化，
// 其余契约落到 Remote 绑定。同一镜像以不同 -target 部署即得
// modular monolith ↔ 微服务切换（Loki 模式）。
type App struct {
	cfg     appConfig
	reg     *Registry
	modules []Module
	// serverErr 承接运行期 HTTP Serve 的异常退出，带缓冲避免写侧阻塞。
	serverErr chan error
}

type appConfig struct {
	target          string
	httpAddr        string
	middleware      []func(http.Handler) http.Handler
	logger          *slog.Logger
	shutdownTimeout time.Duration
	drainDelay      time.Duration
	remotes         []func(*Registry)
	migrator        func(ctx context.Context, sets []MigrationSet) error
	bus             Subscriber
	httpServerOpts  []func(*http.Server)
}

// Option 配置 App。
type Option func(*appConfig)

// Target 选择本地实例化的模块集："all"（默认）或逗号分隔的模块名列表。
func Target(t string) Option {
	return func(c *appConfig) { c.target = t }
}

// HTTPAddr 设置监听地址，默认 ":8080"。healthz/readyz 总是被挂载，
// 即使 target 内没有任何模块路由（worker 角色也需要探针）。
func HTTPAddr(addr string) Option {
	return func(c *appConfig) { c.httpAddr = addr }
}

// Middleware 设置根 HTTP 中间件链（外层在前）。通常传 httpserver.Base(...)。
func Middleware(mw ...func(http.Handler) http.Handler) Option {
	return func(c *appConfig) { c.middleware = append(c.middleware, mw...) }
}

// Logger 设置框架日志器，默认 slog.Default()。
func Logger(l *slog.Logger) Option {
	return func(c *appConfig) { c.logger = l }
}

// ShutdownTimeout 设置关停预算（HTTP drain + 各 OnStop 的总时限），默认 20s。
func ShutdownTimeout(d time.Duration) Option {
	return func(c *appConfig) { c.shutdownTimeout = d }
}

// DrainDelay 设置摘流量（readyz 置 503）与开始关停 HTTP 之间的等待，
// 用于负载均衡器传播，默认 0（K8s 场景配合 preStop 使用）。
func DrainDelay(d time.Duration) Option {
	return func(c *appConfig) { c.drainDelay = d }
}

// Remote 注册 T 的远程兜底实现：当 T 的提供方模块不在 target 集内时，
// 用合约仓库生成的 client（实现同一接口）满足消费方。
func Remote[T any](ctor func(*Registry) (T, error)) Option {
	return func(c *appConfig) {
		c.remotes = append(c.remotes, func(reg *Registry) {
			t := typeOf[T]()
			reg.remotes[t] = &binding{
				module: "remote",
				ctor:   func(r *Registry) (any, error) { return ctor(r) },
			}
		})
	}
}

// Bus 注入事件总线的订阅端（通常是 outbox.DirectBus）。登记过 Consumer
// 的应用必须设置，否则启动期报错——消费者绝不静默丢弃。
func Bus(s Subscriber) Option {
	return func(c *appConfig) { c.bus = s }
}

// HTTPServer 在框架默认值（ReadTimeout 60s、WriteTimeout 60s、IdleTimeout 120s、
// ReadHeaderTimeout 10s）之上调整 http.Server。可多次注册，按序应用。
func HTTPServer(fn func(*http.Server)) Option {
	return func(c *appConfig) { c.httpServerOpts = append(c.httpServerOpts, fn) }
}

// Migrator 注入迁移执行器（通常是 pgmigrate.Runner）。设置后，
// 启动时在依赖解析之后、Setup 之前应用全部已声明的 MigrationSet。
func Migrator(fn func(ctx context.Context, sets []MigrationSet) error) Option {
	return func(c *appConfig) { c.migrator = fn }
}

// New 构造 App。modules 全集在此声明，实际启用集由 Target 决定。
func New(modules []Module, opts ...Option) *App {
	cfg := appConfig{
		target:          "all",
		httpAddr:        ":8080",
		shutdownTimeout: 20 * time.Second,
	}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.logger == nil {
		cfg.logger = slog.Default()
	}
	return &App{cfg: cfg, reg: newRegistry(), modules: modules, serverErr: make(chan error, 1)}
}

// enabledModules 按 target 过滤模块集，target 中的未知模块名报错。
func (a *App) enabledModules() ([]Module, error) {
	if strings.TrimSpace(a.cfg.target) == "" || a.cfg.target == "all" {
		return a.modules, nil
	}
	byName := make(map[string]Module, len(a.modules))
	for _, m := range a.modules {
		byName[m.Name()] = m
	}
	var enabled []Module
	for name := range strings.SplitSeq(a.cfg.target, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		m, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("appkit: -target 里的模块 %q 未注册（可用：%s）", name, moduleNames(a.modules))
		}
		enabled = append(enabled, m)
	}
	if len(enabled) == 0 {
		return nil, fmt.Errorf("appkit: -target=%q 未选中任何模块", a.cfg.target)
	}
	return enabled, nil
}

func moduleNames(ms []Module) string {
	names := make([]string, len(ms))
	for i, m := range ms {
		names[i] = m.Name()
	}
	return strings.Join(names, ", ")
}

// register 执行声明阶段：应用 Remote 绑定、逐个调用 Module.Register。
func (a *App) register(enabled []Module) error {
	for _, r := range a.cfg.remotes {
		r(a.reg)
	}
	for _, m := range enabled {
		if err := a.registerModule(m); err != nil {
			return err
		}
	}
	a.reg.current = ""
	return nil
}

func (a *App) registerModule(m Module) (err error) {
	a.reg.current = m.Name()
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("appkit: 模块 %q 的 Register panic: %v", m.Name(), p)
		}
	}()
	if err := m.Register(a.reg); err != nil {
		return fmt.Errorf("appkit: 模块 %q 注册失败: %w", m.Name(), err)
	}
	return nil
}
