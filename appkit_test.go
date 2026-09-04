package appkit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forgeplex/appkit/health"
)

type greeter interface{ Greet() string }

type localGreeter struct{ from string }

func (g localGreeter) Greet() string { return "hello from " + g.from }

// testModule 是可编程的假模块。
type testModule struct {
	name     string
	register func(reg *Registry) error
}

func (m *testModule) Name() string                 { return m.name }
func (m *testModule) Register(reg *Registry) error { return m.register(reg) }

func TestResolveMemoizedAndLazy(t *testing.T) {
	reg := newRegistry()
	var calls atomic.Int32
	Provide(reg, func(*Registry) (greeter, error) {
		calls.Add(1)
		return localGreeter{from: "a"}, nil
	})
	if calls.Load() != 0 {
		t.Fatal("Provide 不应立即构造")
	}
	g1 := MustResolve[greeter](reg)
	g2 := MustResolve[greeter](reg)
	if g1.Greet() != "hello from a" || g2 != g1 {
		t.Fatalf("解析结果不一致: %v %v", g1, g2)
	}
	if calls.Load() != 1 {
		t.Fatalf("构造函数应只执行一次，实际 %d", calls.Load())
	}
}

func TestResolveMissingDependency(t *testing.T) {
	reg := newRegistry()
	reg.current = "ledger"
	_, err := Resolve[greeter](reg)
	if err == nil || !strings.Contains(err.Error(), "ledger") {
		t.Fatalf("错误应指出需要方模块: %v", err)
	}
}

type svcA interface{ A() }
type svcB interface{ B() }

func TestResolveCycle(t *testing.T) {
	reg := newRegistry()
	Provide(reg, func(r *Registry) (svcA, error) {
		_, err := Resolve[svcB](r)
		return nil, err
	})
	Provide(reg, func(r *Registry) (svcB, error) {
		_, err := Resolve[svcA](r)
		return nil, err
	})
	_, err := Resolve[svcA](reg)
	if err == nil || !strings.Contains(err.Error(), "循环") {
		t.Fatalf("应检测到依赖循环: %v", err)
	}
}

func TestModuleFunc(t *testing.T) {
	m := ModuleFunc("inline", func(reg *Registry) error {
		ProvideValue[greeter](reg, localGreeter{from: "inline"})
		return nil
	})
	if m.Name() != "inline" {
		t.Fatalf("Name = %q", m.Name())
	}
	reg := newRegistry()
	if err := m.Register(reg); err != nil {
		t.Fatal(err)
	}
	if got := MustResolve[greeter](reg).Greet(); got != "hello from inline" {
		t.Fatalf("Register 未生效: %q", got)
	}
}

type wrappedGreeter struct{ inner greeter }

func (w wrappedGreeter) Greet() string { return "wrapped(" + w.inner.Greet() + ")" }

func TestProvideContractWraps(t *testing.T) {
	reg := newRegistry()
	ProvideContract(reg,
		func(*Registry) (greeter, error) { return localGreeter{from: "a"}, nil },
		func(g greeter) greeter { return wrappedGreeter{inner: g} },
	)
	got := MustResolve[greeter](reg).Greet()
	if got != "wrapped(hello from a)" {
		t.Fatalf("Resolve 应拿到已包裹实现，实际 %q", got)
	}
}

func TestProvideContractNilWrapPanics(t *testing.T) {
	reg := newRegistry()
	defer func() {
		if recover() == nil {
			t.Fatal("wrap 为 nil 应 panic")
		}
	}()
	ProvideContract(reg, func(*Registry) (greeter, error) { return localGreeter{}, nil }, nil)
}

func TestDuplicateProvidePanics(t *testing.T) {
	reg := newRegistry()
	Provide(reg, func(*Registry) (greeter, error) { return localGreeter{}, nil })
	defer func() {
		if recover() == nil {
			t.Fatal("重复 Provide 应 panic")
		}
	}()
	Provide(reg, func(*Registry) (greeter, error) { return localGreeter{}, nil })
}

