package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/callctx"
)

// captureHandler 收集 slog 记录，供断言日志字段。
type captureHandler struct {
	mu   sync.Mutex
	recs []capturedRecord
}

type capturedRecord struct {
	level slog.Level
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
	h.recs = append(h.recs, capturedRecord{level: r.Level, msg: r.Message, attrs: attrs})
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

func (h *captureHandler) byMsg(msg string) []capturedRecord {
	var out []capturedRecord
	for _, r := range h.records() {
		if r.msg == msg {
			out = append(out, r)
		}
	}
	return out
}

func chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) apperr.Problem {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	var p apperr.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("解析 problem 响应体失败: %v（body=%s）", err, rec.Body.String())
	}
	return p
}

func TestRequestID(t *testing.T) {
	tests := []struct {
		name     string
		inHeader string
	}{
		{name: "透传已有请求头", inHeader: "req-abc-123"},
		{name: "缺失时生成uuid", inHeader: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ctxID string
			h := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctxID = RequestIDFrom(r.Context())
			}))
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			if tt.inHeader != "" {
				req.Header.Set("X-Request-Id", tt.inHeader)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			got := rec.Header().Get("X-Request-Id")
			if got == "" {
				t.Fatal("响应头缺少 X-Request-Id")
			}
			if got != ctxID {
				t.Fatalf("响应头 %q 与 ctx 内 %q 不一致", got, ctxID)
			}
			if tt.inHeader != "" {
				if got != tt.inHeader {
					t.Fatalf("未透传请求头：got %q, want %q", got, tt.inHeader)
				}
			} else if _, err := uuid.Parse(got); err != nil {
				t.Fatalf("生成的 request id 不是 uuid: %q（%v）", got, err)
			}
		})
	}
}

func TestRequestIDFrom_Empty(t *testing.T) {
	if got := RequestIDFrom(context.Background()); got != "" {
		t.Fatalf("空 ctx 应返回空串, got %q", got)
	}
}

// TestRequestIDExtractsWhitelist 验证入站中间件把整个 callctx 白名单取进 ctx，
// 而不只是 request id——租户与 caller 同样要能传到下游模块与事件。
func TestRequestIDExtractsWhitelist(t *testing.T) {
	var got callctx.Meta
	h := RequestID()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = callctx.From(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(callctx.HeaderRequestID, "req-1")
	req.Header.Set(callctx.HeaderTenantID, "acme")
	req.Header.Set(callctx.HeaderCaller, "gateway")
	h.ServeHTTP(httptest.NewRecorder(), req)

	want := callctx.Meta{RequestID: "req-1", TenantID: "acme", Caller: "gateway"}
	if got != want {
		t.Fatalf("白名单提取不符: %+v", got)
	}
}

func TestRecover(t *testing.T) {
	tests := []struct {
		name        string
		handler     http.HandlerFunc
		wantStatus  int
		wantCode    string
		wantErrLogs int
	}{
		{
			name:        "panic恢复为problem",
			handler:     func(http.ResponseWriter, *http.Request) { panic("boom") },
			wantStatus:  http.StatusInternalServerError,
			wantCode:    apperr.CodeInternal,
			wantErrLogs: 1,
		},
		{
			name: "正常请求不干预",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			},
			wantStatus:  http.StatusNoContent,
			wantErrLogs: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cap := &captureHandler{}
			log := slog.New(cap)
			h := Recover(log)(tt.handler)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantCode != "" {
				if p := decodeProblem(t, rec); p.Code != tt.wantCode {
					t.Fatalf("problem code = %q, want %q", p.Code, tt.wantCode)
				}
			}
			errLogs := cap.byMsg("panic recovered")
			if len(errLogs) != tt.wantErrLogs {
				t.Fatalf("error 日志条数 = %d, want %d", len(errLogs), tt.wantErrLogs)
			}
			if tt.wantErrLogs > 0 {
				r := errLogs[0]
				if r.level != slog.LevelError {
					t.Fatalf("日志级别 = %v, want error", r.level)
				}
				stack, _ := r.attrs["stack"].(string)
				if !strings.Contains(stack, "httpserver") {
					t.Fatalf("stack 字段缺失或不含调用栈: %q", stack)
				}
			}
		})
	}
}

