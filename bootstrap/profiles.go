package bootstrap

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/authn"
	"github.com/forgeplex/appkit/config"
	"github.com/forgeplex/appkit/health"
	"github.com/forgeplex/appkit/httpserver"
	"github.com/forgeplex/appkit/pgmigrate"
	"github.com/forgeplex/appkit/pgtx"
	"github.com/forgeplex/appkit/telemetry"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultProbeAddr = "127.0.0.1:8081"
	probeModuleName  = "appkit-bootstrap-probe"
)

// Capabilities 是新 Bootstrap Profile 可用的进程能力。HTTP 指业务入站 HTTP；
// Probe 是独立的只读健康/就绪 Listener。Migrations 必须依赖 PostgreSQL。
type Capabilities struct {
	BusinessHTTP bool
	PostgreSQL   bool
	Bus          bool
	Migrations   bool
}

// ProbeOptions 配置独立的 Probe-only Listener。Addr 零值绑定到 loopback。
// 非 loopback 地址必须同时声明网络边界并提供认证中间件。
type ProbeOptions struct {
	Addr                     string
	NetworkBoundaryConfirmed bool
	Authenticate             func(http.Handler) http.Handler
}

// ProfileDeps 是新 Profile 模块装配时可用的能力快照；与旧 Deps.IsMinimal
// 区分，避免把正式 no-database Profile 误判为 -minimal。
type ProfileDeps struct {
	Log          *slog.Logger
	Base         Base
	Pool         *pgxpool.Pool
	Bus          EventBus
	Config       config.Options
	Capabilities Capabilities
}

// ProfileOptions 配置新的可组合 Bootstrap Core。旧 Options、RunOptions 和
// SecurityOptions 保持不变；旧完整 Profile 继续由 Main/RunWithSecurity 执行。
type ProfileOptions struct {
	Service                string
	EnvPrefix              string
	DefaultAddr            string
	ConfigFile             string
	Target                 string
	Modules                func(ProfileDeps) ([]appkit.Module, error)
	AppOptions             func(ProfileDeps) []appkit.Option
	Security               SecurityOptions
	AuthnPublicKey         ed25519.PublicKey
	AuthnIssuer            string
	NewBus                 func() EventBus
	AllowDirectBusForSplit bool
	PoolOptions            []pgtx.PoolOption
	Capabilities           Capabilities
	Probe                  *ProbeOptions
}

// Core 持有单个 Profile 的配置、日志、Telemetry 与可选基础设施。
// 每个 Core 仅可启动一次；Start 用于嵌入式调用，Execute 执行一次性受管
// 函数。Core 不安装 OS Signal Handler。
type Core struct {
	mu           sync.Mutex
	options      ProfileOptions
	deps         ProfileDeps
	securityMode appkit.SecurityMode
	telemetry    *telemetry.Telemetry
	pool         *pgxpool.Pool
	used         bool
	starting     bool
	closed       bool
	running      *RunningProfile
	cleanupOnce  sync.Once
	cleanupDone  chan struct{}
	cleanupErr   error
	cleanups     []cleanupHook
}

// Runner 是新 Bootstrap Profile 的进程级入口，负责唯一一次注册并桥接
// SIGINT/SIGTERM。Runner 与 Core 均为单次使用；嵌入式与 One-shot 调用不要使用它。
type Runner struct {
	mu      sync.Mutex
	options ProfileOptions
	used    bool
}

type cleanupHook struct {
	name string
	fn   func(context.Context) error
}

type redactedProfileError struct {
	message string
	cause   error
}

func (e redactedProfileError) Error() string { return e.message }
func (e redactedProfileError) Unwrap() error { return e.cause }

// RunningProfile 是 Bootstrap Core 启动的 Host。Wait 等待 Host 终态并关闭
// Core 资源；Shutdown 受 Context 预算约束，预算耗尽时后台继续完成资源清理。
type RunningProfile struct {
	core             *Core
	host             *appkit.RunningApp
	finishOnce       sync.Once
	finishWaiterOnce sync.Once
	done             chan struct{}
	result           error
}

