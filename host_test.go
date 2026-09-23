package appkit

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forgeplex/appkit/apperr"
)

type recordingManagedService struct {
	mu             sync.Mutex
	events         []string
	startErr       error
	readyErr       error
	drainErr       error
	closeErr       error
	readyGate      chan struct{}
	runReturn      chan error
	runStarted     chan struct{}
	runExited      chan struct{}
	readyStarted   chan struct{}
	drainStarted   chan struct{}
	drainGate      <-chan struct{}
	runCtx         context.Context
	trace          *serviceTrace
	traceName      string
	drainCheck     func()
	closeCheck     func()
	runStartOnce   sync.Once
	runExitOnce    sync.Once
	readyStartOnce sync.Once
	drainStartOnce sync.Once
	closeCalls     atomic.Int32
	drainSawLive   atomic.Bool
}

type hostTestDependency interface{ Value() string }
type hostTestValue string

type serviceTrace struct {
	mu     sync.Mutex
	events []string
}

func (v hostTestValue) Value() string { return string(v) }

func newRecordingManagedService() *recordingManagedService {
	return &recordingManagedService{
		runStarted:   make(chan struct{}),
		runExited:    make(chan struct{}),
		readyStarted: make(chan struct{}),
		drainStarted: make(chan struct{}),
	}
}

func (s *recordingManagedService) record(event string) {
	s.mu.Lock()
	s.events = append(s.events, event)
	trace, name := s.trace, s.traceName
	s.mu.Unlock()
	if trace != nil {
		trace.record(name + ":" + event)
	}
}

func (trace *serviceTrace) record(event string) {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.events = append(trace.events, event)
}

func (trace *serviceTrace) snapshot() []string {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return append([]string(nil), trace.events...)
}

func (s *recordingManagedService) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func (s *recordingManagedService) Start(context.Context) error {
	s.record("start")
	return s.startErr
}

