package appkit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/forgeplex/appkit/health"
	"github.com/forgeplex/appkit/internal/hoststate"
)

// Run 启动应用并阻塞到 ctx 取消、收到 SIGINT/SIGTERM、HTTP 服务异常退出，
// 或关键受管任务异常退出，然后优雅关停。启用业务 HTTP 时必须通过 Security
// 显式选择 HTTP 安全模式；Headless 应用不要求该模式。
//
// 启动顺序：Register（声明）→ Remote 绑定 → 依赖图解析（fail-fast）→ 迁移 →
// Setup（装配）→ 消费者订阅 Bus → OnStart 按 stage 升序（含 HTTP Listener）→
// ManagedService Start/Run/Ready → 全局 Ready。就绪后阻塞在关停信号、HTTP、Worker 或 Critical
// ManagedService 退出上。关停时 readyz 先置 503，HTTP 开始 Shutdown；ManagedService
// 反序 Drain 后取消其 Run Context、等待所有 Run、反序 Close，最后 OnStop 按启动
// 逆序（stage 降序、同 stage 注册逆序）。启动中途失败时，先取消普通运行 ctx，
// ManagedService 仅回滚已调用 Start 的实例，再只对已开始的 stage 执行 OnStop。
func (a *App) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	host, err := a.start(ctx, "Run")
	if err != nil {
		return err
	}
	return host.Wait()
}

func (a *App) runHost(host *RunningApp) error {
	ctx := host.ctx
	log := a.cfg.logger
	a.reg.health.SetLogger(log)
	host.setState(hostRegistering)

	enabled, err := a.enabledModules()
	if err != nil {
		return err
	}
	log.Info("appkit: 启动", "target", a.cfg.target, "modules", moduleNames(enabled))
	if err := a.register(enabled); err != nil {
		return err
	}
	if a.cfg.httpEnabled {
		if err := validateSecurityMode(a.cfg.securityMode); err != nil {
			return err
		}
	}
	cancelBus := a.registerBusLifecycle(ctx)
	defer cancelBus()

	host.setState(hostResolving)
	if err := a.reg.resolveAll(); err != nil {
		return err
	}
	host.setState(hostMigrating)
	if err := a.migrate(ctx); err != nil {
		return err
	}
	host.setState(hostSettingUp)
	if err := a.reg.runSetups(ctx); err != nil {
		return err
	}
	if err := a.resolveManagedServices(host); err != nil {
		return err
	}
	// Setup 也允许挂路由，因此校验必须发生在 buildMux/listen 之前。
	if a.cfg.httpEnabled {
		if err := a.reg.validateRouteSecurity(a.cfg.securityMode, a.cfg.pprof); err != nil {
			return err
		}
	} else if err := a.validateHeadlessRoutes(); err != nil {
		return err
	}
	if err := a.reg.validatePermBindings(); err != nil {
		return err
	}
	if err := a.subscribeConsumers(); err != nil {
		return err
	}

	var server *http.Server
	if a.cfg.httpEnabled {
		mux, err := a.buildMux()
		if err != nil {
			return err
		}
		server = a.buildServer(a.wrap(mux))
	}

	host.setState(hostStarting)
	maxStage, startErr := a.startHooks(ctx, server)
	if startErr == nil {
		startErr = host.startManagedServices(ctx)
	}
	if startErr == nil {
		select {
		case startErr = <-host.serviceErr:
		default:
		}
	}
	if startErr != nil {
		log.Error("appkit: 启动失败，进入关停", "err", startErr)
		host.draining.Store(true)
		host.setState(hostStopping)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), a.cfg.shutdownTimeout)
		shutdownErr := a.shutdown(server, maxStage, host, cleanupCtx)
		cancel()
		return errors.Join(startErr, shutdownErr)
	}

	a.reg.health.SetReady(true)
	if server != nil {
		log.Info("appkit: 就绪", "addr", a.cfg.httpAddr)
	} else {
		log.Info("appkit: 就绪", "profile", "headless")
	}
	host.markReady()

	var triggerErr error
	cleanupParent := context.Background()
	select {
	case <-ctx.Done():
		log.Info("appkit: 收到关停信号")
	case err := <-a.runtime.serverErr:
		triggerErr = fmt.Errorf("appkit: HTTP 服务异常退出: %w", err)
		log.Error("appkit: HTTP 服务异常退出，进入关停", "err", err)
	case err := <-a.reg.runtime.workerErr:
		triggerErr = err
		log.Error("appkit: 后台 worker 异常退出，进入关停", "err", err)
	case err := <-host.serviceErr:
		triggerErr = err
		log.Error("appkit: Critical ManagedService 异常退出，进入关停", "err", err)
	case request := <-host.stop:
		cleanupParent = request.ctx
	}

	host.draining.Store(true)
	host.setState(hostDraining)
	cleanupCtx, cancel := context.WithTimeout(cleanupParent, a.cfg.shutdownTimeout)
	shutdownErr := a.shutdown(server, maxStage, host, cleanupCtx)
	cancel()
	return errors.Join(triggerErr, shutdownErr, errors.Join(host.serviceExitErrorsSnapshot()...))
}

