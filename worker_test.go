package appkit

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// TestWorkerRunsAndStopsWithCtx 验证 Worker 的三件事：起来了、ctx 取消时停、
// 关停等它退出（正常退出不产生错误）。
func TestWorkerRunsAndStopsWithCtx(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	ready := make(chan struct{})
	m := &testModule{name: "billing", register: func(reg *Registry) error {
		reg.Worker("relay", func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			close(stopped)
			return ctx.Err() // context.Canceled 视为正常退出
		})
		return nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	app := New([]Module{m, gateModule(ready)},
		HTTPAddr("127.0.0.1:0"), ShutdownTimeout(2*time.Second))
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(ctx) }()

	waitFor(t, started, "worker 未启动")
	waitFor(t, ready, "启动超时")
	cancel()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("worker 正常退出不应产生错误: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("关停超时")
	}
	waitFor(t, stopped, "worker 未退出")
}

// TestWorkerCrashStopsApp 验证 worker 异常退出会掀翻进程：探针绿着、
// 事件却停摆是最难发现的故障，必须变成显式的关停 + 错误。
func TestWorkerCrashStopsApp(t *testing.T) {
	ready := make(chan struct{})
	boom := make(chan struct{})
	m := &testModule{name: "billing", register: func(reg *Registry) error {
		reg.Worker("relay", func(context.Context) error {
			<-boom
			return errors.New("relay 轮询崩了")
		})
		return nil
	}}
	app := New([]Module{m, gateModule(ready)},
		HTTPAddr("127.0.0.1:0"), ShutdownTimeout(2*time.Second))
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(context.Background()) }()

	waitFor(t, ready, "启动超时")
	close(boom)

	select {
	case err := <-runDone:
		if err == nil || !strings.Contains(err.Error(), "relay 轮询崩了") ||
			!strings.Contains(err.Error(), "billing/relay") {
			t.Fatalf("worker 崩溃应带模块名与根因返回: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker 崩溃未触发关停")
	}
}

// TestWorkerHangDoesNotBlockShutdown 验证不理会 ctx 的 worker 不会吊死关停：
// 超出预算就记录错误继续，进程仍能退出。
func TestWorkerHangDoesNotBlockShutdown(t *testing.T) {
	ready := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	m := &testModule{name: "billing", register: func(reg *Registry) error {
		reg.Worker("stuck", func(context.Context) error {
			<-release // 故意不 select ctx
			return nil
		})
		return nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	app := New([]Module{m, gateModule(ready)},
		HTTPAddr("127.0.0.1:0"), ShutdownTimeout(300*time.Millisecond))
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(ctx) }()

	waitFor(t, ready, "启动超时")
	cancel()

	// 两层守卫同时到期（Worker 的 OnStop 自己 select ctx，框架的 runStop 也
	// 兜了一层超时），谁先赢由调度决定；断言取二者的交集：报错且指名模块。
	select {
	case err := <-runDone:
		if err == nil || !strings.Contains(err.Error(), "billing") {
			t.Fatalf("卡死的 worker 应超时放弃并报错: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("卡死的 worker 吊死了关停")
	}
}

func TestWorkerNotStartedAfterSameStageFailureDoesNotWait(t *testing.T) {
	boom := errors.New("earlier worker stage failed")
	workerCalled := false
	m := &testModule{name: "billing", register: func(reg *Registry) error {
		reg.OnStart(StageWorker, func(context.Context) error { return boom })
		reg.Worker("never-started", func(context.Context) error {
			workerCalled = true
			return nil
		})
		return nil
	}}
	app := New([]Module{m}, HTTPAddr("127.0.0.1:0"), ShutdownTimeout(100*time.Millisecond))
	err := app.Run(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("Run 应保留同 stage 启动根因，实际 %v", err)
	}
	if workerCalled {
		t.Fatal("前序同 stage 钩子失败后不应启动后续 Worker")
	}
	if strings.Contains(err.Error(), "未在关停预算内退出") {
		t.Fatalf("未启动 Worker 不应空等到关停超时: %v", err)
	}
}

// TestMigrationsWithoutMigratorFailFast 验证登记了迁移却没有执行器时启动即报错，
// 且错误里带上「怎么修」——静默跳过迁移会让服务对着旧 schema 跑。
func TestMigrationsWithoutMigratorFailFast(t *testing.T) {
	app := New([]Module{migratingModule()},
		HTTPAddr("127.0.0.1:0"), ShutdownTimeout(time.Second))
	err := app.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "未注入迁移执行器") ||
		!strings.Contains(err.Error(), "billing") ||
		!strings.Contains(err.Error(), "SkipMigrations") {
		t.Fatalf("应 fail-fast 并给出修法: %v", err)
	}
}

// TestSkipMigrationsOptOut 验证显式声明进程外施加迁移后可以正常启动。
func TestSkipMigrationsOptOut(t *testing.T) {
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	app := New([]Module{migratingModule(), gateModule(ready)},
		HTTPAddr("127.0.0.1:0"), SkipMigrations(), ShutdownTimeout(time.Second))
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(ctx) }()

	waitFor(t, ready, "SkipMigrations 下应能正常启动")
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run 返回错误: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("关停超时")
	}
}

