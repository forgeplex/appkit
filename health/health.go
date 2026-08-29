// Package health 提供 liveness/readiness 探针注册表。
//
// 语义（K8s 约定）：/healthz 只反映进程存活，关停期间仍返回 200，防止被提前杀死；
// /readyz 在全部启动钩子完成前、以及收到关停信号后返回 503，用于摘流量。
package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"sync"
	"time"
)

// Checker 是一个就绪检查。返回 error 表示未就绪。
type Checker interface {
	Check(ctx context.Context) error
}

// CheckFunc 把函数适配为 Checker。
type CheckFunc func(ctx context.Context) error

func (f CheckFunc) Check(ctx context.Context) error { return f(ctx) }

// Registry 聚合各模块的就绪检查，并持有全局 ready 状态。并发安全。
type Registry struct {
	mu     sync.RWMutex
	checks map[string]Checker
	ready  bool
	log    *slog.Logger
}

func NewRegistry() *Registry {
	return &Registry{checks: make(map[string]Checker)}
}

// Add 注册一个命名检查。同名覆盖。
func (r *Registry) Add(name string, c Checker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks[name] = c
}

// SetLogger 设置检查失败详情的落点。未设置时用 slog.Default()。
func (r *Registry) SetLogger(log *slog.Logger) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log = log
}

func (r *Registry) logger() *slog.Logger {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.log != nil {
		return r.log
	}
	return slog.Default()
}

// SetReady 设置全局就绪状态。框架在全部 OnStart 完成后置 true，
// 收到关停信号后立即置 false。
func (r *Registry) SetReady(ready bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ready = ready
}

// Ready 执行全部检查。返回 nil 表示可以接流量。
func (r *Registry) Ready(ctx context.Context) map[string]error {
	r.mu.RLock()
	ready := r.ready
	checks := make(map[string]Checker, len(r.checks))
	maps.Copy(checks, r.checks)
	r.mu.RUnlock()

	failures := make(map[string]error)
	if !ready {
		failures["appkit/ready"] = errNotReady
		return failures
	}
	for name, c := range checks {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := c.Check(cctx); err != nil {
			failures[name] = err
		}
		cancel()
	}
	return failures
}

var errNotReady = notReadyError{}

type notReadyError struct{}

func (notReadyError) Error() string { return "尚未就绪或正在关停" }

// LiveHandler 返回 /healthz 处理器：进程活着即 200。
func (r *Registry) LiveHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

// ReadyHandler 返回 /readyz 处理器：未就绪或任一检查失败即 503，body 只列
// 失败项名与固定文案 "unhealthy"——探针任何人都能打，原始错误可能携带内部
// 信息（连接串、拓扑），详情只经 SetLogger 注入的 logger 记录。
func (r *Registry) ReadyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		failures := r.Ready(req.Context())
		status := http.StatusOK
		body := map[string]string{}
		if len(failures) > 0 {
			status = http.StatusServiceUnavailable
			log := r.logger()
			for name, err := range failures {
				body[name] = "unhealthy"
				log.LogAttrs(req.Context(), slog.LevelWarn, "readiness check failed",
					slog.String("check", name),
					slog.Any("err", err),
				)
			}
		} else {
			body["status"] = "ready"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
}