// registerBusLifecycle 把可选的持久化 Broker 生命周期纳入 App 的标准启动、
// readiness、异常传播与反序关停。普通进程内 Subscriber 保持原行为。
func (a *App) registerBusLifecycle(ctx context.Context) context.CancelFunc {
	bus, ok := a.cfg.bus.(ManagedSubscriber)
	if !ok {
		return func() {}
	}
	busCtx, cancelBus := context.WithCancel(context.WithoutCancel(ctx))
	previous := a.reg.current
	a.reg.current = "appkit-bus"
	defer func() { a.reg.current = previous }()

	a.reg.Health("ready", health.CheckFunc(bus.Ready))
	connectAttempted := false
	a.reg.OnStart(StageInfra, func(ctx context.Context) error {
		connectAttempted = true
		return bus.Connect(ctx)
	})
	// Close 在 Worker 启动钩子登记前绑定 Infra stage：Connect 即使只完成部分
	// 初始化便失败，shutdown 也会执行 Close；正常关停时它排在 Worker stage 后。
	a.reg.OnStop(func(ctx context.Context) error {
		if !connectAttempted {
			return nil
		}
		return bus.Close(ctx)
	})

	// Worker 仍由 Registry 托管错误传播和等待，但忽略普通 runCtx，改用独立
	// busCtx；因此 App 取消普通 worker 后，Broker 仍可继续处理在途消息。
	worker := a.reg.worker("consume", func(context.Context) error {
		return bus.Run(busCtx)
	})
	// 同一 stage 的 stop 逆序执行：Drain → cancelBus → Worker 等待。
	a.reg.OnStop(func(context.Context) error {
		if !worker.started {
			return nil
		}
		cancelBus()
		return nil
	})
	a.reg.OnStop(func(ctx context.Context) error {
		if !worker.started {
			return nil
		}
		return bus.Drain(ctx)
	})
	return cancelBus
}

// Migrate 只做「声明 → 应用迁移」然后返回：不解析依赖图、不跑 Setup/OnStart、
// 不监听端口。供部署的前置步骤使用（K8s initContainer 或 Job）——多副本滚动
// 更新时先由一个 Job 把 schema 迁到位，服务副本再带 SkipMigrations 起来，
// 避免 N 个副本同时改 schema。
//
// 迁移清单与 Run 用的是同一份模块声明，不存在「迁移用的清单和服务用的不是
// 同一份」这种漂移。必须注入 Migrator，否则报错（此处 SkipMigrations 无意义）。
func (a *App) Migrate(ctx context.Context) error {
	if err := a.claimUse("Migrate"); err != nil {
		return err
	}
	enabled, err := a.enabledModules()
	if err != nil {
		return err
	}
	if err := a.register(enabled); err != nil {
		return err
	}
	if len(a.reg.migrations) == 0 {
		a.cfg.logger.Info("appkit: 无迁移可应用", "target", a.cfg.target)
		return nil
	}
	if a.cfg.disableMigrations {
		return fmt.Errorf("appkit: Profile 未启用迁移 capability，但模块 %q 声明了迁移", a.reg.migrations[0].Module)
	}
	if a.cfg.migrator == nil {
		return fmt.Errorf("appkit: Migrate 需要迁移执行器：注入 appkit.Migrator(pgmigrate.Runner(pool))")
	}
	a.cfg.logger.Info("appkit: 应用迁移", "target", a.cfg.target, "sets", len(a.reg.migrations))
	if err := a.cfg.migrator(ctx, a.reg.migrations); err != nil {
		return fmt.Errorf("appkit: 迁移失败: %w", err)
	}
	a.cfg.logger.Info("appkit: 迁移完成")
	return nil
}