// TestSkipMigrationsOverridesMigrator 锁住部署承诺：bootstrap 会统一注入
// Migrator，服务副本再通过 AppOptions 追加 SkipMigrations。两者共存时，
// 显式 skip 必须优先，否则每个副本仍会在滚动发布时尝试 DDL。
func TestSkipMigrationsOverridesMigrator(t *testing.T) {
	called := false
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	app := New([]Module{migratingModule(), gateModule(ready)},
		HTTPAddr("127.0.0.1:0"), ShutdownTimeout(time.Second),
		Migrator(func(context.Context, []MigrationSet) error {
			called = true
			return nil
		}),
		SkipMigrations())
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(ctx) }()

	waitFor(t, ready, "SkipMigrations 与 Migrator 共存时应用未就绪")
	if called {
		t.Fatal("SkipMigrations 应优先于 Migrator，普通 Run 不应执行迁移")
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
}

// TestMigratorReceivesSets 验证注入执行器后拿到全部已声明迁移集。
func TestMigratorReceivesSets(t *testing.T) {
	var got []MigrationSet
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	app := New([]Module{migratingModule(), gateModule(ready)},
		HTTPAddr("127.0.0.1:0"), ShutdownTimeout(time.Second),
		Migrator(func(_ context.Context, sets []MigrationSet) error {
			got = sets
			return nil
		}))
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(ctx) }()

	waitFor(t, ready, "启动超时")
	cancel()
	<-runDone

	if len(got) != 1 || got[0].Schema != "billing" || got[0].Module != "billing" {
		t.Fatalf("迁移集不符: %+v", got)
	}
}

// TestMigrateOnly 验证 Migrate 只跑迁移：不解析依赖、不跑 Setup/OnStart、
// 不监听端口——部署前置 Job 用它，与服务共用同一份模块声明。
func TestMigrateOnly(t *testing.T) {
	var applied []MigrationSet
	touched := false
	side := &testModule{name: "side", register: func(reg *Registry) error {
		reg.Setup(func(context.Context) error { touched = true; return nil })
		reg.OnStart(StageWorker, func(context.Context) error { touched = true; return nil })
		return nil
	}}
	// 端口给个必然绑不上的地址：真去监听就会失败，从而证明它没监听。
	app := New([]Module{migratingModule(), side}, HTTPAddr("256.0.0.1:1"),
		Migrator(func(_ context.Context, sets []MigrationSet) error {
			applied = sets
			return nil
		}))
	if err := app.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(applied) != 1 || applied[0].Schema != "billing" {
		t.Fatalf("迁移集不符: %+v", applied)
	}
	if touched {
		t.Error("Migrate 不应执行 Setup / OnStart")
	}
}

// TestMigrateOnlyIgnoresSkipMigrations 验证显式迁移进程不会被服务副本的
// SkipMigrations 选项短路：Migrate 的唯一职责就是施加迁移。
func TestMigrateOnlyIgnoresSkipMigrations(t *testing.T) {
	called := false
	app := New([]Module{migratingModule()},
		Migrator(func(context.Context, []MigrationSet) error {
			called = true
			return nil
		}),
		SkipMigrations())

	if err := app.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !called {
		t.Fatal("显式 Migrate 不应受 SkipMigrations 影响")
	}
}

// TestMigrateNeedsMigrator 验证迁移模式下缺执行器直接报错——
// 这里 SkipMigrations 没有意义（这个进程存在的唯一目的就是跑迁移）。
func TestMigrateNeedsMigrator(t *testing.T) {
	app := New([]Module{migratingModule()}, SkipMigrations())
	err := app.Migrate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "需要迁移执行器") {
		t.Fatalf("应报缺执行器: %v", err)
	}
}

// TestMigrateRespectsTarget 验证 -target 过滤同样作用于迁移模式：
// 只迁本次部署要起的模块的 schema。
func TestMigrateRespectsTarget(t *testing.T) {
	var applied []MigrationSet
	other := &testModule{name: "other", register: func(reg *Registry) error {
		reg.Migrations("other", fs.FS(fstest.MapFS{}))
		return nil
	}}
	app := New([]Module{migratingModule(), other}, Target("billing"),
		Migrator(func(_ context.Context, sets []MigrationSet) error {
			applied = sets
			return nil
		}))
	if err := app.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(applied) != 1 || applied[0].Schema != "billing" {
		t.Fatalf("target 之外的迁移不该被应用: %+v", applied)
	}
}

func migratingModule() Module {
	return &testModule{name: "billing", register: func(reg *Registry) error {
		reg.Migrations("billing", fs.FS(fstest.MapFS{
			"0001_init.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
		}))
		return nil
	}}
}

func waitFor(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal(msg)
	}
}