// NewCore 加载通用配置并初始化日志/Telemetry，再按显式 capability 打开可选
// PostgreSQL 与 Bus。BusinessHTTP 关闭时不会加载或校验 HTTP SecurityMode。
func NewCore(ctx context.Context, options ProfileOptions) (*Core, error) {
	if ctx == nil {
		return nil, errors.New("bootstrap: Core Context 不能为空")
	}
	if strings.TrimSpace(options.Service) == "" {
		return nil, errors.New("bootstrap: ProfileOptions.Service 不能为空")
	}
	if options.Capabilities.Migrations && !options.Capabilities.PostgreSQL {
		return nil, errors.New("bootstrap: Migrations capability 需要 PostgreSQL capability")
	}
	if options.Probe != nil && options.Capabilities.BusinessHTTP {
		return nil, errors.New("bootstrap: Probe-only Listener 不能与业务 HTTP capability 同时启用")
	}
	if options.Probe != nil {
		probe, err := normalizeProbeOptions(*options.Probe)
		if err != nil {
			return nil, err
		}
		options.Probe = &probe
	}

	if options.ConfigFile == "" {
		options.ConfigFile = "config/dev.yaml"
	}
	if options.Target == "" {
		options.Target = "all"
	}
	copts := config.Options{
		Files:     []string{options.ConfigFile},
		EnvPrefix: cmp.Or(options.EnvPrefix, strings.ToUpper(options.Service)),
		Optional:  true,
	}
	base, err := config.Load[Base](copts)
	if err != nil {
		return nil, fmt.Errorf("%s: 加载配置: %w", options.Service, err)
	}
	base.Env = cmp.Or(base.Env, "dev")
	if options.Capabilities.BusinessHTTP {
		base.Addr = cmp.Or(base.Addr, options.DefaultAddr, ":8080")
	} else if base.Debug.Pprof {
		return nil, fmt.Errorf("%s: debug.pprof 需要 BusinessHTTP capability", options.Service)
	}

	var securityConfig runtimeSecurityConfig
	if options.Capabilities.BusinessHTTP {
		securityConfig, err = config.Load[runtimeSecurityConfig](copts)
		if err != nil {
			return nil, fmt.Errorf("%s: 加载 HTTP 安全配置: %w", options.Service, err)
		}
	}
	security := options.Security
	if options.Capabilities.BusinessHTTP && securityConfig.Security.Service != nil {
		if security.ServiceVerifier != nil {
			return nil, fmt.Errorf("%s: security.service 与 SecurityOptions.ServiceVerifier 不能同时配置", options.Service)
		}
		security.ServiceVerifier, err = securityConfig.Security.Service.verifier()
		if err != nil {
			return nil, fmt.Errorf("%s: security.service 配置无效: %w", options.Service, err)
		}
	}
	options.Security = security
	securityMode := securityConfig.Security.Mode
	legacyOptions := Options{
		Service:        options.Service,
		AuthnPublicKey: options.AuthnPublicKey,
		AuthnIssuer:    options.AuthnIssuer,
	}
	if options.Capabilities.BusinessHTTP {
		if err := validateHTTPSecurityWithOptions(legacyOptions, base.Env, securityMode, copts.EnvPrefix, false, security); err != nil {
			return nil, err
		}
	}
	if options.Capabilities.Bus && isSplitTarget(options.Target) && options.NewBus == nil && !options.AllowDirectBusForSplit {
		return nil, fmt.Errorf("%s: -target=%q 是拆分部署，禁止隐式使用进程内 DirectBus；请配置 NewBus 或明确允许拆分部署使用 DirectBus", options.Service, options.Target)
	}
	if options.Capabilities.PostgreSQL && strings.TrimSpace(base.Database.URL) == "" {
		return nil, fmt.Errorf("%s: PostgreSQL capability 需要 database.url", options.Service)
	}

	tel, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName: options.Service,
		Env:         base.Env,
		LogLevel:    base.Log.Level,
		LogFormat:   base.Log.Format,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: 初始化遥测: %w", options.Service, err)
	}
	core := &Core{
		options: options, securityMode: securityMode,
		telemetry: tel, cleanupDone: make(chan struct{}),
	}
	core.addCleanup("Telemetry", tel.Shutdown)
	if options.Capabilities.PostgreSQL {
		pool, err := pgtx.NewPool(ctx, base.Database.URL, options.PoolOptions...)
		if err != nil {
			safeErr := redactedProfileError{message: "PostgreSQL 初始化失败", cause: err}
			return nil, errors.Join(fmt.Errorf("%s: PostgreSQL 初始化失败；检查 database.url 与数据库可达性: %w", options.Service, safeErr), core.closeResources(context.Background()))
		}
		core.pool = pool
		core.addCleanup("PostgreSQL", func(context.Context) error {
			pool.Close()
			return nil
		})
	}
	var bus EventBus
	if options.Capabilities.Bus {
		bus = newBus(Options{
			NewBus:                 options.NewBus,
			AllowDirectBusForSplit: options.AllowDirectBusForSplit,
		})
		if isNilEventBus(bus) {
			return nil, errors.Join(fmt.Errorf("%s: Bus capability factory 返回 nil", options.Service), core.closeResources(context.Background()))
		}
	}

	profileBase := base
	if !options.Capabilities.PostgreSQL {
		profileBase.Database.URL = ""
	}
	capabilities := options.Capabilities
	core.deps = ProfileDeps{
		Log: tel.Logger, Base: profileBase, Pool: core.pool, Bus: bus,
		Config: copts, Capabilities: capabilities,
	}
	return core, nil
}