// migrate 应用全部已声明迁移。登记了迁移却没有执行器属装配错误，fail-fast——
// 静默跳过迁移＝服务对着旧 schema 跑，症状要到第一条查询才出现，且长得像业务 bug。
// 迁移确实由进程外施加（K8s initContainer 跑 appkit migrate）时，
// 用 SkipMigrations 显式声明。
func (a *App) migrate(ctx context.Context) error {
	sets := a.reg.migrations
	if len(sets) == 0 {
		return nil
	}
	if a.cfg.disableMigrations {
		return fmt.Errorf("appkit: Profile 未启用迁移 capability，但模块 %q 声明了迁移", sets[0].Module)
	}
	if a.cfg.skipMigrations {
		a.cfg.logger.Info("appkit: 跳过迁移（SkipMigrations）", "sets", len(sets))
		return nil
	}
	if a.cfg.migrator == nil {
		return fmt.Errorf("appkit: 有 %d 个迁移集待应用（如模块 %q 的 schema %q）但未注入迁移执行器："+
			"注入 appkit.Migrator(pgmigrate.Runner(pool))，或以 appkit.SkipMigrations() 声明由进程外施加",
			len(sets), sets[0].Module, sets[0].Schema)
	}
	if err := a.cfg.migrator(ctx, sets); err != nil {
		return fmt.Errorf("appkit: 迁移失败: %w", err)
	}
	return nil
}

// subscribeConsumers 把 Registry.Consumer 登记的消费者逐条订阅到 Bus。
// 有消费者却未注入 Bus 属装配错误，fail-fast。
func (a *App) subscribeConsumers() error {
	if len(a.reg.consumers) == 0 {
		return nil
	}
	if a.cfg.bus == nil {
		c := a.reg.consumers[0]
		return fmt.Errorf("appkit: 登记了 %d 个事件消费者（如模块 %q 的 topic %q）但未通过 appkit.Bus 注入订阅端",
			len(a.reg.consumers), c.Module, c.Topic)
	}
	for _, c := range a.reg.consumers {
		a.cfg.bus.Subscribe(c.Topic, c.Handler)
	}
	return nil
}

func (a *App) validateHeadlessRoutes() error {
	if a.cfg.pprof {
		return errors.New("appkit: Headless 不支持 pprof；请启用业务 HTTP 或独立的受控诊断 Listener")
	}
	if len(a.reg.mounts) != 0 {
		route := a.reg.mounts[0]
		return fmt.Errorf("appkit: Headless 不支持 HTTP 路由 %q（模块 %q）", route.pattern, route.module)
	}
	return nil
}

// buildMux 组装根路由：探针 + 各模块 Mount。重复 pattern 转为启动错误。
func (a *App) buildMux() (mux *http.ServeMux, err error) {
	mux = http.NewServeMux()
	mux.Handle("/healthz", a.reg.health.LiveHandler())
	mux.Handle("/readyz", a.reg.health.ReadyHandler())
	if a.cfg.pprof {
		guard := func(h http.Handler) http.Handler { return h }
		if a.cfg.securityMode == SecurityInternalService || a.cfg.securityMode == SecurityMixed {
			guard = requireService
		}
		mountPprof(mux, guard)
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("appkit: 路由挂载失败: %v", p)
		}
	}()
	for _, m := range a.reg.mounts {
		mux.Handle(m.pattern, m.handler)
	}
	return mux, nil
}

// mountPprof 挂标准 pprof 端点集。显式列举而非 import 副作用挂
// DefaultServeMux：路由必须落在应用自己的 mux 上，与探针同级。
func mountPprof(mux *http.ServeMux, guard func(http.Handler) http.Handler) {
	mux.Handle("GET /debug/pprof/", guard(http.HandlerFunc(pprof.Index)))
	mux.Handle("GET /debug/pprof/cmdline", guard(http.HandlerFunc(pprof.Cmdline)))
	mux.Handle("GET /debug/pprof/profile", guard(http.HandlerFunc(pprof.Profile)))
	mux.Handle("GET /debug/pprof/symbol", guard(http.HandlerFunc(pprof.Symbol)))
	mux.Handle("GET /debug/pprof/trace", guard(http.HandlerFunc(pprof.Trace)))
	for _, name := range []string{"goroutine", "heap", "allocs", "block", "mutex", "threadcreate"} {
		mux.Handle("GET /debug/pprof/"+name, guard(pprof.Handler(name)))
	}
}