func TestTargetFiltersModulesAndRemoteFallback(t *testing.T) {
	var resolved greeter
	consumer := &testModule{name: "gateway", register: func(reg *Registry) error {
		reg.Setup(func(context.Context) error {
			resolved = MustResolve[greeter](reg)
			return nil
		})
		return nil
	}}
	provider := &testModule{name: "ledger", register: func(reg *Registry) error {
		ProvideValue[greeter](reg, localGreeter{from: "local"})
		return nil
	}}

	// target 只含 gateway：greeter 落到 Remote 绑定。
	app := New([]Module{provider, consumer},
		Target("gateway"),
		Remote(func(*Registry) (greeter, error) { return localGreeter{from: "remote"}, nil }),
	)
	enabled, err := app.enabledModules()
	if err != nil {
		t.Fatal(err)
	}
	if len(enabled) != 1 || enabled[0].Name() != "gateway" {
		t.Fatalf("target 过滤失败: %v", moduleNames(enabled))
	}
	if err := app.register(enabled); err != nil {
		t.Fatal(err)
	}
	if err := app.reg.resolveAll(); err != nil {
		t.Fatal(err)
	}
	if err := app.reg.runSetups(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resolved == nil || resolved.Greet() != "hello from remote" {
		t.Fatalf("应使用 Remote 绑定: %v", resolved)
	}
}

func TestTargetUnknownModule(t *testing.T) {
	app := New([]Module{&testModule{name: "ledger", register: func(*Registry) error { return nil }}},
		Target("ledgerr"))
	if _, err := app.enabledModules(); err == nil {
		t.Fatal("未知 target 模块应报错")
	}
}

func TestRunLifecycleOrder(t *testing.T) {
	var order []string
	record := func(step string) HookFunc {
		return func(context.Context) error {
			order = append(order, step)
			return nil
		}
	}
	ready := make(chan struct{})
	m := &testModule{name: "demo", register: func(reg *Registry) error {
		ProvideValue[greeter](reg, localGreeter{from: "demo"})
		reg.Setup(record("setup"))
		reg.OnStart(StageWorker, record("start-worker"))
		reg.OnStart(StageInfra, record("start-infra"))
		reg.OnStart(StageServer+1, func(ctx context.Context) error {
			order = append(order, "start-late")
			close(ready)
			return nil
		})
		reg.OnStop(record("stop-1"))
		reg.OnStop(record("stop-2"))
		return nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	app := New([]Module{m}, HTTPAddr("127.0.0.1:0"), ShutdownTimeout(2*time.Second))
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()

	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("启动超时")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run 返回错误: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("关停超时")
	}

	want := []string{"setup", "start-infra", "start-worker", "start-late", "stop-2", "stop-1"}
	if len(order) != len(want) {
		t.Fatalf("生命周期顺序不符: %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("第 %d 步应为 %s，实际 %v", i, want[i], order)
		}
	}
}

// TestPprofGatedByOption 锁住排障端点的开关语义：默认关（404 而不是
// 「挂着但没人知道」——危险默认不能靠文档提醒），声明 Pprof() 后
// 索引页与具名 profile 都可用。
func TestPprofGatedByOption(t *testing.T) {
	mux, err := New(nil).buildMux()
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("默认不应挂 pprof: /debug/pprof/ = %d, want 404", rec.Code)
	}

	mux, err = New(nil, Pprof()).buildMux()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	for _, path := range []string{
		"/debug/pprof/", "/debug/pprof/cmdline",
		"/debug/pprof/goroutine?debug=1", "/debug/pprof/heap?debug=1",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200（%s）", path, resp.StatusCode, body)
		}
		if path == "/debug/pprof/" && !strings.Contains(string(body), "goroutine") {
			t.Error("pprof 索引页未列出 goroutine profile")
		}
	}
}

