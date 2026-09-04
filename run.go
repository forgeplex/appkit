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
)

// Run 启动应用并阻塞到 ctx 取消、收到 SIGINT/SIGTERM 或 HTTP 服务异常退出，
// 然后优雅关停。
//
// 启动顺序：Register（声明）→ Remote 绑定 → 依赖图解析（fail-fast）→ 迁移 →
// Setup（装配）→ 消费者订阅 Bus → OnStart 按 stage 升序 → HTTP 监听 → 置 ready。
// 就绪后阻塞在三件事上：关停信号、HTTP 服务异常退出、长驻 Worker 异常退出。
// 关停顺序：readyz 置 503 摘流量 → drain 等待 → HTTP Shutdown → OnStop 按启动
// 逆序（stage 降序、同 stage 注册逆序）。启动中途失败时，先取消启动钩子派生的
// ctx（叫停已启动的 worker），再只对实际启动过的 stage 执行 OnStop。
func (a *App) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	log := a.cfg.logger
	// 健康检查失败详情走 App 统一 logger，而非 slog.Default()。
	a.reg.health.SetLogger(log)

	enabled, err := a.enabledModules()
	if err != nil {
		return err
	}
	log.Info("appkit: 启动", "target", a.cfg.target, "modules", moduleNames(enabled))

	if err := a.register(enabled); err != nil {
		return err
	}
	a.registerBusLifecycle()
	if err := a.reg.resolveAll(); err != nil {
		return err
	}
	if err := a.migrate(ctx); err != nil {
		return err
	}
	if err := a.reg.runSetups(ctx); err != nil {
		return err
	}
	// 全部 Setup 之后统一校验权限绑定 ⊆ 声明——模块内部 mux 在 Setup 期
	// 绑的码也要覆盖；拼错的码在监听之前曝光，而不是等到运行时 403。
	if err := a.reg.validatePermBindings(); err != nil {
		return err
	}
	// 消费者装配放在全部 Setup 之后：Register 与 Setup 阶段登记的都会生效。
	if err := a.subscribeConsumers(); err != nil {
		return err
	}

	mux, err := a.buildMux()
	if err != nil {
		return err
	}
	server := a.buildServer(a.wrap(mux))

	// runCtx 覆盖全部启动钩子（含其派生的 worker goroutine）。进入关停前
	// 无条件取消：启动失败路径也必须叫停已启动的 worker，否则等它退出的
	// OnStop 会永久阻塞。
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	// 启动钩子：模块钩子 + 框架的 HTTP 监听（StageServer）。
	maxStage, startErr := a.startHooks(runCtx, server)

	var serveErr error
	if startErr == nil {
		a.reg.health.SetReady(true)
		log.Info("appkit: 就绪", "addr", a.cfg.httpAddr)
		select {
		case <-ctx.Done():
			log.Info("appkit: 收到关停信号")
		case err := <-a.serverErr:
			serveErr = fmt.Errorf("appkit: HTTP 服务异常退出: %w", err)
			log.Error("appkit: HTTP 服务异常退出，进入关停", "err", err)
		case err := <-a.reg.workerErr:
			// 长驻 worker 死了而进程还活着 = 探针绿着但事件停摆。同 HTTP 异常退出处理。
			serveErr = err
			log.Error("appkit: 后台 worker 异常退出，进入关停", "err", err)
		}
	} else {
		log.Error("appkit: 启动失败，进入关停", "err", startErr)
	}

	cancelRun()
	shutdownErr := a.shutdown(server, maxStage)
	return errors.Join(startErr, serveErr, shutdownErr)
}

// registerBusLifecycle 把可选的持久化 Broker 生命周期纳入 App 的标准启动、
// readiness、异常传播与反序关停。普通进程内 Subscriber 保持原行为。
func (a *App) registerBusLifecycle() {
	bus, ok := a.cfg.bus.(ManagedSubscriber)
	if !ok {
		return
	}
	previous := a.reg.current
	a.reg.current = "appkit-bus"
	defer func() { a.reg.current = previous }()

	a.reg.Health("ready", health.CheckFunc(bus.Ready))
	a.reg.OnStart(StageInfra, bus.Connect)
	// Close 先登记在 Infra stage；反序关停时它会在 Drain 和 Worker 退出之后执行。
	a.reg.OnStop(bus.Close)
	a.reg.Worker("consume", bus.Run)
	// 同 stage 逆序执行，所以 Drain 先于 Worker 的等待钩子。
	a.reg.OnStop(bus.Drain)
}

// Migrate 只做「声明 → 应用迁移」然后返回：不解析依赖图、不跑 Setup/OnStart、
// 不监听端口。供部署的前置步骤使用（K8s initContainer 或 Job）——多副本滚动
// 更新时先由一个 Job 把 schema 迁到位，服务副本再带 SkipMigrations 起来，
// 避免 N 个副本同时改 schema。
//
// 迁移清单与 Run 用的是同一份模块声明，不存在「迁移用的清单和服务用的不是
// 同一份」这种漂移。必须注入 Migrator，否则报错（此处 SkipMigrations 无意义）。
func (a *App) Migrate(ctx context.Context) error {
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

// buildMux 组装根路由：探针 + 各模块 Mount。重复 pattern 转为启动错误。
func (a *App) buildMux() (mux *http.ServeMux, err error) {
	mux = http.NewServeMux()
	mux.Handle("/healthz", a.reg.health.LiveHandler())
	mux.Handle("/readyz", a.reg.health.ReadyHandler())
	if a.cfg.pprof {
		mountPprof(mux)
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
func mountPprof(mux *http.ServeMux) {
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	for _, name := range []string{"goroutine", "heap", "allocs", "block", "mutex", "threadcreate"} {
		mux.Handle("GET /debug/pprof/"+name, pprof.Handler(name))
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
	return server
}

func (a *App) wrap(h http.Handler) http.Handler {
	for i := len(a.cfg.middleware) - 1; i >= 0; i-- {
		h = a.cfg.middleware[i](h)
	}
	return h
}

// stageNone 表示尚无任何启动钩子开始执行。
const stageNone = math.MinInt

// startHooks 按 (stage, 注册序) 执行启动钩子，并在 StageServer 插入 HTTP 监听。
// 返回实际开始执行过的最高 stage（关停时更高 stage 的 OnStop 会被跳过）。
func (a *App) startHooks(ctx context.Context, server *http.Server) (maxStage int, err error) {
	maxStage = stageNone
	hooks := make([]startHook, len(a.reg.starts))
	copy(hooks, a.reg.starts)
	hooks = append(hooks, startHook{
		stage:  StageServer,
		seq:    len(hooks),
		module: "appkit",
		fn:     func(context.Context) error { return a.listen(server) },
	})
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
			a.serverErr <- err
		}
	}()
	return nil
}

// shutdown 执行优雅关停：摘流量 → drain → HTTP Shutdown → OnStop 按启动逆序。
// 使用独立的超时 ctx（不能复用已取消的信号 ctx）。maxStartedStage 之上的
// OnStop 直接跳过——对应的 OnStart 从未执行过。
func (a *App) shutdown(server *http.Server, maxStartedStage int) error {
	a.reg.health.SetReady(false)
	if a.cfg.drainDelay > 0 {
		time.Sleep(a.cfg.drainDelay)
	}

	sctx, cancel := context.WithTimeout(context.Background(), a.cfg.shutdownTimeout)
	defer cancel()

	var errs []error
	if err := server.Shutdown(sctx); err != nil {
		errs = append(errs, fmt.Errorf("appkit: HTTP 关停: %w", err))
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
		if err := a.runStop(sctx, s); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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