// buildServer 构造 http.Server：安全默认超时（防慢客户端占死连接），
// 再应用 HTTPServer 选项覆盖。
func (a *App) buildServer(h http.Handler) *http.Server {
	server := &http.Server{
		Addr:              a.cfg.httpAddr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	for _, f := range a.cfg.httpServerOpts {
		f(server)
	}
	if a.cfg.securityMode != SecurityDisabled {
		// HTTPServer 是超时等传输参数的扩展点，不是替换根 handler 的逃生口。
		// 严格模式强制恢复，防止绕过路由分类、guard 与身份边界；disabled
		// 保留历史逃生口，避免 dev/test 兼容模式出现无关行为变化。
		server.Handler = h
	}
	return server
}

func (a *App) wrap(h http.Handler) http.Handler {
	for i := len(a.cfg.middleware) - 1; i >= 0; i-- {
		h = a.cfg.middleware[i](h)
	}
	if a.cfg.securityMode != SecurityDisabled {
		// 放在所有可注入中间件之外：调用方无法通过中间件顺序让 unsigned
		// identity header 或预置 ctx 绕过信任边界；验签发生在边界内并重建。
		h = identityBoundary(h)
	}
	return h
}

// stageNone 表示尚无任何启动钩子开始执行。
const stageNone = math.MinInt

// startHooks 按 (stage, 注册序) 执行启动钩子；HTTP 启用时在 StageServer 插入监听。
// 返回实际开始执行过的最高 stage（关停时更高 stage 的 OnStop 会被跳过）。
func (a *App) startHooks(ctx context.Context, server *http.Server) (maxStage int, err error) {
	maxStage = stageNone
	hooks := make([]startHook, len(a.reg.starts))
	copy(hooks, a.reg.starts)
	if server != nil {
		hooks = append(hooks, startHook{
			stage:  StageServer,
			seq:    len(hooks),
			module: "appkit",
			fn:     func(context.Context) error { return a.listen(server) },
		})
	}
	sort.SliceStable(hooks, func(i, j int) bool {
		if hooks[i].stage != hooks[j].stage {
			return hooks[i].stage < hooks[j].stage
		}
		return hooks[i].seq < hooks[j].seq
	})
	for _, h := range hooks {
		if ctx.Err() != nil {
			return maxStage, ctx.Err()
		}
		maxStage = h.stage
		if e := h.fn(ctx); e != nil {
			return maxStage, fmt.Errorf("appkit: 模块 %q 启动失败: %w", h.module, e)
		}
	}
	return maxStage, nil
}

// listen 同步绑定端口——绑定失败确定性地成为启动错误；随后异步 Serve，
// 运行期错误写入带缓冲的 serverErr，由 Run 的阻塞点消费。
func (a *App) listen(server *http.Server) error {
	ln, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("appkit: 监听 %s: %w", server.Addr, err)
	}
	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.runtime.serverErr <- err
		}
	}()
	return nil
}

