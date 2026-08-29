package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/forgeplex/appkit/health"
)

func failing(msg string) health.Checker {
	return health.CheckFunc(func(context.Context) error { return errors.New(msg) })
}

func passing() health.Checker {
	return health.CheckFunc(func(context.Context) error { return nil })
}

// captureHandler 收集 slog 记录，供断言失败详情落进了日志。
type captureHandler struct {
	mu   sync.Mutex
	recs []capturedRecord
}

type capturedRecord struct {
	msg   string
	attrs map[string]any
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]any)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, capturedRecord{msg: r.Message, attrs: attrs})
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) records() []capturedRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]capturedRecord, len(h.recs))
	copy(out, h.recs)
	return out
}

func TestReadyHandler(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(r *health.Registry)
		wantStatus  int
		wantBodyKey string // 期望出现在 body 里的失败项名
	}{
		{
			name:        "SetReady 前返回 503",
			setup:       func(*health.Registry) {},
			wantStatus:  http.StatusServiceUnavailable,
			wantBodyKey: "appkit/ready",
		},
		{
			name:       "置 ready 后返回 200",
			setup:      func(r *health.Registry) { r.SetReady(true) },
			wantStatus: http.StatusOK,
		},
		{
			name: "单个 Checker 失败返回 503 且 body 含失败项",
			setup: func(r *health.Registry) {
				r.SetReady(true)
				r.Add("ledger/db", failing("connection refused"))
				r.Add("ledger/bus", passing())
			},
			wantStatus:  http.StatusServiceUnavailable,
			wantBodyKey: "ledger/db",
		},
		{
			name: "全部 Checker 通过返回 200",
			setup: func(r *health.Registry) {
				r.SetReady(true)
				r.Add("ledger/db", passing())
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "ready 又摘除后返回 503",
			setup: func(r *health.Registry) {
				r.SetReady(true)
				r.SetReady(false)
			},
			wantStatus:  http.StatusServiceUnavailable,
			wantBodyKey: "appkit/ready",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := health.NewRegistry()
			reg.SetLogger(slog.New(&captureHandler{}))
			tt.setup(reg)

			rec := httptest.NewRecorder()
			reg.ReadyHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d（body: %s）", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body 非法 JSON: %v", err)
			}
			if tt.wantBodyKey != "" {
				// body 只允许固定文案，不得回显 checker 的原始错误。
				if got := body[tt.wantBodyKey]; got != "unhealthy" {
					t.Errorf("body[%q] = %q, want %q", tt.wantBodyKey, got, "unhealthy")
				}
			}
			if tt.wantStatus == http.StatusOK && body["status"] != "ready" {
				t.Errorf("200 响应应带 status=ready：%v", body)
			}
		})
	}
}

// TestReadyHandlerDoesNotLeakErrors 复现评审场景：/readyz 不得把 checker
// 的 err.Error()（可能含连接串等内部信息）返回给任意调用方；详情只进日志。
func TestReadyHandlerDoesNotLeakErrors(t *testing.T) {
	const secret = "postgres://user:s3cr3t-pa55@10.0.0.7:5432 connection refused"

	cap := &captureHandler{}
	reg := health.NewRegistry()
	reg.SetLogger(slog.New(cap))
	reg.SetReady(true)
	reg.Add("ledger/db", failing(secret))

	rec := httptest.NewRecorder()
	reg.ReadyHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Body.String(); strings.Contains(got, "s3cr3t-pa55") || strings.Contains(got, "connection refused") {
		t.Fatalf("响应体泄漏内部错误详情: %s", got)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body 非法 JSON: %v", err)
	}
	if body["ledger/db"] != "unhealthy" {
		t.Fatalf(`body["ledger/db"] = %q, want "unhealthy"`, body["ledger/db"])
	}

	// 详情必须落进注入的 logger。
	var found bool
	for _, r := range cap.records() {
		if r.msg != "readiness check failed" {
			continue
		}
		if r.attrs["check"] == "ledger/db" && strings.Contains(fmt.Sprint(r.attrs["err"]), secret) {
			found = true
		}
	}
	if !found {
		t.Fatalf("日志缺少失败详情: %+v", cap.records())
	}
}

func TestLiveHandler(t *testing.T) {
	tests := []struct {
		name  string
		setup func(r *health.Registry)
	}{
		{"未就绪时仍 200", func(*health.Registry) {}},
		{"就绪检查失败时仍 200", func(r *health.Registry) {
			r.SetReady(true)
			r.Add("db", failing("down"))
		}},
		{"关停摘流量后仍 200", func(r *health.Registry) {
			r.SetReady(true)
			r.SetReady(false)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := health.NewRegistry()
			reg.SetLogger(slog.New(&captureHandler{}))
			tt.setup(reg)

			rec := httptest.NewRecorder()
			reg.LiveHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

			if rec.Code != http.StatusOK {
				t.Errorf("liveness status = %d, want 200", rec.Code)
			}
		})
	}
}
