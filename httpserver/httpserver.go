// Package httpserver 提供根 HTTP 中间件链，标准库风格：接缝只出现
// http.Handler，与 appkit.Middleware(httpserver.Base(log)...) 组合使用。
// 模块内部选用什么 web 框架是模块自己的事，本包不出现 gin 类型。
package httpserver

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/forgeplex/appkit/apperr"
)

const headerRequestID = "X-Request-Id"

// isProbe 判断探针路径。AccessLog 与 OTel 共用：kubelet 每几秒打一次，
// 既不该刷日志也不该产生 span。
func isProbe(path string) bool {
	return path == "/healthz" || path == "/readyz"
}

// Base 返回推荐的根中间件链，外层在前（与 appkit.Middleware 的约定一致）：
// RequestID → Recover → AccessLog → OTel。
func Base(log *slog.Logger) []func(http.Handler) http.Handler {
	return []func(http.Handler) http.Handler{
		RequestID(),
		Recover(log),
		AccessLog(log),
		OTel(),
	}
}

type requestIDKey struct{}

// RequestID 读取 X-Request-Id 请求头（没有则生成 uuid），写入响应头并存进 ctx。
// 放在链最外层，保证 Recover/AccessLog 的日志都带得上 request_id。
func RequestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(headerRequestID)
			if id == "" {
				id = uuid.NewString()
			}
			w.Header().Set(headerRequestID, id)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
		})
	}
}

// RequestIDFrom 取出当前请求的 request id；不存在时返回空串。
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// Recover 捕获下游 panic：记录 error 级日志（含堆栈），并以 problem+json
// 写出 500。http.ErrAbortHandler 是 net/http 认可的中止方式，原样上抛。
// 若响应头已发出，这里的 WriteHeader 无效但无害（net/http 忽略并告警）。
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				p := recover()
				if p == nil {
					return
				}
				if p == http.ErrAbortHandler {
					panic(p)
				}
				log.LogAttrs(r.Context(), slog.LevelError, "panic recovered",
					slog.Any("panic", p),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("request_id", RequestIDFrom(r.Context())),
					slog.String("stack", string(debug.Stack())),
				)
				apperr.WriteProblem(w, apperr.Internal(fmt.Errorf("panic: %v", p)))
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// AccessLog 每请求记录一条访问日志：method/path/status/bytes/duration/request_id。
// 探针路径（/healthz、/readyz）不记录。日志在 defer 里落，panic 上抛的请求
// 也会留下记录（此时状态按 500 记——外层 Recover 会写 500）。
func AccessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isProbe(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			sw := &statusWriter{ResponseWriter: w}
			start := time.Now()
			panicking := true
			defer func() {
				status := sw.status
				if status == 0 {
					// 未显式写状态：正常返回时 net/http 隐式 200。
					status = http.StatusOK
					if panicking {
						status = http.StatusInternalServerError
					}
				}
				log.LogAttrs(r.Context(), slog.LevelInfo, "http access",
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Int("status", status),
					slog.Int64("bytes", sw.bytes),
					slog.Duration("duration", time.Since(start)),
					slog.String("request_id", RequestIDFrom(r.Context())),
				)
			}()
			next.ServeHTTP(sw, r)
			panicking = false
		})
	}
}

// statusWriter 捕获状态码与响应字节数。只记录第一次 WriteHeader
// （与 net/http 忽略后续调用的行为一致）。
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap 供 http.ResponseController 透传 Flush/Hijack 等能力。
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// OTel 用 otelhttp 包装下游，span 名为 "METHOD pattern"。探针路径不产生
// span（与 AccessLog 同一套 isProbe 判断）。
// formatter 会被调用两次：span 起始时 ServeMux 尚未匹配（r.Pattern 为空，
// 先以 method 起名），handler 返回后 otelhttp 用它按 pattern 重命名。
// 带方法的 pattern（含空格）不再重复拼 method。
func OTel() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return otelhttp.NewHandler(next, "http.server",
			otelhttp.WithFilter(func(r *http.Request) bool {
				return !isProbe(r.URL.Path)
			}),
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				pat := r.Pattern
				switch {
				case pat == "":
					return r.Method
				case strings.Contains(pat, " "):
					return pat
				default:
					return r.Method + " " + pat
				}
			}))
	}
}

// WriteError 是服务端错误响应的统一出口：规范化为 apperr 后写 RFC 9457
// 响应；5xx 时记录一条 error 日志（含完整错误链）。整条请求路径只应在
// 这里 log 一次错误——上游层 wrap 后原样上抛即可，不要边 log 边返回。
func WriteError(log *slog.Logger, w http.ResponseWriter, err error) {
	e := apperr.From(err)
	if e.Status() >= 500 {
		log.LogAttrs(context.Background(), slog.LevelError, "request failed",
			slog.String("code", e.Code()),
			slog.Int("status", e.Status()),
			slog.Any("err", err),
		)
	}
	apperr.WriteProblem(w, e)
}
