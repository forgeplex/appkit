package bootstrap

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/forgeplex/appkit"
)

type profileTestModule struct {
	name     string
	register func(*appkit.Registry) error
}

func (m profileTestModule) Name() string                        { return m.name }
func (m profileTestModule) Register(reg *appkit.Registry) error { return m.register(reg) }

func profileTestOptions(t *testing.T) ProfileOptions {
	t.Helper()
	return ProfileOptions{
		Service:    "profiletest",
		ConfigFile: filepath.Join(t.TempDir(), "absent.yaml"),
	}
}

func TestCoreHeadlessStartReadinessAndShutdown(t *testing.T) {
	options := profileTestOptions(t)
	options.Modules = func(deps ProfileDeps) ([]appkit.Module, error) {
		if deps.Capabilities.BusinessHTTP || deps.Capabilities.PostgreSQL || deps.Capabilities.Bus {
			t.Fatalf("unexpected default capabilities: %+v", deps.Capabilities)
		}
		if deps.Base.Database.URL != "" {
			t.Fatalf("database URL must not escape when capability is disabled: %q", deps.Base.Database.URL)
		}
		return nil, nil
	}
	core, err := NewCore(t.Context(), options)
	if err != nil {
		t.Fatalf("NewCore: %v", err)
	}
	running, err := core.Start(t.Context())
	if err != nil {
		t.Fatalf("Start Headless: %v", err)
	}
	if readiness, err := running.Readiness(t.Context()); err != nil || len(readiness) != 0 {
		t.Fatalf("Readiness = (%v, %v), want empty map", readiness, err)
	}
	if err := running.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := core.Close(context.Background()); err != nil {
		t.Fatalf("Close after shutdown: %v", err)
	}
	if _, err := core.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "只能启动一次") {
		t.Fatalf("second Start error = %v, want single-use error", err)
	}
}

