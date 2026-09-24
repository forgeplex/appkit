package appkit

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/health"
	"github.com/forgeplex/appkit/internal/hoststate"
)

type appUseState struct {
	mu     sync.Mutex
	usedBy string
}

type appRuntime struct {
	use       appUseState
	serverErr chan error
}

func (a *App) claimUse(entrypoint string) error {
	if a.runtime == nil {
		return apperr.Internal(errors.New("appkit: App 未通过 New 构造"))
	}
	a.runtime.use.mu.Lock()
	defer a.runtime.use.mu.Unlock()
	if a.runtime.use.usedBy != "" {
		return apperr.Conflict("App 已由 %s 消费，不能再调用 %s", a.runtime.use.usedBy, entrypoint).
			WithDetail("used_by", a.runtime.use.usedBy)
	}
	a.runtime.use.usedBy = entrypoint
	return nil
}

type hostState uint8

const (
	hostNew hostState = iota
	hostRegistering
	hostResolving
	hostMigrating
	hostSettingUp
	hostStarting
	hostReady
	hostDraining
	hostStopping
	hostStopped
)

type shutdownRequest struct{ ctx context.Context }

// RunningApp 是已达到 Ready 的嵌入式 Host 句柄。Wait 可并发、重复调用；
// Shutdown 首次调用请求关停，后续调用等待同一个终态。
type RunningApp struct {
	app            *App
	ctx            context.Context
	cancelRun      context.CancelFunc
	serviceCtx     context.Context
	cancelServices context.CancelFunc

	stop       chan shutdownRequest
	stopOnce   sync.Once
	ready      chan struct{}
	done       chan struct{}
	serviceErr chan error

	mu                sync.Mutex
	state             hostState
	err               error
	serviceExitErrors []error
	services          []*managedServiceRuntime
	draining          atomic.Bool
}

type managedServiceRuntime struct {
	reg            serviceReg
	service        ManagedService
	startAttempted atomic.Bool
	runStarted     bool
	unexpected     atomic.Bool
	runLive        chan struct{}
	done           chan struct{}
	mu             sync.Mutex
	runErr         error
	lifecycle      *hoststate.Service
}

// Start 在依赖装配、迁移、Setup、启动钩子和 Service Ready 后返回。
// 与 Run 不同，Start 不注册 OS Signal Handler；传入 Context 覆盖启动期和
// 整个运行期，其取消会发起优雅关停。
func (a *App) Start(ctx context.Context) (*RunningApp, error) {
	return a.start(ctx, "Start")
}

