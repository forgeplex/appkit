package appkit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forgeplex/appkit/internal/hoststate"
)

var hostStateTestSequence atomic.Uint64

type gatedStateService struct {
	startEntered chan struct{}
	startRelease chan struct{}
	runEntered   chan struct{}
	readyEntered chan struct{}
	readyRelease chan struct{}
	drainEntered chan struct{}
	drainRelease chan struct{}
	closeEntered chan struct{}
	closeRelease chan struct{}
	startOnce    sync.Once
	runOnce      sync.Once
	readyOnce    sync.Once
	drainOnce    sync.Once
	closeOnce    sync.Once
}

func newGatedStateService() *gatedStateService {
	return &gatedStateService{
		startEntered: make(chan struct{}), startRelease: make(chan struct{}),
		runEntered: make(chan struct{}), readyEntered: make(chan struct{}), readyRelease: make(chan struct{}),
		drainEntered: make(chan struct{}), drainRelease: make(chan struct{}),
		closeEntered: make(chan struct{}), closeRelease: make(chan struct{}),
	}
}

func (s *gatedStateService) Start(ctx context.Context) error {
	s.startOnce.Do(func() { close(s.startEntered) })
	select {
	case <-s.startRelease:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *gatedStateService) Run(ctx context.Context) error {
	s.runOnce.Do(func() { close(s.runEntered) })
	<-ctx.Done()
	return ctx.Err()
}

func (s *gatedStateService) Ready(ctx context.Context) error {
	s.readyOnce.Do(func() { close(s.readyEntered) })
	select {
	case <-s.readyRelease:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *gatedStateService) Drain(ctx context.Context) error {
	s.drainOnce.Do(func() { close(s.drainEntered) })
	select {
	case <-s.drainRelease:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *gatedStateService) Close(ctx context.Context) error {
	s.closeOnce.Do(func() { close(s.closeEntered) })
	select {
	case <-s.closeRelease:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newHostStateTestApp(moduleName, serviceName string, service ManagedService, startErr error) *App {
	module := ModuleFunc(moduleName, func(reg *Registry) error {
		if startErr != nil {
			reg.OnStart(StageInfra, func(context.Context) error { return startErr })
		}
		return reg.ManagedService(serviceName, ServiceCritical, func(*Registry) (ManagedService, error) {
			return service, nil
		})
	})
	return newHostTestApp([]Module{module})
}

func nextHostStateTestIdentity() (string, string) {
	id := hostStateTestSequence.Add(1)
	return fmt.Sprintf("host-state-test-%d", id), "managed"
}

func hostStateCount(module, service string, state hoststate.State) int64 {
	for _, count := range hoststate.Snapshot() {
		if count.Module == module && count.Service == service && count.State == state {
			return count.Count
		}
	}
	return 0
}

func awaitHostState(t *testing.T, module, service string, state hoststate.State) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for hostStateCount(module, service, state) != 1 {
		select {
		case <-deadline.C:
			t.Fatalf("ManagedService 状态未到 %q；snapshot=%v", state, hoststate.Snapshot())
		case <-ticker.C:
		}
	}
}

func awaitSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("等待 %s 超时", description)
	}
}

func TestManagedServiceHostLifecycleStates(t *testing.T) {
	module, serviceName := nextHostStateTestIdentity()
	service := newGatedStateService()
	app := newHostStateTestApp(module, serviceName, service, nil)
	type startResult struct {
		host *RunningApp
		err  error
	}
	started := make(chan startResult, 1)
	go func() {
		host, err := app.Start(context.Background())
		started <- startResult{host: host, err: err}
	}()

	awaitSignal(t, service.startEntered, "Start")
	awaitHostState(t, module, serviceName, hoststate.StateStarting)
	close(service.startRelease)
	awaitSignal(t, service.runEntered, "Run")
	awaitHostState(t, module, serviceName, hoststate.StateRunning)
	awaitSignal(t, service.readyEntered, "Ready")
	close(service.readyRelease)
	result := <-started
	if result.err != nil {
		t.Fatalf("Start: %v", result.err)
	}
	awaitHostState(t, module, serviceName, hoststate.StateReady)

	shutdown := make(chan error, 1)
	go func() { shutdown <- result.host.Shutdown(context.Background()) }()
	awaitSignal(t, service.drainEntered, "Drain")
	awaitHostState(t, module, serviceName, hoststate.StateDraining)
	close(service.drainRelease)
	awaitSignal(t, service.closeEntered, "Close")
	awaitHostState(t, module, serviceName, hoststate.StateStopping)
	close(service.closeRelease)
	if err := <-shutdown; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	awaitHostState(t, module, serviceName, hoststate.StateStopped)
}

func TestManagedServiceStateAggregatesAcrossApps(t *testing.T) {
	module, serviceName := nextHostStateTestIdentity()
	firstApp := newHostStateTestApp(module, serviceName, newRecordingManagedService(), nil)
	secondApp := newHostStateTestApp(module, serviceName, newRecordingManagedService(), nil)
	first, err := firstApp.Start(context.Background())
	if err != nil {
		t.Fatalf("first App.Start: %v", err)
	}
	second, err := secondApp.Start(context.Background())
	if err != nil {
		t.Fatalf("second App.Start: %v", err)
	}
	if got := hostStateCount(module, serviceName, hoststate.StateReady); got != 2 {
		t.Fatalf("same module/service ready count across Apps = %d, want 2", got)
	}
	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if got := hostStateCount(module, serviceName, hoststate.StateStopped); got != 1 {
		t.Fatalf("stopped count after first App shutdown = %d, want 1", got)
	}
	if got := hostStateCount(module, serviceName, hoststate.StateReady); got != 1 {
		t.Fatalf("ready count after first App shutdown = %d, want 1", got)
	}
	if err := second.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	if got := hostStateCount(module, serviceName, hoststate.StateStopped); got != 2 {
		t.Fatalf("stopped count after both Apps shutdown = %d, want 2", got)
	}
}

func TestManagedServiceRunFailureDuringDrainRemainsFailedAfterClose(t *testing.T) {
	module, serviceName := nextHostStateTestIdentity()
	service := newRecordingManagedService()
	service.runReturn = make(chan error, 1)
	drainRelease := make(chan struct{})
	service.drainGate = drainRelease
	app := newHostStateTestApp(module, serviceName, service, nil)
	host, err := app.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	runFailure := errors.New("ManagedService Run failed during Drain")
	shutdown := make(chan error, 1)
	go func() { shutdown <- host.Shutdown(context.Background()) }()
	awaitSignal(t, service.drainStarted, "Drain")
	service.runReturn <- runFailure
	awaitSignal(t, service.runExited, "Run exit")
	close(drainRelease)
	if err := <-shutdown; !errors.Is(err, runFailure) {
		t.Fatalf("Shutdown error = %v, want Run failure %v", err, runFailure)
	}
	if got := service.closeCalls.Load(); got != 1 {
		t.Fatalf("Close calls = %d, want 1 successful cleanup", got)
	}
	if got := hostStateCount(module, serviceName, hoststate.StateFailed); got != 1 {
		t.Fatalf("failed state after successful Close = %d, want 1", got)
	}
	if got := hostStateCount(module, serviceName, hoststate.StateStopped); got != 0 {
		t.Fatalf("stopped state after failed Run = %d, want 0", got)
	}
}

func TestManagedServiceFailedAndNotStartedStatesSurviveRollback(t *testing.T) {
	module, serviceName := nextHostStateTestIdentity()
	startFailure := errors.New("managed service start failed")
	service := newRecordingManagedService()
	service.startErr = startFailure
	app := newHostStateTestApp(module, serviceName, service, nil)
	if host, err := app.Start(context.Background()); host != nil || !errors.Is(err, startFailure) {
		t.Fatalf("Start failure result: host=%v err=%v", host, err)
	}
	if got := hostStateCount(module, serviceName, hoststate.StateFailed); got != 1 {
		t.Fatalf("failed state count = %d, want 1", got)
	}

	module, serviceName = nextHostStateTestIdentity()
	readyFailure := errors.New("managed service readiness failed")
	service = newRecordingManagedService()
	service.readyErr = readyFailure
	app = newHostStateTestApp(module, serviceName, service, nil)
	if host, err := app.Start(context.Background()); host != nil || !errors.Is(err, readyFailure) {
		t.Fatalf("Ready failure result: host=%v err=%v", host, err)
	}
	if got := hostStateCount(module, serviceName, hoststate.StateFailed); got != 1 {
		t.Fatalf("failed Ready state count = %d, want 1", got)
	}

	module, serviceName = nextHostStateTestIdentity()
	startFailure = errors.New("earlier infrastructure startup failed")
	service = newRecordingManagedService()
	app = newHostStateTestApp(module, serviceName, service, startFailure)
	if host, err := app.Start(context.Background()); host != nil || !errors.Is(err, startFailure) {
		t.Fatalf("earlier startup failure result: host=%v err=%v", host, err)
	}
	if got := hostStateCount(module, serviceName, hoststate.StateNotStarted); got != 1 {
		t.Fatalf("not_started state count = %d, want 1", got)
	}
	if got := service.closeCalls.Load(); got != 0 {
		t.Fatalf("unstarted service Close calls = %d, want 0", got)
	}
}
