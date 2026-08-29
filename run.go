package appkit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"
)

// Run 启动应用并阻塞到 ctx 取消、收到 SIGINT/SIGTERM 或 HTTP 服务异常退出，
// 然后优雅关停。
//
// 启动顺序：Register（声明）→ Remote 绑定 → 依赖图解析（fail-fast）→ 迁移 →
// Setup（装配）→ 消费者订阅 Bus → OnStart 按 stage 升序 → HTTP 监听 → 置 ready。
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
	if err := a.reg.resolveAll(); err != nil {
		return err
	}
	if a.cfg.migrator != nil && len(a.reg.migrations) > 0 {
		if err := a.cfg.migrator(ctx, a.reg.migrations); err != nil {
			return fmt.Errorf("appkit: 迁移失败: %w", err)
		}
	}
	if err := a.reg.runSetups(ctx); err != nil {
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
		}
	} else {
		log.Error("appkit: 启动失败，进入关停", "err", startErr)
	}

	cancelRun()
	shutdownErr := a.shutdown(server, maxStage)
	return errors.Join(startErr, serveErr, shutdownErr)
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