func (a *App) start(ctx context.Context, entrypoint string) (*RunningApp, error) {
	if ctx == nil {
		return nil, apperr.InvalidArgument("%s Context 不能为空", entrypoint)
	}
	if err := a.claimUse(entrypoint); err != nil {
		return nil, err
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	serviceCtx, cancelServices := context.WithCancel(context.WithoutCancel(ctx))
	host := &RunningApp{
		app: a, ctx: runCtx, cancelRun: cancelRun,
		serviceCtx: serviceCtx, cancelServices: cancelServices,
		stop: make(chan shutdownRequest, 1), ready: make(chan struct{}),
		done: make(chan struct{}), serviceErr: make(chan error, 1),
		state: hostNew,
	}
	go host.run()
	select {
	case <-host.ready:
		return host, nil
	case <-host.done:
		return nil, host.Wait()
	}
}

func (h *RunningApp) run() {
	err := h.app.runHost(h)
	// Errors before ManagedService startup can return without entering shutdown
	// (for example, dependency resolution). Those instances were never started.
	for _, service := range h.services {
		if !service.startAttempted.Load() {
			service.lifecycle.Transition(hoststate.StateNotStarted)
		}
	}
	h.cancelRun()
	h.cancelServices()
	if err != nil {
		h.setState(hostStopping)
	}
	h.setState(hostStopped)
	h.mu.Lock()
	h.err = err
	close(h.done)
	h.mu.Unlock()
}

// Wait 等待 Host 终止。所有调用均返回相同的运行终态错误。
func (h *RunningApp) Wait() error {
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// Readiness 在进程内执行 App 的就绪检查，不要求开启 HTTP Listener。空结果表示
// Host 就绪；关停开始后会包含框架 not-ready 检查项。返回错误仅供进程内受信代码
// 使用，不应复制到未认证的探针响应中。
func (h *RunningApp) Readiness(ctx context.Context) (map[string]error, error) {
	if h == nil || h.app == nil {
		return nil, apperr.InvalidArgument("RunningApp 不能为空")
	}
	if ctx == nil {
		return nil, apperr.InvalidArgument("Readiness Context 不能为空")
	}
	return h.app.reg.health.Ready(ctx), nil
}

// Shutdown 请求 Host 优雅关停并等待完成。ctx 同时限制本次关停预算；预算
// 耗尽时返回 ctx.Err，Host 仍会继续执行剩余清理并走强制关闭路径。
func (h *RunningApp) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return apperr.InvalidArgument("Shutdown Context 不能为空")
	}
	h.stopOnce.Do(func() { h.stop <- shutdownRequest{ctx: ctx} })
	select {
	case <-h.done:
		return h.Wait()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *RunningApp) setState(state hostState) {
	h.mu.Lock()
	h.state = state
	h.mu.Unlock()
}

func (h *RunningApp) markReady() {
	h.setState(hostReady)
	close(h.ready)
}

func (h *RunningApp) recordServiceExit(err error) {
	h.mu.Lock()
	h.serviceExitErrors = append(h.serviceExitErrors, err)
	h.mu.Unlock()
	if h.app.cfg.logger != nil {
		h.app.cfg.logger.Warn("appkit: Optional ManagedService 意外退出", "err", err)
	}
}

func (h *RunningApp) addService(reg serviceReg, service ManagedService) *managedServiceRuntime {
	runtime := &managedServiceRuntime{
		reg: reg, service: service,
		lifecycle: hoststate.NewService(reg.module, reg.name),
	}
	h.services = append(h.services, runtime)
	return runtime
}

func (s *managedServiceRuntime) setLifecycleState(state hoststate.State) {
	s.lifecycle.Transition(state)
}

func (s *managedServiceRuntime) setRunResult(err error) {
	s.mu.Lock()
	s.runErr = err
	close(s.done)
	s.mu.Unlock()
}

func (s *managedServiceRuntime) result() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runErr
}

func (a *App) resolveManagedServices(host *RunningApp) error {
	for _, registration := range a.reg.runtime.services {
		previous := a.reg.current
		a.reg.current = registration.module
		service, err := callManagedServiceFactory(registration.factory, a.reg)
		a.reg.current = previous
		if err != nil {
			return fmt.Errorf("appkit: 模块 %q 构造 ManagedService %q 失败: %w", registration.module, registration.name, err)
		}
		if isNilManagedService(service) {
			return fmt.Errorf("appkit: 模块 %q 的 ManagedService %q factory 返回 nil", registration.module, registration.name)
		}
		host.addService(registration, service)
		a.reg.current = registration.module
		a.reg.Health("service/"+registration.name, health.CheckFunc(service.Ready))
		a.reg.current = previous
	}
	return nil
}

func callManagedServiceFactory(factory ManagedServiceFactory, registry *Registry) (service ManagedService, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("ManagedService factory panic: %v", recovered)
		}
	}()
	return factory(registry)
}