// gateModule 返回一个在 StageServer 之后置位 ready 的模块，用于等待启动完成。
func gateModule(ready chan struct{}) Module {
	return &testModule{name: "gate", register: func(reg *Registry) error {
		reg.OnStart(StageServer+1, func(context.Context) error {
			close(ready)
			return nil
		})
		return nil
	}}
}

// TestRunInfraFailureReturnsWithoutDeadlock 复现评审场景：StageInfra 失败时，
// relay 式模块（OnStop 直接 <-done，不感知 ctx）的 OnStart 从未执行、channel
// 永远无人写入。旧实现同步跑其 OnStop 会永久死锁；新实现按「实际启动过的
// 最高 stage」跳过它，Run 在关停预算内返回且错误含根因。
func TestRunInfraFailureReturnsWithoutDeadlock(t *testing.T) {
	done := make(chan error, 1)
	relay := &testModule{name: "relay", register: func(reg *Registry) error {
		reg.OnStart(StageWorker, func(ctx context.Context) error {
			go func() { <-ctx.Done(); done <- nil }()
			return nil
		})
		reg.OnStop(func(context.Context) error { return <-done })
		return nil
	}}
	infra := &testModule{name: "infra", register: func(reg *Registry) error {
		reg.OnStart(StageInfra, func(context.Context) error {
			return errors.New("连接池初始化失败")
		})
		return nil
	}}

	app := New([]Module{relay, infra}, HTTPAddr("127.0.0.1:0"), ShutdownTimeout(time.Second))
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(context.Background()) }()

	select {
	case err := <-runDone:
		if err == nil || !strings.Contains(err.Error(), "连接池初始化失败") {
			t.Fatalf("错误应含启动失败根因: %v", err)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("Run 未在 ShutdownTimeout 内返回（疑似 OnStop 死锁）")
	}
}

// TestRunStartFailureCancelsStartedWorkers 复现评审场景：worker 在 StageInfra
// 已启动、StageWorker 失败。旧实现不取消启动 ctx，等 worker 退出的 OnStop
// 会阻塞到超时；新实现进入关停前无条件 cancel，worker 正常退出。
func TestRunStartFailureCancelsStartedWorkers(t *testing.T) {
	done := make(chan error, 1)
	var stopped atomic.Bool
	worker := &testModule{name: "worker", register: func(reg *Registry) error {
		reg.OnStart(StageInfra, func(ctx context.Context) error {
			go func() {
				<-ctx.Done()
				stopped.Store(true)
				done <- nil
			}()
			return nil
		})
		reg.OnStop(func(ctx context.Context) error {
			select {
			case err := <-done:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		return nil
	}}
	boom := &testModule{name: "boom", register: func(reg *Registry) error {
		reg.OnStart(StageWorker, func(context.Context) error { return errors.New("worker 启动爆炸") })
		return nil
	}}

	app := New([]Module{worker, boom}, HTTPAddr("127.0.0.1:0"), ShutdownTimeout(time.Second))
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(context.Background()) }()

	select {
	case err := <-runDone:
		if err == nil || !strings.Contains(err.Error(), "worker 启动爆炸") {
			t.Fatalf("错误应含根因: %v", err)
		}
		if strings.Contains(err.Error(), "关停超时") {
			t.Fatalf("worker 应因 ctx 取消正常退出而非超时: %v", err)
		}
		if !stopped.Load() {
			t.Fatal("已启动 worker 的 ctx 未被取消")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未及时返回")
	}
}

// TestShutdownMirrorsStartOrder 验证停机顺序镜像启动顺序（stage 降序、同 stage
// 注册逆序），而非纯注册逆序；无 OnStart 的模块默认按 StageWorker 参与排序。
func TestShutdownMirrorsStartOrder(t *testing.T) {
	var mu sync.Mutex
	var stops []string
	record := func(name string) HookFunc {
		return func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			stops = append(stops, name)
			return nil
		}
	}
	worker := &testModule{name: "worker", register: func(reg *Registry) error {
		reg.OnStart(StageWorker, func(context.Context) error { return nil })
		reg.OnStop(record("worker"))
		return nil
	}}
	infra := &testModule{name: "infra", register: func(reg *Registry) error {
		reg.OnStart(StageInfra, func(context.Context) error { return nil })
		reg.OnStop(record("infra"))
		return nil
	}}
	plain := &testModule{name: "plain", register: func(reg *Registry) error {
		reg.OnStop(record("plain")) // 无 OnStart，默认 StageWorker
		return nil
	}}

	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	// worker 先注册、infra 后注册：纯注册逆序会先停 infra（错误）。
	app := New([]Module{worker, infra, plain, gateModule(ready)},
		HTTPAddr("127.0.0.1:0"), ShutdownTimeout(2*time.Second))
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(ctx) }()

	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("启动超时")
	}
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run 返回错误: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("关停超时")
	}

	want := []string{"plain", "worker", "infra"}
	mu.Lock()
	defer mu.Unlock()
	if len(stops) != len(want) {
		t.Fatalf("停机顺序不符: %v", stops)
	}
	for i := range want {
		if stops[i] != want[i] {
			t.Fatalf("第 %d 个停机应为 %s，实际 %v", i, want[i], stops)
		}
	}
}