func TestRecover_ErrAbortHandler(t *testing.T) {
	h := Recover(slog.New(&captureHandler{}))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if p := recover(); p != http.ErrAbortHandler {
			t.Fatalf("http.ErrAbortHandler 应原样上抛, got %v", p)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestAccessLog(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		handler    http.HandlerFunc
		wantLogged bool
		wantStatus int64
		wantBytes  int64
	}{
		{
			name: "记录显式状态与字节数",
			path: "/orders",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte("created"))
			},
			wantLogged: true,
			wantStatus: http.StatusCreated,
			wantBytes:  7,
		},
		{
			name:       "未写状态按隐式200记",
			path:       "/noop",
			handler:    func(http.ResponseWriter, *http.Request) {},
			wantLogged: true,
			wantStatus: http.StatusOK,
			wantBytes:  0,
		},
		{
			name:       "healthz跳过",
			path:       "/healthz",
			handler:    func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) },
			wantLogged: false,
		},
		{
			name:       "readyz跳过",
			path:       "/readyz",
			handler:    func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) },
			wantLogged: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cap := &captureHandler{}
			log := slog.New(cap)
			h := chain(tt.handler, RequestID(), AccessLog(log))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))

			recs := cap.byMsg("http access")
			if !tt.wantLogged {
				if len(recs) != 0 {
					t.Fatalf("探针路径不应记访问日志, got %d 条", len(recs))
				}
				return
			}
			if len(recs) != 1 {
				t.Fatalf("访问日志条数 = %d, want 1", len(recs))
			}
			a := recs[0].attrs
			if a["method"] != "GET" {
				t.Errorf("method = %v, want GET", a["method"])
			}
			if a["path"] != tt.path {
				t.Errorf("path = %v, want %s", a["path"], tt.path)
			}
			if got, _ := a["status"].(int64); got != tt.wantStatus {
				t.Errorf("status = %v, want %d", a["status"], tt.wantStatus)
			}
			if got, _ := a["bytes"].(int64); got != tt.wantBytes {
				t.Errorf("bytes = %v, want %d", a["bytes"], tt.wantBytes)
			}
			if _, ok := a["duration"]; !ok {
				t.Error("缺少 duration 字段")
			}
			if id, _ := a["request_id"].(string); id == "" || id != rec.Header().Get("X-Request-Id") {
				t.Errorf("request_id = %v, want %q", a["request_id"], rec.Header().Get("X-Request-Id"))
			}
		})
	}
}

func TestWriteError(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantErrLogs int
	}{
		{
			name:       "apperr原样映射",
			err:        apperr.NotFound("account %s not found", "a1"),
			wantStatus: http.StatusNotFound,
			wantCode:   apperr.CodeNotFound,
		},
		{
			name:       "wrap过的apperr保留错误码",
			err:        fmt.Errorf("post entries: %w", apperr.Conflict("version mismatch")),
			wantStatus: http.StatusConflict,
			wantCode:   apperr.CodeConflict,
		},
		{
			name:        "裸error折叠为internal且记日志",
			err:         fmt.Errorf("query accounts: %w", io.ErrUnexpectedEOF),
			wantStatus:  http.StatusInternalServerError,
			wantCode:    apperr.CodeInternal,
			wantErrLogs: 1,
		},
		{
			name:        "5xx的apperr也记日志",
			err:         apperr.Unavailable(io.ErrUnexpectedEOF),
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    apperr.CodeUnavailable,
			wantErrLogs: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cap := &captureHandler{}
			log := slog.New(cap)
			rec := httptest.NewRecorder()
			WriteError(log, rec, tt.err)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if p := decodeProblem(t, rec); p.Code != tt.wantCode {
				t.Fatalf("problem code = %q, want %q", p.Code, tt.wantCode)
			}
			errLogs := cap.byMsg("request failed")
			if len(errLogs) != tt.wantErrLogs {
				t.Fatalf("error 日志条数 = %d, want %d", len(errLogs), tt.wantErrLogs)
			}
			if tt.wantErrLogs > 0 {
				got := fmt.Sprint(errLogs[0].attrs["err"])
				if !strings.Contains(got, tt.err.Error()) && !strings.Contains(tt.err.Error(), got) {
					t.Fatalf("err 字段 = %q, 应含完整错误链 %q", got, tt.err.Error())
				}
			}
		})
	}
}