// Start 启动 Profile 并在 Host Ready 后返回；不注册 OS Signal Handler。
func (c *Core) Start(ctx context.Context) (*RunningProfile, error) {
	if c == nil {
		return nil, errors.New("bootstrap: Core 不能为空")
	}
	if ctx == nil {
		return nil, errors.New("bootstrap: Start Context 不能为空")
	}
	c.mu.Lock()
	if c.used {
		c.mu.Unlock()
		return nil, errors.New("bootstrap: Core 只能启动一次")
	}
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("bootstrap: Core 已关闭")
	}
	c.used, c.starting = true, true
	c.mu.Unlock()

	modules, err := c.modules()
	if err != nil {
		c.finishFailedStart()
		return nil, errors.Join(err, c.closeResources(context.Background()))
	}
	if err := validateProfileModules(modules); err != nil {
		c.finishFailedStart()
		return nil, errors.Join(err, c.closeResources(context.Background()))
	}
	if c.options.Probe != nil {
		for _, module := range modules {
			if module.Name() == probeModuleName {
				err = fmt.Errorf("bootstrap: 模块名 %q 为内部 Probe Profile 保留", probeModuleName)
				c.finishFailedStart()
				return nil, errors.Join(err, c.closeResources(context.Background()))
			}
		}
		modules = append(modules, probeModule{options: *c.options.Probe})
	}
	target := c.options.Target
	if c.options.Probe != nil {
		target = includeProbeTarget(target)
	}

	appOptions := []appkit.Option{appkit.Target(target), appkit.Logger(c.deps.Log)}
	if c.options.Capabilities.BusinessHTTP {
		appOptions = append(appOptions,
			appkit.HTTPAddr(c.deps.Base.Addr),
			appkit.Middleware(httpserver.Base(c.deps.Log)...),
		)
		if c.securityMode == appkit.SecurityUserFacing || c.securityMode == appkit.SecurityMixed {
			if len(c.options.Security.UserIssuers) != 0 {
				appOptions = append(appOptions, appkit.Middleware(authn.MultiIssuer(c.options.Security.UserIssuers)))
			} else {
				appOptions = append(appOptions, appkit.Middleware(authn.Middleware(c.options.AuthnPublicKey, c.options.AuthnIssuer)))
			}
		}
		if c.securityMode == appkit.SecurityInternalService || c.securityMode == appkit.SecurityMixed {
			appOptions = append(appOptions, appkit.Middleware(c.options.Security.ServiceVerifier.Middleware))
		}
		if c.deps.Base.Debug.Pprof {
			appOptions = append(appOptions, appkit.Pprof())
		}
	}
	if c.options.AppOptions != nil {
		appOptions = append(appOptions, c.options.AppOptions(c.deps)...)
	}
	// Profile capability guards are deliberately last: AppOptions cannot re-enable
	// a disabled listener, Bus, or migration runner.
	appOptions = append(appOptions, appkit.Target(target), appkit.Logger(c.deps.Log))
	if c.options.Capabilities.BusinessHTTP {
		appOptions = append(appOptions, appkit.Security(c.securityMode))
	} else {
		appOptions = append(appOptions, appkit.Headless())
	}
	if c.options.Capabilities.Bus {
		appOptions = append(appOptions, appkit.Bus(c.deps.Bus))
	} else {
		appOptions = append(appOptions, appkit.Bus(nil))
	}
	if c.options.Capabilities.Migrations {
		appOptions = append(appOptions, appkit.Migrator(pgmigrate.Runner(c.pool)))
	} else {
		appOptions = append(appOptions, appkit.DisableMigrations())
	}

	app := appkit.New(modules, appOptions...)
	host, err := app.Start(ctx)
	if err != nil {
		c.finishFailedStart()
		return nil, errors.Join(fmt.Errorf("bootstrap: 启动 Profile 失败: %w", err), c.closeResources(context.Background()))
	}
	running := &RunningProfile{core: c, host: host, done: make(chan struct{})}
	c.mu.Lock()
	c.starting = false
	c.running = running
	c.mu.Unlock()
	return running, nil
}