// TestRunBindErrorSynchronous 验证端口被占时 Run 同步报出绑定错误，
// 不依赖 100ms 竞态窗口。
func TestRunBindErrorSynchronous(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	app := New(nil, HTTPAddr(ln.Addr().String()), ShutdownTimeout(time.Second))
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(context.Background()) }()

	select {
	case err := <-runDone:
		if err == nil || !strings.Contains(err.Error(), "监听") {
			t.Fatalf("应确定性报出绑定错误: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未及时返回绑定错误")
	}
}

// TestOnStopTimeoutDoesNotBlockOthers 验证卡死的 OnStop 只消耗关停预算、
// 被记录为超时错误，后续 OnStop 仍会被执行。
func TestOnStopTimeoutDoesNotBlockOthers(t *testing.T) {
	var laterRan atomic.Bool
	blocker := &testModule{name: "blocker", register: func(reg *Registry) error {
		reg.OnStart(StageWorker, func(context.Context) error { return nil })
		reg.OnStop(func(context.Context) error {
			select {} // 永久阻塞，模拟卡死的关停钩子
		})
		return nil
	}}
	infra := &testModule{name: "infra", register: func(reg *Registry) error {
		reg.OnStart(StageInfra, func(context.Context) error { return nil })
		reg.OnStop(func(context.Context) error {
			laterRan.Store(true)
			return nil
		})
		return nil
	}}

	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	app := New([]Module{blocker, infra, gateModule(ready)},
		HTTPAddr("127.0.0.1:0"), ShutdownTimeout(300*time.Millisecond))
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(ctx) }()

	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("启动超时")
	}
	cancel()
	select {
	case err := <-runDone:
		if err == nil || !strings.Contains(err.Error(), "关停超时") || !strings.Contains(err.Error(), "blocker") {
			t.Fatalf("应记录 blocker 关停超时: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("卡死的 OnStop 拖垮了整个关停")
	}
	// 后续 OnStop 在独立 goroutine 里派发，轮询等它落地。
	deadline := time.Now().Add(time.Second)
	for !laterRan.Load() {
		if time.Now().After(deadline) {
			t.Fatal("blocker 之后的 OnStop 未被执行")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRunConsumesServerError 验证运行期 HTTP 服务错误会被 Run 消费：
// 记日志、走关停、并入返回错误。
func TestRunConsumesServerError(t *testing.T) {
	ready := make(chan struct{})
	app := New([]Module{gateModule(ready)}, HTTPAddr("127.0.0.1:0"), ShutdownTimeout(time.Second))
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(context.Background()) }()

	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("启动超时")
	}
	app.serverErr <- errors.New("accept 循环崩溃")
	select {
	case err := <-runDone:
		if err == nil || !strings.Contains(err.Error(), "HTTP 服务异常退出") ||
			!strings.Contains(err.Error(), "accept 循环崩溃") {
			t.Fatalf("运行期 server 错误应并入返回错误: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run 未消费运行期 server 错误")
	}
}

// TestBuildServerDefaultsAndOverride 验证 http.Server 的安全默认超时，
// 以及 HTTPServer 选项可覆盖。
func TestBuildServerDefaultsAndOverride(t *testing.T) {
	s := New(nil).buildServer(nil)
	if s.ReadTimeout != 60*time.Second || s.WriteTimeout != 60*time.Second ||
		s.IdleTimeout != 120*time.Second || s.ReadHeaderTimeout != 10*time.Second {
		t.Fatalf("默认超时不符: read=%v write=%v idle=%v header=%v",
			s.ReadTimeout, s.WriteTimeout, s.IdleTimeout, s.ReadHeaderTimeout)
	}

	s2 := New(nil, HTTPServer(func(srv *http.Server) {
		srv.ReadTimeout = 5 * time.Second
	})).buildServer(nil)
	if s2.ReadTimeout != 5*time.Second {
		t.Fatalf("HTTPServer 选项未生效: %v", s2.ReadTimeout)
	}
	if s2.WriteTimeout != 60*time.Second {
		t.Fatalf("未覆盖的默认值不应改变: %v", s2.WriteTimeout)
	}
}

// fakeBus 记录订阅，验证消费者装配。
type fakeBus struct {
	mu     sync.Mutex
	topics []string
}

type managedFakeBus struct {
	mu       sync.Mutex
	events   []string
	runErr   error
	readyErr error
	runGate  chan struct{}
}

func (b *managedFakeBus) record(event string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, event)
}

func (b *managedFakeBus) Subscribe(string, EventHandler) {}
func (b *managedFakeBus) Connect(context.Context) error {
	b.record("connect")
	return nil
}
func (b *managedFakeBus) Run(ctx context.Context) error {
	b.record("run")
	if b.runGate != nil {
		select {
		case <-b.runGate:
			return b.runErr
		case <-ctx.Done():
			b.record("run-exit")
			return ctx.Err()
		}
	}
	<-ctx.Done()
	b.record("run-exit")
	return ctx.Err()
}
func (b *managedFakeBus) Ready(context.Context) error { return b.readyErr }
func (b *managedFakeBus) Drain(context.Context) error {
	b.record("drain")
	return nil
}
func (b *managedFakeBus) Close(context.Context) error {
	b.record("close")
	return nil
}

func (b *managedFakeBus) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.events)
}