// TestOTelSpanName 验证路由匹配后 span 被重命名为 method+pattern。
func TestOTelSpanName(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	tests := []struct {
		name     string
		pattern  string
		reqPath  string
		wantSpan string
	}{
		{name: "带方法的pattern不重复拼", pattern: "GET /things/{id}", reqPath: "/things/42", wantSpan: "GET /things/{id}"},
		{name: "无方法的pattern前缀method", pattern: "/plain", reqPath: "/plain", wantSpan: "GET /plain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(sr.Ended())
			mux := http.NewServeMux()
			mux.HandleFunc(tt.pattern, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			h := OTel()(mux)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.reqPath, nil))

			spans := sr.Ended()[before:]
			if len(spans) != 1 {
				t.Fatalf("span 数 = %d, want 1", len(spans))
			}
			if got := spans[0].Name(); got != tt.wantSpan {
				t.Fatalf("span 名 = %q, want %q", got, tt.wantSpan)
			}
		})
	}
}

// TestOTelSkipsProbes 复现评审场景：kubelet 每次探测 /healthz、/readyz
// 都不得产生 span；业务路径必须照常产生。
func TestOTelSkipsProbes(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	tests := []struct {
		name      string
		path      string
		wantSpans int
	}{
		{name: "healthz零span", path: "/healthz", wantSpans: 0},
		{name: "readyz零span", path: "/readyz", wantSpans: 0},
		{name: "业务路径有span", path: "/orders", wantSpans: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(sr.Ended())
			h := OTel()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := len(sr.Ended()) - before; got != tt.wantSpans {
				t.Fatalf("span 数 = %d, want %d", got, tt.wantSpans)
			}
		})
	}
}

// TestBaseChain 用 httptest 走完整中间件链（模拟 App.wrap 的包装方向）。
func TestBaseChain(t *testing.T) {
	cap := &captureHandler{}
	log := slog.New(cap)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /things/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "thing %s (req %s)", r.PathValue("id"), RequestIDFrom(r.Context()))
	})
	mux.HandleFunc("GET /panic", func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	srv := httptest.NewServer(chain(mux, Base(log)...))
	defer srv.Close()

	t.Run("request id 透传全链", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/things/42", nil)
		req.Header.Set("X-Request-Id", "trace-me-1")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got := resp.Header.Get("X-Request-Id"); got != "trace-me-1" {
			t.Fatalf("响应头 X-Request-Id = %q, want trace-me-1", got)
		}
		if want := "thing 42 (req trace-me-1)"; string(body) != want {
			t.Fatalf("body = %q, want %q", body, want)
		}
		access := cap.byMsg("http access")
		if len(access) != 1 {
			t.Fatalf("访问日志条数 = %d, want 1", len(access))
		}
		if id := access[0].attrs["request_id"]; id != "trace-me-1" {
			t.Fatalf("访问日志 request_id = %v, want trace-me-1", id)
		}
	})

	t.Run("panic 恢复为 problem 且访问日志记 500", func(t *testing.T) {
		cap.mu.Lock()
		cap.recs = nil
		cap.mu.Unlock()

		resp, err := srv.Client().Get(srv.URL + "/panic")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
			t.Fatalf("Content-Type = %q, want application/problem+json", ct)
		}
		var p apperr.Problem
		if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
			t.Fatal(err)
		}
		if p.Code != apperr.CodeInternal {
			t.Fatalf("problem code = %q, want %q", p.Code, apperr.CodeInternal)
		}
		if strings.Contains(p.Detail, "kaboom") {
			t.Fatalf("panic 细节不应外泄: %q", p.Detail)
		}
		if n := len(cap.byMsg("panic recovered")); n != 1 {
			t.Fatalf("panic 日志条数 = %d, want 1", n)
		}
		access := cap.byMsg("http access")
		if len(access) != 1 {
			t.Fatalf("访问日志条数 = %d, want 1", len(access))
		}
		if got, _ := access[0].attrs["status"].(int64); got != http.StatusInternalServerError {
			t.Fatalf("访问日志 status = %v, want 500", access[0].attrs["status"])
		}
	})

	t.Run("healthz 不记访问日志", func(t *testing.T) {
		cap.mu.Lock()
		cap.recs = nil
		cap.mu.Unlock()

		resp, err := srv.Client().Get(srv.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if n := len(cap.byMsg("http access")); n != 0 {
			t.Fatalf("healthz 不应有访问日志, got %d 条", n)
		}
	})
}