// NewRunner 创建一个单次使用的进程级入口。进程级程序应由它调用 Core.Start，
// 不要再把其内部 Host 交给 App.Run 或安装第二个框架 Signal Handler。
func NewRunner(options ProfileOptions) *Runner {
	return &Runner{options: options}
}

// Run 注册一次 SIGINT/SIGTERM Handler 并桥接到 Host。Core 本身不接管进程信号。
func (r *Runner) Run(ctx context.Context) error {
	if r == nil {
		return errors.New("bootstrap: Runner 不能为空")
	}
	if ctx == nil {
		return errors.New("bootstrap: Runner Context 不能为空")
	}
	r.mu.Lock()
	if r.used {
		r.mu.Unlock()
		return errors.New("bootstrap: Runner 只能运行一次")
	}
	r.used = true
	r.mu.Unlock()

	signalCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	core, err := NewCore(signalCtx, r.options)
	if err != nil {
		return err
	}
	running, err := core.Start(signalCtx)
	if err != nil {
		return err
	}
	return running.Wait()
}

// Execute 以无监听、无 OS Signal 的 One-shot Profile 启动 App，执行 fn 后
// 优雅关停。fn 的错误与关停/Telemetry 错误通过 errors.Join 保留。
func (c *Core) Execute(ctx context.Context, fn func(context.Context, ProfileDeps) error) error {
	if c == nil {
		return errors.New("bootstrap: Core 不能为空")
	}
	if ctx == nil {
		return errors.New("bootstrap: Execute Context 不能为空")
	}
	if fn == nil {
		return errors.New("bootstrap: One-shot 函数不能为空")
	}
	if c.options.Capabilities.BusinessHTTP || c.options.Probe != nil {
		return errors.New("bootstrap: One-shot Profile 不支持业务 HTTP 或 Probe Listener")
	}
	running, err := c.Start(ctx)
	if err != nil {
		return err
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	defer func() { _ = running.Shutdown(shutdownCtx) }()
	fnErr := fn(ctx, c.deps)
	return errors.Join(fnErr, running.Shutdown(shutdownCtx))
}

// Close 关闭尚未启动的 Core；若 Host 已启动，则先请求优雅关停。Start 期间
// 不允许并发 Close，避免资源在装配中途被释放。
func (c *Core) Close(ctx context.Context) error {
	if c == nil {
		return errors.New("bootstrap: Core 不能为空")
	}
	if ctx == nil {
		return errors.New("bootstrap: Close Context 不能为空")
	}
	c.mu.Lock()
	if c.starting {
		c.mu.Unlock()
		return errors.New("bootstrap: Core 正在启动")
	}
	running := c.running
	c.mu.Unlock()
	if running != nil {
		return running.Shutdown(ctx)
	}
	return c.closeResources(ctx)
}

// Readiness 返回 Host 的程序化就绪检查结果，不要求开启 HTTP。
func (r *RunningProfile) Readiness(ctx context.Context) (map[string]error, error) {
	if r == nil || r.host == nil {
		return nil, errors.New("bootstrap: RunningProfile 不能为空")
	}
	return r.host.Readiness(ctx)
}

// Wait 等待 Host 终止并关闭 Core 资源。
func (r *RunningProfile) Wait() error {
	if r == nil || r.host == nil {
		return errors.New("bootstrap: RunningProfile 不能为空")
	}
	return r.finish(r.host.Wait())
}

// Shutdown 请求 Host 关停；ctx 限制本次等待预算。若 Host 在预算耗尽后仍继续
// 清理，本方法及时返回，并在后台等 Host 终止后关闭连接池与 flush Telemetry；
// 后续 Wait 仍可读取最终运行/清理错误。
func (r *RunningProfile) Shutdown(ctx context.Context) error {
	if r == nil || r.host == nil {
		return errors.New("bootstrap: RunningProfile 不能为空")
	}
	if ctx == nil {
		return errors.New("bootstrap: Shutdown Context 不能为空")
	}
	requestErr := r.host.Shutdown(ctx)
	if requestErr != nil && ctx.Err() != nil {
		r.finishWaiterOnce.Do(func() {
			go func() { _ = r.finish(r.host.Wait()) }()
		})
		return requestErr
	}
	return r.finish(r.host.Wait())
}

func (r *RunningProfile) finish(hostErr error) error {
	r.finishOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), telShutdownTimeout)
		defer cancel()
		r.result = errors.Join(hostErr, r.core.closeResources(ctx))
		r.core.mu.Lock()
		r.core.closed = true
		r.core.mu.Unlock()
		close(r.done)
	})
	<-r.done
	return r.result
}