func TestManagedBusLifecycle(t *testing.T) {
	bus := &managedFakeBus{}
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	app := New([]Module{gateModule(ready)}, Bus(bus), HTTPAddr("127.0.0.1:0"), ShutdownTimeout(time.Second))
	go func() { done <- app.Run(ctx) }()

	select {
	case <-ready:
		cancel()
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("等待应用就绪超时")
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	events := bus.snapshot()
	for _, want := range []string{"connect", "run", "drain", "run-exit", "close"} {
		if !slices.Contains(events, want) {
			t.Fatalf("生命周期缺少 %q: %v", want, events)
		}
	}
	index := func(v string) int { return slices.Index(events, v) }
	if index("drain") > index("close") || index("run-exit") > index("close") {
		t.Fatalf("Close 必须在 Drain 和消费循环退出之后: %v", events)
	}
}

func TestManagedBusRunFailureStopsApp(t *testing.T) {
	gate := make(chan struct{})
	bus := &managedFakeBus{runErr: errors.New("broker disconnected"), runGate: gate}
	done := make(chan error, 1)
	app := New(nil, Bus(bus), HTTPAddr("127.0.0.1:0"), ShutdownTimeout(time.Second))
	go func() { done <- app.Run(context.Background()) }()
	close(gate)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "broker disconnected") {
			t.Fatalf("消费循环错误应触发应用关停，实际 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("消费循环失败后应用未关停")
	}
}

func TestManagedBusReadiness(t *testing.T) {
	bus := &managedFakeBus{readyErr: errors.New("broker not ready")}
	app := New(nil, Bus(bus))
	app.registerBusLifecycle()
	app.reg.health.SetReady(true)
	failures := app.reg.health.Ready(context.Background())
	if err := failures["appkit-bus/ready"]; !errors.Is(err, bus.readyErr) {
		t.Fatalf("Broker readiness 未接入应用探针: %v", failures)
	}
}

func (b *fakeBus) Subscribe(topic string, _ EventHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.topics = append(b.topics, topic)
}

// TestBusAssembly 验证 Register 与 Setup 阶段登记的消费者都被订阅到注入的 Bus。
func TestBusAssembly(t *testing.T) {
	bus := &fakeBus{}
	ready := make(chan struct{})
	m := &testModule{name: "billing", register: func(reg *Registry) error {
		reg.Consumer("pay.settled", func(context.Context, Event) error { return nil })
		reg.Setup(func(context.Context) error {
			reg.Consumer("pay.refunded", func(context.Context, Event) error { return nil })
			return nil
		})
		return nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	app := New([]Module{m, gateModule(ready)},
		HTTPAddr("127.0.0.1:0"), Bus(bus), ShutdownTimeout(time.Second))
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(ctx) }()

	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("启动超时")
	}
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run 返回错误: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("关停超时")
	}

	bus.mu.Lock()
	defer bus.mu.Unlock()
	want := []string{"pay.settled", "pay.refunded"}
	if len(bus.topics) != len(want) {
		t.Fatalf("订阅不符: %v", bus.topics)
	}
	for i := range want {
		if bus.topics[i] != want[i] {
			t.Fatalf("第 %d 个订阅应为 %s，实际 %v", i, want[i], bus.topics)
		}
	}
}