func (s *recordingManagedService) Run(ctx context.Context) error {
	s.mu.Lock()
	s.runCtx = ctx
	s.mu.Unlock()
	s.record("run")
	s.runStartOnce.Do(func() { close(s.runStarted) })
	var err error
	if s.runReturn == nil {
		<-ctx.Done()
		err = ctx.Err()
	} else {
		select {
		case err = <-s.runReturn:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	s.record("run-exit")
	s.runExitOnce.Do(func() { close(s.runExited) })
	return err
}

func (s *recordingManagedService) Ready(ctx context.Context) error {
	select {
	case <-s.runStarted:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.record("ready")
	s.readyStartOnce.Do(func() { close(s.readyStarted) })
	if s.readyGate != nil {
		select {
		case <-s.readyGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.readyErr
}

func (s *recordingManagedService) Drain(ctx context.Context) error {
	s.record("drain")
	if s.drainCheck != nil {
		s.drainCheck()
	}
	s.mu.Lock()
	runCtx := s.runCtx
	s.mu.Unlock()
	if runCtx == nil || runCtx.Err() != nil {
		return errors.New("Run Context was cancelled before Drain")
	}
	s.drainSawLive.Store(true)
	s.drainStartOnce.Do(func() { close(s.drainStarted) })
	if s.drainGate != nil {
		select {
		case <-s.drainGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.drainErr
}

func (s *recordingManagedService) Close(context.Context) error {
	s.closeCalls.Add(1)
	if s.closeCheck != nil {
		s.closeCheck()
	}
	s.record("close")
	return s.closeErr
}

func managedServiceModule(s *recordingManagedService, policy ServicePolicy) Module {
	return ModuleFunc("managed", func(reg *Registry) error {
		return reg.ManagedService("test", policy, func(*Registry) (ManagedService, error) {
			return s, nil
		})
	})
}

func newHostTestApp(modules []Module, opts ...Option) *App {
	opts = append([]Option{HTTPAddr("127.0.0.1:0"), ShutdownTimeout(time.Second)}, opts...)
	return newTestApp(modules, opts...)
}

func TestHeadlessAppStartNeedsNoSecurityOrHTTPListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	app := New(nil, HTTPAddr(listener.Addr().String()), Headless())
	host, err := app.Start(context.Background())
	if err != nil {
		t.Fatalf("Headless Start: %v", err)
	}
	if got, err := host.Readiness(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("ready Headless Readiness = (%v, %v), want empty map", got, err)
	}
	if err := host.Shutdown(context.Background()); err != nil {
		t.Fatalf("Headless Shutdown: %v", err)
	}
	if got, err := host.Readiness(context.Background()); err != nil || len(got) == 0 {
		t.Fatalf("stopped Headless Readiness = (%v, %v), want not-ready entry", got, err)
	}
}

func TestHeadlessRejectsRoutesAndPprof(t *testing.T) {
	module := ModuleFunc("route", func(reg *Registry) error {
		reg.Mount("/business", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		return nil
	})
	for _, tc := range []struct {
		name    string
		modules []Module
		options []Option
		want    string
	}{
		{name: "route", modules: []Module{module}, want: `HTTP 路由 "/business"`},
		{name: "pprof", options: []Option{Pprof()}, want: "pprof"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := append(tc.options, Headless())
			app := New(tc.modules, opts...)
			if _, err := app.Start(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Headless Start error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestDisableMigrationsFailsFastWhenMigrationsAreDeclared(t *testing.T) {
	app := newTestApp([]Module{migratingModule()}, Headless(), DisableMigrations())
	if err := app.Run(context.Background()); err == nil || !strings.Contains(err.Error(), `模块 "billing" 声明了迁移`) {
		t.Fatalf("Run error = %v, want disabled migration capability error", err)
	}

	app = New([]Module{migratingModule()}, DisableMigrations())
	if err := app.Migrate(context.Background()); err == nil || !strings.Contains(err.Error(), `模块 "billing" 声明了迁移`) {
		t.Fatalf("Migrate error = %v, want disabled migration capability error", err)
	}
}

func TestStartWaitShutdownManagesServiceLifecycle(t *testing.T) {
	service := newRecordingManagedService()
	app := newHostTestApp([]Module{managedServiceModule(service, ServiceCritical)})
	host, err := app.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	readState := func() hostState {
		host.mu.Lock()
		defer host.mu.Unlock()
		return host.state
	}
	if got := readState(); got != hostReady {
		t.Fatalf("Start 返回时 Host state = %d, want Ready", got)
	}
	var drainState, closeState hostState
	readinessRevoked := false
	service.drainCheck = func() {
		drainState = readState()
		readinessRevoked = len(app.reg.health.Ready(context.Background())) > 0
	}
	service.closeCheck = func() { closeState = readState() }
	<-service.runStarted
	if err := host.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got, want := service.snapshot(), []string{"start", "run", "ready", "drain", "run-exit", "close"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("service 生命周期顺序 = %v, want %v", got, want)
	}
	if !service.drainSawLive.Load() {
		t.Fatal("Service Run Context 必须在 Drain 完成前保持有效")
	}
	if got := service.closeCalls.Load(); got != 1 {
		t.Fatalf("Close 调用次数 = %d, want 1", got)
	}
	if err := host.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := host.Wait(); err != nil {
		t.Fatalf("第二次 Wait: %v", err)
	}
	if drainState != hostDraining || closeState != hostStopping || readState() != hostStopped {
		t.Fatalf("Host 状态阶段错误：Drain=%d Close=%d final=%d", drainState, closeState, readState())
	}
	if !readinessRevoked {
		t.Fatal("进入 ManagedService Drain 前必须先撤销全局 readiness")
	}
	if err := host.Shutdown(context.Background()); err != nil {
		t.Fatalf("重复 Shutdown: %v", err)
	}
}

func TestStartDoesNotReturnBeforeManagedServiceReady(t *testing.T) {
	service := newRecordingManagedService()
	service.readyGate = make(chan struct{})
	app := newHostTestApp([]Module{managedServiceModule(service, ServiceCritical)})
	type startResult struct {
		host *RunningApp
		err  error
	}
	result := make(chan startResult, 1)
	go func() {
		host, err := app.Start(context.Background())
		result <- startResult{host: host, err: err}
	}()
	select {
	case <-service.readyStarted:
	case <-time.After(time.Second):
		t.Fatal("Service Ready 未開始")
	}
	select {
	case got := <-result:
		t.Fatalf("Ready 未完成時 Start 不應返回：host=%v err=%v", got.host, got.err)
	case <-time.After(20 * time.Millisecond):
	}
	close(service.readyGate)
	var started startResult
	select {
	case started = <-result:
	case <-time.After(time.Second):
		t.Fatal("Ready 完成後 Start 未返回")
	}
	if started.err != nil {
		t.Fatalf("Start: %v", started.err)
	}
	if err := started.host.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestAppIsSingleUseAcrossStartAndMigrate(t *testing.T) {
	app := newHostTestApp(nil)
	host, err := app.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := app.Start(context.Background()); !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("重复 Start 应返回 CONFLICT，实际 %v", err)
	}
	if err := app.Migrate(context.Background()); !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("Start 后 Migrate 应返回 CONFLICT，实际 %v", err)
	}
	if err := app.Run(context.Background()); !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("Start 后 Run 应返回 CONFLICT，实际 %v", err)
	}
	if err := host.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	migrationApp := newHostTestApp(nil)
	if err := migrationApp.Migrate(context.Background()); err != nil {
		t.Fatalf("首次 Migrate: %v", err)
	}
	if _, err := migrationApp.Start(context.Background()); !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("Migrate 后 Start 应返回 CONFLICT，实际 %v", err)
	}
	if err := migrationApp.Migrate(context.Background()); !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("重复 Migrate 应返回 CONFLICT，实际 %v", err)
	}
}

func TestConcurrentStartOnlyConsumesAppOnce(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	module := ModuleFunc("block", func(reg *Registry) error {
		reg.OnStart(StageInfra, func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
		return nil
	})
	app := newHostTestApp([]Module{module})
	type startResult struct {
		host *RunningApp
		err  error
	}
	first := make(chan startResult, 1)
	go func() {
		host, err := app.Start(context.Background())
		first <- startResult{host: host, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("首次 Start 未进入启动钩子")
	}
	if _, err := app.Start(context.Background()); !apperr.Is(err, apperr.CodeConflict) {
		close(release)
		t.Fatalf("并发 Start 应立即返回 CONFLICT，实际 %v", err)
	}
	close(release)
	result := <-first
	if result.err != nil {
		t.Fatalf("首次 Start: %v", result.err)
	}
	if err := result.host.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestManagedServiceStartFailureClosesPartialResource(t *testing.T) {
	startErr := errors.New("partial startup")
	closeErr := errors.New("partial resource close failed")
	service := newRecordingManagedService()
	service.startErr = startErr
	service.closeErr = closeErr
	app := newHostTestApp([]Module{managedServiceModule(service, ServiceCritical)})
	if host, err := app.Start(context.Background()); host != nil || !errors.Is(err, startErr) || !errors.Is(err, closeErr) {
		t.Fatalf("Start 应返回根因且不返回句柄，host=%v err=%v", host, err)
	}
	if _, err := app.Start(context.Background()); !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("启动失败后重试应返回 CONFLICT，实际 %v", err)
	}
	if got, want := service.snapshot(), []string{"start", "close"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("部分启动失败的回滚 = %v, want %v", got, want)
	}
	if got := service.closeCalls.Load(); got != 1 {
		t.Fatalf("Close 调用次数 = %d, want 1", got)
	}
}

func TestUnstartedManagedServiceIsNotClosedOnEarlierStartFailure(t *testing.T) {
	service := newRecordingManagedService()
	startErr := errors.New("infra startup failed")
	failing := ModuleFunc("failing", func(reg *Registry) error {
		reg.OnStart(StageInfra, func(context.Context) error { return startErr })
		return nil
	})
	app := newHostTestApp([]Module{managedServiceModule(service, ServiceCritical), failing})
	if host, err := app.Start(context.Background()); host != nil || !errors.Is(err, startErr) {
		t.Fatalf("预期基础设施启动失败，host=%v err=%v", host, err)
	}
	if got := service.snapshot(); len(got) != 0 {
		t.Fatalf("未调用 Start 的 Service 不应进入回滚：%v", got)
	}
	if got := service.closeCalls.Load(); got != 0 {
		t.Fatalf("未调用 Start 的 Service 不应调用 Close，次数=%d", got)
	}
}

func TestManagedServiceReadyFailureDrainsThenCloses(t *testing.T) {
	readyErr := errors.New("not ready")
	service := newRecordingManagedService()
	service.readyErr = readyErr
	app := newHostTestApp([]Module{managedServiceModule(service, ServiceCritical)})
	if host, err := app.Start(context.Background()); host != nil || !errors.Is(err, readyErr) {
		t.Fatalf("Ready 错误应导致启动回滚，host=%v err=%v", host, err)
	}
	if got, want := service.snapshot(), []string{"start", "run", "ready", "drain", "run-exit", "close"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Ready 失败的回滚顺序 = %v, want %v", got, want)
	}
}

func TestCriticalManagedServiceExitStopsHost(t *testing.T) {
	exitErr := errors.New("service failed")
	closeErr := errors.New("close failed")
	service := newRecordingManagedService()
	service.closeErr = closeErr
	service.runReturn = make(chan error, 1)
	app := newHostTestApp([]Module{managedServiceModule(service, ServiceCritical)})
	host, err := app.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	service.runReturn <- exitErr
	if err := host.Wait(); !errors.Is(err, exitErr) || !errors.Is(err, closeErr) {
		t.Fatalf("Critical Service 根因与 Close 错误都应保留，实际 %v", err)
	}
	if service.closeCalls.Load() != 1 {
		t.Fatalf("关停后 Close 调用次数 = %d, want 1", service.closeCalls.Load())
	}
}

func TestCriticalServiceExitAbortsLaterServiceReadiness(t *testing.T) {
	exitErr := errors.New("first service failed during startup")
	first := newRecordingManagedService()
	first.runReturn = make(chan error, 1)
	second := newRecordingManagedService()
	second.readyGate = make(chan struct{})
	module := func(name string, service *recordingManagedService) Module {
		return ModuleFunc(name, func(reg *Registry) error {
			return reg.ManagedService("test", ServiceCritical, func(*Registry) (ManagedService, error) {
				return service, nil
			})
		})
	}
	app := newHostTestApp([]Module{module("first", first), module("second", second)})
	type startResult struct {
		host *RunningApp
		err  error
	}
	result := make(chan startResult, 1)
	go func() {
		host, err := app.Start(context.Background())
		result <- startResult{host: host, err: err}
	}()
	select {
	case <-second.readyStarted:
	case <-time.After(time.Second):
		t.Fatal("第二个 Service Ready 未开始")
	}
	first.runReturn <- exitErr
	select {
	case got := <-result:
		if got.host != nil || !errors.Is(got.err, exitErr) {
			t.Fatalf("关键 Service 退出应中止启动并保留错误，host=%v err=%v", got.host, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("先前 Critical Service 退出后，Host 仍卡在后续 Service Ready")
	}
	if first.closeCalls.Load() != 1 || second.closeCalls.Load() != 1 {
		t.Fatalf("启动中止后已启动的 Service 均须 Close：first=%d second=%d", first.closeCalls.Load(), second.closeCalls.Load())
	}
}

func TestOptionalManagedServiceExitDoesNotStopHost(t *testing.T) {
	exitErr := errors.New("optional service failed")
	service := newRecordingManagedService()
	service.runReturn = make(chan error, 1)
	app := newHostTestApp([]Module{managedServiceModule(service, ServiceOptional)})
	host, err := app.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	service.runReturn <- exitErr
	select {
	case <-service.runExited:
	case <-time.After(time.Second):
		t.Fatal("Optional Service 未退出")
	}
	select {
	case <-host.done:
		t.Fatal("Optional Service 退出不应自动关闭 Host")
	default:
	}
	if err := host.Shutdown(context.Background()); !errors.Is(err, exitErr) {
		t.Fatalf("Optional Service 错误应记录在终态中，实际 %v", err)
	}
}

func TestShutdownBudgetExpiryStillCancelsWaitsAndCloses(t *testing.T) {
	service := newRecordingManagedService()
	service.drainGate = make(chan struct{})
	app := newHostTestApp([]Module{managedServiceModule(service, ServiceCritical)})
	host, err := app.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := host.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown 应在预算耗尽时返回 DeadlineExceeded，实际 %v", err)
	}
	if err := host.Wait(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Host 终态应记录 Drain 超时，实际 %v", err)
	}
	if !service.drainSawLive.Load() {
		t.Fatal("预算耗尽前 Drain 阶段必须保留 Service Run Context")
	}
	if service.closeCalls.Load() != 1 {
		t.Fatalf("预算耗尽后仍须调用 Close，次数=%d", service.closeCalls.Load())
	}
}

func TestShutdownBudgetExpiryIsRetainedWithoutServiceHooks(t *testing.T) {
	app := newHostTestApp(nil)
	host, err := app.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := host.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown 应报告预算耗尽，实际 %v", err)
	}
	if err := host.Wait(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait 应保留预算耗尽错误，实际 %v", err)
	}
}

func TestShutdownBudgetForceClosesInflightHTTPRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		server.Close()
	})
	requestResult := make(chan error, 1)
	go func() {
		response, err := http.Get(server.URL)
		if response != nil {
			_ = response.Body.Close()
		}
		requestResult <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("HTTP handler 未收到请求")
	}

	app := newHostTestApp(nil)
	_, cancelRun := context.WithCancel(context.Background())
	_, cancelServices := context.WithCancel(context.Background())
	defer cancelRun()
	defer cancelServices()
	host := &RunningApp{app: app, cancelRun: cancelRun, cancelServices: cancelServices}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := app.shutdown(server.Config, stageNone, host, ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("HTTP 等待超出预算应进入强制关闭并保留 deadline，实际 %v", err)
	}
	select {
	case err := <-requestResult:
		if err == nil {
			t.Fatal("强制关闭后在途 HTTP 请求不应成功完成")
		}
	case <-time.After(time.Second):
		t.Fatal("强制关闭没有中断在途 HTTP 请求")
	}
	releaseOnce.Do(func() { close(release) })
}

func TestParentContextCancellationGracefullyStopsHost(t *testing.T) {
	service := newRecordingManagedService()
	ctx, cancel := context.WithCancel(context.Background())
	app := newHostTestApp([]Module{managedServiceModule(service, ServiceCritical)})
	host, err := app.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()
	if err := host.Wait(); err != nil {
		t.Fatalf("父 Context 正常取消不应作为运行错误：%v", err)
	}
	if got, want := service.snapshot(), []string{"start", "run", "ready", "drain", "run-exit", "close"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("父 Context 取消时的生命周期顺序 = %v, want %v", got, want)
	}
}

func TestManagedServicesDrainWaitAndCloseInReversePhases(t *testing.T) {
	trace := &serviceTrace{}
	first := newRecordingManagedService()
	first.trace, first.traceName = trace, "first"
	second := newRecordingManagedService()
	second.trace, second.traceName = trace, "second"
	module := func(name string, service *recordingManagedService) Module {
		return ModuleFunc(name, func(reg *Registry) error {
			return reg.ManagedService("test", ServiceCritical, func(*Registry) (ManagedService, error) {
				return service, nil
			})
		})
	}
	app := newHostTestApp([]Module{module("first", first), module("second", second)})
	host, err := app.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := host.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	got := trace.snapshot()
	index := func(event string) int {
		for i, item := range got {
			if item == event {
				return i
			}
		}
		return -1
	}
	if index("second:drain") < 0 || index("first:drain") < 0 || index("second:run-exit") < 0 || index("first:run-exit") < 0 || index("second:close") < 0 || index("first:close") < 0 {
		t.Fatalf("生命周期阶段缺失：%v", got)
	}
	if !(index("second:drain") < index("first:drain")) {
		t.Fatalf("Drain 必须按注册逆序：%v", got)
	}
	lastDrain := max(index("second:drain"), index("first:drain"))
	firstRunExit := min(index("second:run-exit"), index("first:run-exit"))
	lastRunExit := max(index("second:run-exit"), index("first:run-exit"))
	if !(lastDrain < firstRunExit && lastRunExit < index("second:close")) {
		t.Fatalf("所有 Drain 完成后才取消/等待，且所有 Run 退出后才 Close：%v", got)
	}
	if !(index("second:close") < index("first:close")) {
		t.Fatalf("Close 必须按注册逆序：%v", got)
	}
}

func TestConcurrentWaitAndShutdownShareTerminalState(t *testing.T) {
	app := newHostTestApp(nil)
	host, err := app.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	const callers = 8
	entered := make(chan struct{}, callers)
	results := make(chan error, callers)
	for i := 0; i < callers/2; i++ {
		go func() {
			entered <- struct{}{}
			results <- host.Wait()
		}()
	}
	for i := callers / 2; i < callers; i++ {
		go func() {
			entered <- struct{}{}
			results <- host.Shutdown(context.Background())
		}()
	}
	for i := 0; i < callers; i++ {
		<-entered
	}
	for i := 0; i < callers; i++ {
		if err := <-results; err != nil {
			t.Fatalf("并发 Wait/Shutdown: %v", err)
		}
	}
}

func TestManagedServiceFactoryCanResolveAfterSetup(t *testing.T) {
	var got string
	setupComplete := false
	service := newRecordingManagedService()
	module := ModuleFunc("factory", func(reg *Registry) error {
		ProvideValue[hostTestDependency](reg, hostTestValue("resolved"))
		reg.Setup(func(context.Context) error {
			setupComplete = true
			return nil
		})
		return reg.ManagedService("test", ServiceOptional, func(reg *Registry) (ManagedService, error) {
			if !setupComplete {
				return nil, errors.New("ManagedService factory ran before Setup")
			}
			dep, err := Resolve[hostTestDependency](reg)
			if err != nil {
				return nil, err
			}
			got = dep.Value()
			return service, nil
		})
	})
	app := newHostTestApp([]Module{module})
	host, err := app.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got != "resolved" {
		t.Fatalf("factory resolved %q, want resolved", got)
	}
	if err := host.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