func TestRunningProfileShutdownRespectsBudgetAndWaitFinalizes(t *testing.T) {
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	stopStarted := make(chan struct{})
	options := profileTestOptions(t)
	options.Modules = func(ProfileDeps) ([]appkit.Module, error) {
		return []appkit.Module{profileTestModule{name: "shutdown-budget", register: func(reg *appkit.Registry) error {
			reg.OnStart(appkit.StageInfra, func(context.Context) error { return nil })
			reg.OnStop(func(context.Context) error {
				close(stopStarted)
				<-release // Simulate a non-cooperative hook; Host must stop waiting at its budget.
				return nil
			})
			return nil
		}}}, nil
	}
	core, err := NewCore(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	running, err := core.Start(t.Context())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	shutdownDone := make(chan error, 1)
	startedAt := time.Now()
	go func() { shutdownDone <- running.Shutdown(ctx) }()
	select {
	case <-stopStarted:
	case <-time.After(time.Second):
		t.Fatal("OnStop did not start")
	}
	var shutdownErr error
	select {
	case shutdownErr = <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("Shutdown exceeded its Context budget")
	}
	if !errors.Is(shutdownErr, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want context deadline", shutdownErr)
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("Shutdown took %s past its 100ms budget", elapsed)
	}

	close(release)
	released = true
	if err := running.Wait(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait after timed-out Shutdown = %v, want Host deadline error", err)
	}
}

func TestHeadlessDoesNotLoadHTTPOnlySecurityConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.yaml")
	if err := osWriteFile(path, []byte("env: prod\nsecurity:\n  mode: not_a_mode\n")); err != nil {
		t.Fatal(err)
	}
	options := profileTestOptions(t)
	options.ConfigFile = path
	core, err := NewCore(t.Context(), options)
	if err != nil {
		t.Fatalf("Headless must ignore HTTP-only SecurityMode: %v", err)
	}
	if err := core.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestBusinessHTTPStillRequiresValidatedSecurityMode(t *testing.T) {
	options := profileTestOptions(t)
	options.Capabilities.BusinessHTTP = true
	if _, err := NewCore(t.Context(), options); err == nil || !strings.Contains(err.Error(), "security.mode") {
		t.Fatalf("NewCore error = %v, want explicit security.mode error", err)
	}
}

func TestBusinessHTTPProfileLoadsSecurityAndServesRoutes(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "business-http.yaml")
	if err := osWriteFile(path, []byte("env: dev\naddr: "+addr+"\nsecurity:\n  mode: disabled\n")); err != nil {
		t.Fatal(err)
	}
	options := profileTestOptions(t)
	options.ConfigFile = path
	options.Capabilities.BusinessHTTP = true
	options.Modules = func(ProfileDeps) ([]appkit.Module, error) {
		return []appkit.Module{profileTestModule{name: "public-http", register: func(reg *appkit.Registry) error {
			reg.MountPublic("/public", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			return nil
		}}}, nil
	}
	core, err := NewCore(t.Context(), options)
	if err != nil {
		t.Fatalf("NewCore BusinessHTTP: %v", err)
	}
	running, err := core.Start(t.Context())
	if err != nil {
		t.Fatalf("Start BusinessHTTP: %v", err)
	}
	defer func() { _ = running.Shutdown(context.Background()) }()

	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + addr + "/public")
	if err != nil {
		t.Fatalf("GET public route: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("GET public route status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}

func TestProbeOnlyServesHealthAndReadiness(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	options := profileTestOptions(t)
	options.Target = "selected"
	options.Probe = &ProbeOptions{Addr: addr}
	selectedRegistered := false
	options.Modules = func(ProfileDeps) ([]appkit.Module, error) {
		return []appkit.Module{
			profileTestModule{name: "selected", register: func(*appkit.Registry) error {
				selectedRegistered = true
				return nil
			}},
			profileTestModule{name: "not-selected", register: func(*appkit.Registry) error {
				t.Error("business Target must keep unrelated modules disabled")
				return errors.New("unexpected module registration")
			}},
		}, nil
	}
	core, err := NewCore(t.Context(), options)
	if err != nil {
		t.Fatalf("NewCore: %v", err)
	}
	running, err := core.Start(t.Context())
	if err != nil {
		t.Fatalf("Start Probe-only: %v", err)
	}
	if !selectedRegistered {
		t.Fatal("selected business module was not registered")
	}
	defer func() { _ = running.Shutdown(context.Background()) }()

	client := &http.Client{Timeout: time.Second}
	for _, tc := range []struct {
		path string
		code int
	}{
		{path: "/healthz", code: http.StatusOK},
		{path: "/readyz", code: http.StatusOK},
		{path: "/readyz/extra", code: http.StatusNotFound},
	} {
		resp, err := client.Get("http://" + addr + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.code {
			t.Fatalf("GET %s status = %d, want %d", tc.path, resp.StatusCode, tc.code)
		}
	}
	resp, err := client.Post("http://"+addr+"/readyz", "text/plain", strings.NewReader(""))
	if err != nil {
		t.Fatalf("POST /readyz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /readyz status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

func TestProbeNonLoopbackRequiresExplicitBoundaryAndAuthentication(t *testing.T) {
	options := profileTestOptions(t)
	options.Probe = &ProbeOptions{Addr: "0.0.0.0:18081"}
	if _, err := NewCore(t.Context(), options); err == nil || !strings.Contains(err.Error(), "网络边界") {
		t.Fatalf("NewCore error = %v, want network and authentication gate", err)
	}

	options.Probe.NetworkBoundaryConfirmed = true
	options.Probe.Authenticate = func(next http.Handler) http.Handler { return next }
	core, err := NewCore(t.Context(), options)
	if err != nil {
		t.Fatalf("explicit remote Probe policy: %v", err)
	}
	if err := core.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestProbeRejectsBusinessRoutesAndPprof(t *testing.T) {
	options := profileTestOptions(t)
	options.Probe = &ProbeOptions{Addr: "127.0.0.1:0"}
	options.Modules = func(ProfileDeps) ([]appkit.Module, error) {
		return []appkit.Module{profileTestModule{name: "business", register: func(reg *appkit.Registry) error {
			reg.Mount("/private", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			return nil
		}}}, nil
	}
	core, err := NewCore(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.Start(t.Context()); err == nil || !strings.Contains(err.Error(), `HTTP 路由 "/private"`) {
		t.Fatalf("Start error = %v, want fail-fast route error", err)
	}

	options = profileTestOptions(t)
	options.Probe = &ProbeOptions{Addr: "127.0.0.1:0"}
	options.AppOptions = func(ProfileDeps) []appkit.Option { return []appkit.Option{appkit.Pprof()} }
	core, err = NewCore(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "pprof") {
		t.Fatalf("Start error = %v, want fail-fast pprof error", err)
	}
}

func TestDisabledBusAndMigrationCapabilitiesFailFast(t *testing.T) {
	options := profileTestOptions(t)
	options.Modules = func(ProfileDeps) ([]appkit.Module, error) {
		return []appkit.Module{profileTestModule{name: "consumer", register: func(reg *appkit.Registry) error {
			reg.Consumer("topic", func(context.Context, appkit.Event) error { return nil })
			return nil
		}}}, nil
	}
	core, err := NewCore(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "未通过 appkit.Bus 注入") {
		t.Fatalf("Start error = %v, want disabled Bus capability error", err)
	}

	options = profileTestOptions(t)
	options.Modules = func(ProfileDeps) ([]appkit.Module, error) {
		return []appkit.Module{profileTestModule{name: "migrator", register: func(reg *appkit.Registry) error {
			reg.Migrations("sample", fs.FS(fstest.MapFS{"0001.sql": &fstest.MapFile{Data: []byte("SELECT 1")}}))
			return nil
		}}}, nil
	}
	core, err = NewCore(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.Start(t.Context()); err == nil || !strings.Contains(err.Error(), `模块 "migrator" 声明了迁移`) {
		t.Fatalf("Start error = %v, want disabled migration capability error", err)
	}
}

func TestMigrationsCapabilityRequiresPostgreSQL(t *testing.T) {
	options := profileTestOptions(t)
	options.Capabilities.Migrations = true
	if _, err := NewCore(t.Context(), options); err == nil || !strings.Contains(err.Error(), "需要 PostgreSQL") {
		t.Fatalf("NewCore error = %v, want capability dependency error", err)
	}
}

func TestPostgreSQLInitializationErrorRedactsDSN(t *testing.T) {
	secret := "postgres://profile-user:profile-password@/missing?sslmode=disable"
	path := filepath.Join(t.TempDir(), "database.yaml")
	if err := os.WriteFile(path, []byte("database:\n  url: \""+secret+"\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	options := profileTestOptions(t)
	options.ConfigFile = path
	options.Capabilities.PostgreSQL = true
	if _, err := NewCore(t.Context(), options); err == nil || strings.Contains(err.Error(), "profile-password") {
		t.Fatalf("NewCore error = %v, want safe PostgreSQL initialization error", err)
	}
}

func TestProfileModuleValidationRejectsNilAndDuplicateNames(t *testing.T) {
	for _, tc := range []struct {
		name    string
		modules []appkit.Module
		want    string
	}{
		{name: "nil", modules: []appkit.Module{nil}, want: "为 nil"},
		{name: "duplicate", modules: []appkit.Module{
			profileTestModule{name: "same"}, profileTestModule{name: "same"},
		}, want: "重复声明模块"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := profileTestOptions(t)
			options.Modules = func(ProfileDeps) ([]appkit.Module, error) { return tc.modules, nil }
			core, err := NewCore(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := core.Start(t.Context()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Start error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestOneShotExecuteAlwaysShutsDown(t *testing.T) {
	options := profileTestOptions(t)
	var stopped bool
	options.Modules = func(ProfileDeps) ([]appkit.Module, error) {
		return []appkit.Module{profileTestModule{name: "oneshot", register: func(reg *appkit.Registry) error {
			reg.OnStart(appkit.StageInfra, func(context.Context) error { return nil })
			reg.OnStop(func(context.Context) error { stopped = true; return nil })
			return nil
		}}}, nil
	}
	core, err := NewCore(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("one-shot failed")
	err = core.Execute(t.Context(), func(ctx context.Context, deps ProfileDeps) error {
		if deps.Log == nil {
			t.Fatal("One-shot must receive initialized logger")
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Execute error = %v, want wrapped action error", err)
	}
	if !stopped {
		t.Fatal("One-shot error must still shut down the Host")
	}
}

func TestOneShotRejectsListeners(t *testing.T) {
	options := profileTestOptions(t)
	options.Probe = &ProbeOptions{Addr: "127.0.0.1:0"}
	core, err := NewCore(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.Execute(t.Context(), func(context.Context, ProfileDeps) error { return nil }); err == nil || !strings.Contains(err.Error(), "One-shot Profile 不支持") {
		t.Fatalf("Execute error = %v, want listener restriction", err)
	}
	if err := core.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestRunnerCanRunOnlyOnce(t *testing.T) {
	runner := NewRunner(ProfileOptions{})
	if err := runner.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "Service 不能为空") {
		t.Fatalf("first Runner.Run error = %v, want invalid service error", err)
	}
	if err := runner.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "只能运行一次") {
		t.Fatalf("second Runner.Run error = %v, want single-use error", err)
	}
}

func TestCoreCleanupStackRunsReverseAndContinuesAfterErrors(t *testing.T) {
	firstErr := errors.New("pool close failed")
	secondErr := errors.New("telemetry flush failed")
	var events []string
	core := &Core{cleanupDone: make(chan struct{})}
	core.addCleanup("Telemetry", func(context.Context) error {
		events = append(events, "telemetry")
		return secondErr
	})
	core.addCleanup("PostgreSQL", func(context.Context) error {
		events = append(events, "pool")
		return firstErr
	})

	err := core.closeResources(context.Background())
	if !errors.Is(err, firstErr) || !errors.Is(err, secondErr) {
		t.Fatalf("cleanup error = %v, want both failures", err)
	}
	if strings.Join(events, ",") != "pool,telemetry" {
		t.Fatalf("cleanup order = %v, want pool then telemetry", events)
	}
	if err := core.closeResources(context.Background()); !errors.Is(err, firstErr) || !errors.Is(err, secondErr) {
		t.Fatalf("repeated Close error = %v, want same joined failures", err)
	}
	if len(events) != 2 {
		t.Fatalf("cleanup hooks ran %d times, want exactly once", len(events))
	}
}

func osWriteFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0600)
}