// mustPrefix 解析 CIDR，测试数据写错当场炸。
func mustPrefix(t *testing.T, cidr string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		t.Fatalf("ParsePrefix(%q): %v", cidr, err)
	}
	return p
}

// TestClientIPMiddleware 锁住信任模型：只有对端本身在可信网段内才信
// 转发头；伪造头永远拿不到答案。
func TestClientIPMiddleware(t *testing.T) {
	lb := mustPrefix(t, "10.0.0.0/8") // 负载均衡网段
	mw := ClientIP(lb)

	tests := []struct {
		name    string
		remote  string            // TCP 对端
		headers map[string]string // 请求头
		want    string            // 期望解析出的客户端 IP；空 = 零值
	}{
		{
			name:   "直连不可信：忽略一切头用对端",
			remote: "203.0.113.9:44444",
			headers: map[string]string{
				"X-Forwarded-For": "1.2.3.4",
				"X-Client-IP":     "1.2.3.4",
			},
			want: "203.0.113.9",
		},
		{
			name:   "可信代理后的 XFF 链",
			remote: "10.0.0.5:39999",
			headers: map[string]string{
				// 客户端注入的伪造在最左，真实链在右：walk 从右到左，
				// 第一个不可信地址是真实客户端。
				"X-Forwarded-For": "6.6.6.6, 198.51.100.23, 10.0.0.9",
			},
			want: "198.51.100.23",
		},
		{
			name:   "XFF 全链可信取最左",
			remote: "10.0.0.5:39999",
			headers: map[string]string{
				"X-Forwarded-For": "10.0.0.20, 10.0.0.10",
			},
			want: "10.0.0.20",
		},
		{
			name:   "X-Client-IP 优先于 XFF",
			remote: "10.0.0.5:39999",
			headers: map[string]string{
				"X-Client-IP":     "198.51.100.77",
				"X-Forwarded-For": "198.51.100.23",
			},
			want: "198.51.100.77",
		},
		{
			name:   "可信代理没带有效头：用对端",
			remote: "10.0.0.5:39999",
			want:   "10.0.0.5",
		},
		{
			name:   "XFF 最右不可解析：不往左走，用对端兜底",
			remote: "10.0.0.5:39999",
			headers: map[string]string{
				"X-Forwarded-For": "1.2.3.4, not-an-ip",
			},
			want: "10.0.0.5",
		},
		{
			name:   "IPv6 对端带方括号",
			remote: "[2001:db8::1]:51234",
			want:   "2001:db8::1",
		},
		{
			name:   "RemoteAddr 解析不出：存零值",
			remote: "",
			want:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got netip.Addr
			h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = callctx.ClientIP(r.Context())
			}))
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.RemoteAddr = tt.remote
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)

			if tt.want == "" {
				if got.IsValid() {
					t.Fatalf("应存零值，得到 %v", got)
				}
				return
			}
			want := netip.MustParseAddr(tt.want)
			if got != want {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
}

// TestClientIPDefaultTrustsPeer 零个可信网段的安全默认：永远用 TCP 对端。
func TestClientIPDefaultTrustsPeer(t *testing.T) {
	var got netip.Addr
	h := ClientIP()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = callctx.ClientIP(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "198.51.100.23:1000"
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if want := netip.MustParseAddr("198.51.100.23"); got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}