func isNilManagedService(service ManagedService) bool {
	if service == nil {
		return true
	}
	value := reflect.ValueOf(service)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (h *RunningApp) startManagedServices(startCtx context.Context) error {
	for _, runtime := range h.services {
		select {
		case err := <-h.serviceErr:
			return err
		default:
		}
		runtime.setLifecycleState(hoststate.StateStarting)
		runtime.startAttempted.Store(true)
		if err := callManagedService("Start", func() error { return runtime.service.Start(startCtx) }); err != nil {
			runtime.setLifecycleState(hoststate.StateFailed)
			return fmt.Errorf("appkit: 模块 %q ManagedService %q Start 失败: %w", runtime.reg.module, runtime.reg.name, err)
		}
		select {
		case err := <-h.serviceErr:
			return err
		default:
		}
		runtime.done = make(chan struct{})
		runtime.runLive = make(chan struct{})
		runtime.runStarted = true
		go func(s *managedServiceRuntime) {
			s.setLifecycleState(hoststate.StateRunning)
			close(s.runLive)
			err := callManagedService("Run", func() error { return s.service.Run(h.serviceCtx) })
			if !h.draining.Load() {
				s.setLifecycleState(hoststate.StateFailed)
				s.unexpected.Store(true)
				if err == nil {
					err = errors.New("Run 在 Host 进入 Draining 前返回")
				}
				wrapped := fmt.Errorf("appkit: 模块 %q ManagedService %q 异常退出: %w", s.reg.module, s.reg.name, err)
				if s.reg.policy == ServiceCritical {
					select {
					case h.serviceErr <- wrapped:
					default:
					}
				} else {
					h.recordServiceExit(wrapped)
				}
			}
			s.setRunResult(err)
		}(runtime)
		select {
		case <-runtime.runLive:
		case <-runtime.done:
			runtime.setLifecycleState(hoststate.StateFailed)
			return fmt.Errorf("appkit: 模块 %q ManagedService %q Run 在启动前退出: %w", runtime.reg.module, runtime.reg.name, unexpectedServiceExit(runtime))
		case <-startCtx.Done():
			runtime.setLifecycleState(hoststate.StateFailed)
			return startCtx.Err()
		}
		if err := waitManagedServiceReady(startCtx, runtime, h.serviceErr); err != nil {
			return fmt.Errorf("appkit: 模块 %q ManagedService %q 未就绪: %w", runtime.reg.module, runtime.reg.name, err)
		}
		runtime.setLifecycleState(hoststate.StateReady)
	}
	return nil
}

func callManagedService(phase string, fn func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%s panic: %v", phase, recovered)
		}
	}()
	return fn()
}

func waitManagedServiceReady(ctx context.Context, runtime *managedServiceRuntime, serviceErr <-chan error) error {
	ready := make(chan error, 1)
	go func() {
		ready <- callManagedService("Ready", func() error { return runtime.service.Ready(ctx) })
	}()
	select {
	case err := <-ready:
		if err != nil {
			runtime.setLifecycleState(hoststate.StateFailed)
			return err
		}
		select {
		case <-runtime.done:
			return unexpectedServiceExit(runtime)
		case err := <-serviceErr:
			return err
		default:
			return nil
		}
	case <-runtime.done:
		return unexpectedServiceExit(runtime)
	case err := <-serviceErr:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func unexpectedServiceExit(runtime *managedServiceRuntime) error {
	if err := runtime.result(); err != nil {
		return err
	}
	return errors.New("Run 在 Service Ready 前返回")
}

func runManagedServicePhase(ctx context.Context, runtime *managedServiceRuntime, phase string, fn func(context.Context) error) error {
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		done <- callManagedService(phase, func() error {
			close(started)
			return fn(ctx)
		})
	}()
	// 即使预算已耗尽，也等到生命周期回调被派发后再继续；否则 select 可能
	// 直接选择 ctx.Done，Host 已结束而 Close goroutine 尚未开始执行。
	<-started
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("appkit: 模块 %q ManagedService %q %s 失败: %w", runtime.reg.module, runtime.reg.name, phase, err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("appkit: 模块 %q ManagedService %q %s 超时: %w", runtime.reg.module, runtime.reg.name, phase, ctx.Err())
	}
}

func (h *RunningApp) serviceExitErrorsSnapshot() []error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]error(nil), h.serviceExitErrors...)
}