// TestBusMissingFailFast 验证登记了消费者却未注入 Bus 时启动期报错，不静默丢弃。
func TestBusMissingFailFast(t *testing.T) {
	m := &testModule{name: "billing", register: func(reg *Registry) error {
		reg.Consumer("pay.settled", func(context.Context, Event) error { return nil })
		return nil
	}}
	app := New([]Module{m}, HTTPAddr("127.0.0.1:0"), ShutdownTimeout(time.Second))
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(context.Background()) }()

	select {
	case err := <-runDone:
		if err == nil || !strings.Contains(err.Error(), "Bus") ||
			!strings.Contains(err.Error(), "billing") {
			t.Fatalf("应 fail-fast 并指出缺 Bus: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("缺 Bus 未及时报错")
	}
}

// captureHandler 是并发安全的 slog.Handler，记录所有消息文本。
type captureHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) has(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Contains(h.msgs, msg)
}

// TestRunInjectsLoggerIntoHealth 验证 Run 把 App 的 logger 注入 health.Registry：
// 就绪检查失败的详情必须落在注入的 logger 上，而非 slog.Default()。
func TestRunInjectsLoggerIntoHealth(t *testing.T) {
	logCap := &captureHandler{}
	ready := make(chan struct{})
	m := &testModule{name: "demo", register: func(reg *Registry) error {
		reg.Health("db", health.CheckFunc(func(context.Context) error {
			return errors.New("db down")
		}))
		return nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app := New([]Module{m, gateModule(ready)},
		Logger(slog.New(logCap)), HTTPAddr("127.0.0.1:0"), ShutdownTimeout(2*time.Second))
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()

	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("启动超时")
	}

	rec := httptest.NewRecorder()
	app.reg.HealthRegistry().ReadyHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("失败检查应返回 503，实际 %d", rec.Code)
	}
	if !logCap.has("readiness check failed") {
		t.Fatal("检查失败详情未落到 App logger：Run 未注入 health.SetLogger")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run 返回错误: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("关停超时")
	}
}