// shutdown 执行优雅关停：撤销 readiness；HTTP Shutdown 与 ManagedService Drain
// 并行开始；反序 Drain 全部服务后取消 Service Run Context、等待退出、反序 Close，
// 最后 OnStop 按启动逆序。ctx 是总关停预算；maxStartedStage 之上的 OnStop 跳过。
func (a *App) shutdown(server *http.Server, maxStartedStage int, host *RunningApp, ctx context.Context) error {
	a.reg.health.SetReady(false)
	// 先撤销 readiness，再取消普通 Worker Context；ManagedService 使用独立
	// Run Context，保持到全部 Drain 完成后才由 cancelServices 取消。
	host.cancelRun()
	var errs []error
	if a.cfg.drainDelay > 0 {
		timer := time.NewTimer(a.cfg.drainDelay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			errs = append(errs, fmt.Errorf("appkit: 摘流延迟耗尽关停预算: %w", ctx.Err()))
		}
	}

	// Shutdown 立即关闭 Listener/新连接，同时与 ManagedService Drain 并行等待
	// 在途 HTTP 请求；Drain 时 Service Run Context 仍保持有效。Headless 没有
	// appkit HTTP server，仍执行服务与 OnStop 清理。
	var serverDone chan error
	if server != nil {
		serverDone = make(chan error, 1)
		go func() { serverDone <- server.Shutdown(ctx) }()
	}

	for i := len(host.services) - 1; i >= 0; i-- {
		service := host.services[i]
		if !service.runStarted {
			if !service.startAttempted.Load() {
				service.setLifecycleState(hoststate.StateNotStarted)
			}
			continue
		}
		service.setLifecycleState(hoststate.StateDraining)
		if err := runManagedServicePhase(ctx, service, "Drain", service.service.Drain); err != nil {
			service.setLifecycleState(hoststate.StateFailed)
			errs = append(errs, err)
		}
	}
	host.setState(hostStopping)
	for _, service := range host.services {
		if service.startAttempted.Load() {
			service.setLifecycleState(hoststate.StateStopping)
		}
	}
	host.cancelServices()
	for i := len(host.services) - 1; i >= 0; i-- {
		service := host.services[i]
		if service.runStarted {
			if err := waitManagedServiceRun(ctx, service); err != nil {
				service.setLifecycleState(hoststate.StateFailed)
				errs = append(errs, err)
			}
		}
	}
	for i := len(host.services) - 1; i >= 0; i-- {
		service := host.services[i]
		if service.startAttempted.Load() {
			if err := runManagedServicePhase(ctx, service, "Close", service.service.Close); err != nil {
				service.setLifecycleState(hoststate.StateFailed)
				errs = append(errs, err)
			} else {
				service.setLifecycleState(hoststate.StateStopped)
			}
		}
	}

	if server != nil {
		select {
		case err := <-serverDone:
			if err != nil {
				errs = append(errs, fmt.Errorf("appkit: HTTP 关停: %w", err))
				if closeErr := server.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
					errs = append(errs, fmt.Errorf("appkit: HTTP 强制关闭: %w", closeErr))
				}
			}
		case <-ctx.Done():
			if closeErr := server.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
				errs = append(errs, fmt.Errorf("appkit: HTTP 强制关闭: %w", closeErr))
			}
			if err := <-serverDone; err != nil {
				errs = append(errs, fmt.Errorf("appkit: HTTP 关停: %w", err))
			}
		}
	}

	stops := make([]stopHook, len(a.reg.stops))
	copy(stops, a.reg.stops)
	// 镜像启动顺序：stage 降序，同 stage 注册逆序。
	sort.SliceStable(stops, func(i, j int) bool {
		if stops[i].stage != stops[j].stage {
			return stops[i].stage > stops[j].stage
		}
		return stops[i].seq > stops[j].seq
	})
	for _, s := range stops {
		if s.stage > maxStartedStage {
			continue
		}
		if err := a.runStop(ctx, s); err != nil {
			errs = append(errs, err)
		}
	}
	if budgetErr := ctx.Err(); budgetErr != nil && !errors.Is(errors.Join(errs...), budgetErr) {
		errs = append(errs, fmt.Errorf("appkit: 关停预算耗尽: %w", budgetErr))
	}
	return errors.Join(errs...)
}

func waitManagedServiceRun(ctx context.Context, service *managedServiceRuntime) error {
	select {
	case <-service.done:
		err := service.result()
		if err == nil || errors.Is(err, context.Canceled) || service.unexpected.Load() {
			return nil
		}
		return fmt.Errorf("appkit: 模块 %q ManagedService %q Run 关停失败: %w", service.reg.module, service.reg.name, err)
	case <-ctx.Done():
		return fmt.Errorf("appkit: 模块 %q ManagedService %q Run 未在关停预算内退出: %w", service.reg.module, service.reg.name, ctx.Err())
	}
}

// runStop 在独立 goroutine 里执行单个 OnStop，超出关停预算即放弃等待、
// 记录错误并继续下一个（卡死的钩子不能拖垮整个关停）。
func (a *App) runStop(sctx context.Context, s stopHook) error {
	done := make(chan error, 1)
	go func() { done <- s.fn(sctx) }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("appkit: 模块 %q 关停: %w", s.module, err)
		}
		return nil
	case <-sctx.Done():
		return fmt.Errorf("appkit: 模块 %q 关停超时", s.module)
	}
}