func (c *Core) modules() ([]appkit.Module, error) {
	if c.options.Modules == nil {
		return nil, nil
	}
	modules, err := c.options.Modules(c.deps)
	if err != nil {
		return nil, fmt.Errorf("%s: 装配 Profile 模块: %w", c.options.Service, err)
	}
	return modules, nil
}

func (c *Core) finishFailedStart() {
	c.mu.Lock()
	c.starting = false
	c.closed = true
	c.mu.Unlock()
}

func (c *Core) closeResources(ctx context.Context) error {
	c.cleanupOnce.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, telShutdownTimeout)
			defer cancel()
		}
		var errs []error
		for i := len(c.cleanups) - 1; i >= 0; i-- {
			hook := c.cleanups[i]
			if err := hook.fn(ctx); err != nil {
				errs = append(errs, fmt.Errorf("bootstrap: 关闭 %s: %w", hook.name, err))
			}
		}
		c.cleanupErr = errors.Join(errs...)
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		close(c.cleanupDone)
	})
	<-c.cleanupDone
	return c.cleanupErr
}

func (c *Core) addCleanup(name string, fn func(context.Context) error) {
	c.cleanups = append(c.cleanups, cleanupHook{name: name, fn: fn})
}

func validateProfileModules(modules []appkit.Module) error {
	seen := make(map[string]struct{}, len(modules))
	for i, module := range modules {
		if isNilAppModule(module) {
			return fmt.Errorf("bootstrap: Profile 模块索引 %d 为 nil", i)
		}
		name := strings.TrimSpace(module.Name())
		if name == "" {
			return fmt.Errorf("bootstrap: Profile 模块索引 %d 的 Name 不能为空", i)
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("bootstrap: Profile 重复声明模块 %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func isNilAppModule(module appkit.Module) bool {
	if module == nil {
		return true
	}
	value := reflect.ValueOf(module)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func isNilEventBus(bus EventBus) bool {
	if bus == nil {
		return true
	}
	value := reflect.ValueOf(bus)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func includeProbeTarget(target string) string {
	if !isSplitTarget(target) {
		return target
	}
	for _, name := range strings.Split(target, ",") {
		if strings.TrimSpace(name) == probeModuleName {
			return target
		}
	}
	return target + "," + probeModuleName
}

func normalizeProbeOptions(options ProbeOptions) (ProbeOptions, error) {
	options.Addr = cmp.Or(options.Addr, defaultProbeAddr)
	host, _, err := net.SplitHostPort(options.Addr)
	if err != nil {
		return ProbeOptions{}, fmt.Errorf("bootstrap: Probe Addr 无效: %w", err)
	}
	if isLoopbackHost(host) {
		return options, nil
	}
	if !options.NetworkBoundaryConfirmed || options.Authenticate == nil {
		return ProbeOptions{}, errors.New("bootstrap: 非 loopback Probe Listener 必须显式确认网络边界并提供认证中间件")
	}
	return options, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	host, _, _ = strings.Cut(host, "%")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type probeModule struct{ options ProbeOptions }

func (probeModule) Name() string { return probeModuleName }

func (m probeModule) Register(reg *appkit.Registry) error {
	return reg.ManagedService("listener", appkit.ServiceCritical, func(reg *appkit.Registry) (appkit.ManagedService, error) {
		handler := newProbeHandler(reg.HealthRegistry())
		if m.options.Authenticate != nil {
			handler = m.options.Authenticate(handler)
			if handler == nil {
				return nil, errors.New("Probe Authenticate 返回 nil Handler")
			}
		}
		return &probeService{
			addr:   m.options.Addr,
			server: &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second},
		}, nil
	})
}

func newProbeHandler(reg *health.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/healthz", exactProbePath("/healthz", reg.LiveHandler()))
	mux.Handle("/readyz", exactProbePath("/readyz", reg.ReadyHandler()))
	return mux
}

func exactProbePath(path string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type probeService struct {
	addr   string
	server *http.Server
	ln     net.Listener
}

func (s *probeService) Start(context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("bootstrap: 监听 Probe 地址 %s: %w", s.addr, err)
	}
	s.ln = ln
	return nil
}

func (s *probeService) Run(context.Context) error {
	if s.ln == nil {
		return errors.New("Probe Listener 未启动")
	}
	err := s.server.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *probeService) Ready(context.Context) error {
	if s.ln == nil {
		return errors.New("Probe Listener 未绑定")
	}
	return nil
}

func (s *probeService) Drain(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

func (s *probeService) Close(context.Context) error {
	if s.server == nil {
		return nil
	}
	err := s.server.Close()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
