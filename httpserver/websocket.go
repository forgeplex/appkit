package httpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/contract"
	"github.com/forgeplex/appkit/tx"
)

const (
	webSocketSubprotocol = "appkit.contract.bidi.v1"
	maxFrameOverhead     = 128
)

// WebSocketConfig 为一条 V2 Bidi Stream 定义传输预算。Stream.QueueSize
// 限制每个方向的排队帧数；MaxMessageBytes 限制每帧 JSON payload，因而队列
// 同时具有帧数与字节上限。Hub 必须注册为 Host ManagedService，保证连接可
// 被关停流程追踪。
type WebSocketConfig struct {
	System string
	Method string
	Stream contract.StreamConfig
	// MaxMessageBytes 限制一条 JSON 消息 payload，不含内部 wire envelope。
	MaxMessageBytes int64
	// HandshakeTimeout 限制升级响应写入时间。
	HandshakeTimeout time.Duration
	// WriteTimeout 限制每个 WebSocket frame 的写入时间。
	WriteTimeout time.Duration
	// OriginPatterns 仅用于放行跨 Origin 浏览器请求；匹配请求 scheme 与 host
	// 的同源请求始终允许，缺省拒绝所有跨源请求。禁止使用 "*"。
	OriginPatterns []string
	// Hub 跟踪 hijacked 连接，必须非 nil。
	Hub *WebSocketHub
	// IdentityResolver 可由生成的 Handler 使用，以适配自定义认证器；为空时
	// 生成绑定从 AppKit Actor/ServicePrincipal 读取认证身份。
	IdentityResolver WebSocketIdentityResolver
}

// WebSocketIdentity 是 Upgrade 前由可信认证器解析出的连接身份信息。零值
// ExpiresAt 表示认证器没有提供凭证过期时间。
type WebSocketIdentity struct {
	Subject   string
	ExpiresAt time.Time
}

// WebSocketIdentityResolver 只能从已认证的请求 Context 构造身份；不得读取
// URL、query string 或 WebSocket frame 中的身份声明。
type WebSocketIdentityResolver func(context.Context) (WebSocketIdentity, error)

// WebSocketHandler 处理一条 Bidi Stream。与本地 contract.Stream 的服务端
// 视角一致：Send/Receive 类型顺序反转。
type WebSocketHandler[Send, Receive any] func(context.Context, contract.Stream[Receive, Send]) error

// NewWebSocketHandler 创建 JSON text-frame Bidi handler。调用方必须先把结果
// 挂到 Registry 的 MountAuthenticated、MountPermission 或
// MountInternalService 路由；该 handler 还会独立要求可信 Actor/ServicePrincipal。
func NewWebSocketHandler[Send, Receive any](
	cfg WebSocketConfig,
	identity WebSocketIdentityResolver,
	handler WebSocketHandler[Send, Receive],
) (http.Handler, error) {
	if err := validateWebSocketConfig(cfg); err != nil {
		return nil, err
	}
	if identity == nil {
		return nil, apperr.InvalidArgument("httpserver WebSocket: identity resolver must not be nil")
	}
	if handler == nil {
		return nil, apperr.InvalidArgument("httpserver WebSocket: handler must not be nil")
	}
	cfg.OriginPatterns = append([]string(nil), cfg.OriginPatterns...)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveWebSocket[Send, Receive](w, r, cfg, identity, handler)
	}), nil
}

func validateWebSocketConfig(cfg WebSocketConfig) error {
	switch {
	case strings.TrimSpace(cfg.System) == "":
		return apperr.InvalidArgument("httpserver WebSocket: System must not be empty")
	case strings.TrimSpace(cfg.Method) == "":
		return apperr.InvalidArgument("httpserver WebSocket: Method must not be empty")
	case cfg.Stream.MaxDuration <= 0:
		return apperr.InvalidArgument("httpserver WebSocket: Stream.MaxDuration must be positive")
	case cfg.Stream.IdleTimeout < 0:
		return apperr.InvalidArgument("httpserver WebSocket: Stream.IdleTimeout must not be negative")
	case cfg.Stream.CloseTimeout <= 0:
		return apperr.InvalidArgument("httpserver WebSocket: Stream.CloseTimeout must be positive")
	case cfg.Stream.QueueSize < 0:
		return apperr.InvalidArgument("httpserver WebSocket: Stream.QueueSize must not be negative")
	case cfg.MaxMessageBytes <= 0 || cfg.MaxMessageBytes > int64(^uint64(0)>>1)-maxFrameOverhead:
		return apperr.InvalidArgument("httpserver WebSocket: MaxMessageBytes must be positive and bounded")
	case cfg.HandshakeTimeout <= 0:
		return apperr.InvalidArgument("httpserver WebSocket: HandshakeTimeout must be positive")
	case cfg.WriteTimeout <= 0:
		return apperr.InvalidArgument("httpserver WebSocket: WriteTimeout must be positive")
	case cfg.Hub == nil:
		return apperr.InvalidArgument("httpserver WebSocket: Hub is required for Host drain tracking")
	}
	for _, origin := range cfg.OriginPatterns {
		if strings.TrimSpace(origin) != origin {
			return apperr.InvalidArgument("httpserver WebSocket: OriginPatterns entries must not contain surrounding whitespace")
		}
		patternHost := origin
		if scheme, host, ok := strings.Cut(origin, "://"); ok {
			scheme = strings.ToLower(scheme)
			if scheme != "http" && scheme != "https" {
				return apperr.InvalidArgument("httpserver WebSocket: OriginPatterns scheme must be http or https")
			}
			patternHost = host
		} else if strings.Contains(origin, "://") {
			return apperr.InvalidArgument("httpserver WebSocket: OriginPatterns scheme must be http or https")
		}
		if strings.TrimSpace(origin) == "" || patternHost == "" || strings.Trim(patternHost, "*.") == "" || strings.ContainsAny(patternHost, "/?#@") {
			return apperr.InvalidArgument("httpserver WebSocket: OriginPatterns must not contain an empty pattern or wildcard-all")
		}
		if _, err := path.Match(strings.ToLower(origin), "example.invalid"); err != nil {
			return apperr.InvalidArgument("httpserver WebSocket: invalid OriginPatterns entry")
		}
	}
	return nil
}

// WebSocketHub 是可直接作为 appkit.ManagedService 返回的连接注册表（依靠 Go
// 接口的结构化实现）。在模块 Register 中通过 Registry.ManagedService 注册。
// Start 打开连接入口，Drain 停止新升级并按预算发送 GoingAway/强制关停。
type WebSocketHub struct {
	mu          sync.Mutex
	started     bool
	accepting   bool
	closed      bool
	pending     map[*webSocketReservation]struct{}
	connections map[*managedWebSocket]struct{}
	changed     chan struct{}
}

// NewWebSocketHub 创建尚未 Start 的 WebSocket 连接注册表。
func NewWebSocketHub() *WebSocketHub {
	return &WebSocketHub{
		pending:     make(map[*webSocketReservation]struct{}),
		connections: make(map[*managedWebSocket]struct{}),
		changed:     make(chan struct{}),
	}
}

// Start 打开升级入口；Host 在此之前启动 HTTP Listener，因此未就绪连接会被
// fail-closed 拒绝。
func (h *WebSocketHub) Start(ctx context.Context) error {
	if ctx == nil {
		return apperr.InvalidArgument("httpserver WebSocket hub: context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return apperr.Unavailable(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.started {
		return apperr.Conflict("httpserver WebSocket hub cannot be started more than once")
	}
	h.started = true
	h.accepting = true
	h.signalLocked()
	return nil
}

// Run 等待 Host 取消 ManagedService Context；Drain 由 Host 在取消前调用。
func (h *WebSocketHub) Run(ctx context.Context) error {
	if ctx == nil {
		return apperr.InvalidArgument("httpserver WebSocket hub: context must not be nil")
	}
	<-ctx.Done()
	return nil
}

// Ready 报告 Hub 是否已启动且仍接受连接。
func (h *WebSocketHub) Ready(ctx context.Context) error {
	if ctx == nil {
		return apperr.InvalidArgument("httpserver WebSocket hub: context must not be nil")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return apperr.Unavailable(err)
	}
	if !h.started || !h.accepting || h.closed {
		return apperr.Unavailable(errors.New("websocket hub is not accepting connections"))
	}
	return nil
}

// Drain 停止接收新升级，向已建立连接发送 GoingAway，并等待在途连接退出。
// 超出 Host 预算时直接关闭底层 net.Conn，包括已经 Hijack 的连接。
func (h *WebSocketHub) Drain(ctx context.Context) error {
	if ctx == nil {
		return apperr.InvalidArgument("httpserver WebSocket hub: context must not be nil")
	}
	h.mu.Lock()
	h.accepting = false
	connections := make([]*managedWebSocket, 0, len(h.connections))
	for conn := range h.connections {
		connections = append(connections, conn)
	}
	h.signalLocked()
	h.mu.Unlock()
	for _, conn := range connections {
		conn.beginDrain()
	}
	return h.waitEmpty(ctx)
}

// Close 强制关闭剩余连接并等待 HTTP handler 清理完毕。
func (h *WebSocketHub) Close(ctx context.Context) error {
	if ctx == nil {
		return apperr.InvalidArgument("httpserver WebSocket hub: context must not be nil")
	}
	h.mu.Lock()
	h.accepting = false
	h.closed = true
	connections := make([]*managedWebSocket, 0, len(h.connections))
	for conn := range h.connections {
		connections = append(connections, conn)
	}
	reservations := make([]*webSocketReservation, 0, len(h.pending))
	for reservation := range h.pending {
		reservations = append(reservations, reservation)
	}
	h.signalLocked()
	h.mu.Unlock()
	for _, reservation := range reservations {
		reservation.force()
	}
	for _, conn := range connections {
		conn.forceClose()
	}
	return h.waitEmpty(ctx)
}

func (h *WebSocketHub) reserve() (*webSocketReservation, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.started || !h.accepting || h.closed {
		return nil, apperr.Unavailable(errors.New("websocket hub is draining"))
	}
	if h.pending == nil {
		h.pending = make(map[*webSocketReservation]struct{})
	}
	if h.connections == nil {
		h.connections = make(map[*managedWebSocket]struct{})
	}
	r := &webSocketReservation{hub: h}
	h.pending[r] = struct{}{}
	h.signalLocked()
	return r, nil
}

func (h *WebSocketHub) attach(r *webSocketReservation, conn *managedWebSocket) bool {
	h.mu.Lock()
	if _, ok := h.pending[r]; !ok {
		h.mu.Unlock()
		return false
	}
	delete(h.pending, r)
	if h.closed {
		h.signalLocked()
		h.mu.Unlock()
		return false
	}
	conn.hub = h
	h.connections[conn] = struct{}{}
	draining := !h.accepting
	h.signalLocked()
	h.mu.Unlock()
	if draining {
		conn.beginDrain()
	}
	return true
}

func (h *WebSocketHub) release(r *webSocketReservation) {
	h.mu.Lock()
	if _, ok := h.pending[r]; ok {
		delete(h.pending, r)
		h.signalLocked()
	}
	h.mu.Unlock()
}

func (h *WebSocketHub) remove(conn *managedWebSocket) {
	h.mu.Lock()
	if _, ok := h.connections[conn]; ok {
		delete(h.connections, conn)
		h.signalLocked()
	}
	h.mu.Unlock()
}

func (h *WebSocketHub) signalLocked() {
	if h.changed != nil {
		close(h.changed)
	}
	h.changed = make(chan struct{})
}

func (h *WebSocketHub) waitEmpty(ctx context.Context) error {
	for {
		h.mu.Lock()
		if len(h.pending) == 0 && len(h.connections) == 0 {
			h.mu.Unlock()
			return nil
		}
		changed := h.changed
		h.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			h.forceAll()
			return apperr.Unavailable(ctx.Err())
		}
	}
}

func (h *WebSocketHub) forceAll() {
	h.mu.Lock()
	connections := make([]*managedWebSocket, 0, len(h.connections))
	for conn := range h.connections {
		connections = append(connections, conn)
	}
	reservations := make([]*webSocketReservation, 0, len(h.pending))
	for reservation := range h.pending {
		reservations = append(reservations, reservation)
	}
	h.mu.Unlock()
	for _, reservation := range reservations {
		reservation.force()
	}
	for _, conn := range connections {
		conn.forceClose()
	}
}

type webSocketReservation struct {
	hub    *WebSocketHub
	mu     sync.Mutex
	raw    net.Conn
	forced bool
	once   sync.Once
}

func (r *webSocketReservation) capture(conn net.Conn) {
	r.mu.Lock()
	r.raw = conn
	forced := r.forced
	r.mu.Unlock()
	if forced {
		_ = conn.Close()
	}
}

func (r *webSocketReservation) release() {
	r.once.Do(func() { r.hub.release(r) })
}

func (r *webSocketReservation) attach(conn *managedWebSocket) bool {
	ok := false
	r.once.Do(func() { ok = r.hub.attach(r, conn) })
	return ok
}

func (r *webSocketReservation) force() {
	r.mu.Lock()
	r.forced = true
	raw := r.raw
	r.mu.Unlock()
	if raw != nil {
		_ = raw.Close()
	}
}

type managedWebSocket struct {
	hub       *WebSocketHub
	conn      *websocket.Conn
	raw       net.Conn
	cancelIO  context.CancelFunc
	stopApp   func() error
	stopMu    sync.Mutex
	draining  atomic.Bool
	expired   atomic.Bool
	forced    atomic.Bool
	peerClose atomic.Bool
	termMu    sync.Mutex
	termCode  string
	termClose websocket.StatusCode
	drainOnce sync.Once
}

func (c *managedWebSocket) setStopApp(fn func() error) {
	c.stopMu.Lock()
	c.stopApp = fn
	draining := c.draining.Load() || c.forced.Load()
	c.stopMu.Unlock()
	if draining {
		go fn()
	}
}

func (c *managedWebSocket) closeApp() {
	c.stopMu.Lock()
	fn := c.stopApp
	c.stopMu.Unlock()
	if fn != nil {
		_ = fn()
	}
}

func (c *managedWebSocket) beginDrain() {
	c.drainOnce.Do(func() {
		c.draining.Store(true)
		go c.closeApp()
		go func() { _ = c.conn.Close(websocket.StatusGoingAway, "") }()
	})
}

func (c *managedWebSocket) forceClose() {
	c.forced.Store(true)
	c.cancelIO()
	go c.closeApp()
	_ = c.raw.Close()
}

func (c *managedWebSocket) setTerminal(code string, closeCode websocket.StatusCode) {
	c.termMu.Lock()
	if c.termCode == "" {
		c.termCode = safeWireCode(code)
		c.termClose = closeCode
	}
	c.termMu.Unlock()
}

func (c *managedWebSocket) terminal() (string, websocket.StatusCode) {
	c.termMu.Lock()
	defer c.termMu.Unlock()
	return c.termCode, c.termClose
}

type hijackCaptureWriter struct {
	http.ResponseWriter
	reservation *webSocketReservation
}

func (w *hijackCaptureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *hijackCaptureWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.reservation.capture(conn)
	}
	return conn, rw, err
}

func serveWebSocket[Send, Receive any](
	w http.ResponseWriter,
	r *http.Request,
	cfg WebSocketConfig,
	identity WebSocketIdentityResolver,
	handler WebSocketHandler[Send, Receive],
) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeWebSocketProblem(w, apperr.New(apperr.CodeInvalidArgument, http.StatusMethodNotAllowed, "method not allowed"))
		return
	}
	if tx.HasTx(r.Context()) {
		writeWebSocketProblem(w, webSocketTransactionBoundaryError(cfg))
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery || r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		writeWebSocketProblem(w, apperr.InvalidArgument("WebSocket handshake must not include a query or request body"))
		return
	}
	if err := validateWebSocketOrigin(r, cfg.OriginPatterns); err != nil {
		writeWebSocketProblem(w, err)
		return
	}
	if err := validateWebSocketHandshake(r); err != nil {
		writeWebSocketProblem(w, err)
		return
	}
	id, err := resolveWebSocketIdentity(r.Context(), identity)
	if err != nil {
		writeWebSocketProblem(w, err)
		return
	}
	if strings.TrimSpace(id.Subject) == "" {
		writeWebSocketProblem(w, apperr.Unauthenticated("WebSocket authentication required"))
		return
	}
	if !id.ExpiresAt.IsZero() && !id.ExpiresAt.After(time.Now()) {
		writeWebSocketProblem(w, apperr.Unauthenticated("WebSocket credential is expired"))
		return
	}
	connectionBaseCtx := context.WithoutCancel(r.Context())
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(cfg.HandshakeTimeout)); err != nil {
		writeWebSocketProblem(w, apperr.Unavailable(err))
		return
	}
	reservation, err := cfg.Hub.reserve()
	if err != nil {
		writeWebSocketProblem(w, err)
		return
	}
	capture := &hijackCaptureWriter{ResponseWriter: w, reservation: reservation}
	conn, err := websocket.Accept(capture, r, &websocket.AcceptOptions{
		Subprotocols:   []string{webSocketSubprotocol},
		OriginPatterns: cfg.OriginPatterns,
	})
	if err != nil {
		reservation.release()
		return // Accept writes the pre-upgrade protocol error response.
	}
	raw := captureConn(reservation)
	if raw == nil {
		_ = conn.CloseNow()
		reservation.release()
		return
	}
	if err := raw.SetWriteDeadline(time.Time{}); err != nil {
		_ = raw.Close()
		_ = conn.CloseNow()
		reservation.release()
		return
	}
	conn.SetReadLimit(cfg.MaxMessageBytes + maxFrameOverhead)
	ioCtx, cancelIO := context.WithCancel(connectionBaseCtx)
	record := &managedWebSocket{conn: conn, raw: raw, cancelIO: cancelIO, termClose: websocket.StatusInternalError}
	if !reservation.attach(record) {
		cancelIO()
		_ = raw.Close()
		return
	}
	defer func() {
		cancelIO()
		_ = raw.Close()
		cfg.Hub.remove(record)
	}()
	stream, err := contract.OpenLocal[json.RawMessage, json.RawMessage](
		ioCtx, cfg.System, cfg.Method, cfg.Stream,
		func(ctx context.Context, peer contract.Stream[json.RawMessage, json.RawMessage]) error {
			return handler(ctx, webSocketServerStream[Send, Receive]{peer: peer, maxBytes: cfg.MaxMessageBytes})
		},
	)
	if err != nil {
		writeTerminalFrame(ioCtx, conn, cfg.WriteTimeout, wsFrame{Type: "error", Code: safeWireCode(apperr.From(err).Code())})
		_ = conn.Close(websocket.StatusInternalError, "")
		return
	}
	record.setStopApp(stream.Close)
	if !id.ExpiresAt.IsZero() {
		go watchServerWebSocketExpiry(ioCtx, record, stream, id.ExpiresAt)
	}
	readDone := make(chan error, 1)
	go func() {
		err := readWebSocketRequests(ioCtx, conn, stream, record, cfg.MaxMessageBytes)
		readDone <- err
	}()
	writeWebSocketResponses(ioCtx, conn, stream, record, cfg)
	select {
	case <-readDone:
	case <-time.After(cfg.Stream.CloseTimeout):
		_ = raw.Close()
	}
	_ = stream.Close()
}

func captureConn(reservation *webSocketReservation) net.Conn {
	reservation.mu.Lock()
	defer reservation.mu.Unlock()
	return reservation.raw
}

func resolveWebSocketIdentity(ctx context.Context, resolve WebSocketIdentityResolver) (identity WebSocketIdentity, err error) {
	defer func() {
		if recover() != nil {
			identity = WebSocketIdentity{}
			err = apperr.Internal(nil)
		}
	}()
	identity, err = resolve(ctx)
	if err != nil {
		return WebSocketIdentity{}, apperr.From(err)
	}
	return identity, nil
}

func validateWebSocketOrigin(r *http.Request, patterns []string) error {
	values := r.Header.Values("Origin")
	if len(values) > 1 {
		return apperr.InvalidArgument("multiple WebSocket Origin headers")
	}
	if len(values) == 0 || strings.TrimSpace(values[0]) == "" {
		return nil // Non-browser service clients do not send Origin.
	}
	u, err := url.Parse(values[0])
	if err != nil || (!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) || u.Host == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return apperr.InvalidArgument("WebSocket Origin is invalid")
	}
	requestScheme := "http"
	if r.TLS != nil {
		requestScheme = "https"
	}
	if strings.EqualFold(u.Scheme, requestScheme) && strings.EqualFold(u.Host, r.Host) {
		return nil
	}
	for _, pattern := range patterns {
		target := u.Host
		if strings.Contains(pattern, "://") {
			target = u.Scheme + "://" + u.Host
		}
		matched, err := path.Match(strings.ToLower(pattern), strings.ToLower(target))
		if err != nil {
			return apperr.InvalidArgument("WebSocket Origin is invalid")
		}
		if matched {
			return nil
		}
	}
	return apperr.PermissionDenied("WebSocket Origin is not allowed")
}

func validateWebSocketHandshake(r *http.Request) error {
	if !headerHasToken(r.Header, "Connection", "upgrade") || !headerHasToken(r.Header, "Upgrade", "websocket") {
		return apperr.InvalidArgument("WebSocket upgrade headers are required")
	}
	if values := r.Header.Values("Sec-WebSocket-Version"); len(values) != 1 || values[0] != "13" {
		return apperr.InvalidArgument("WebSocket version 13 is required")
	}
	keys := r.Header.Values("Sec-WebSocket-Key")
	if len(keys) != 1 {
		return apperr.InvalidArgument("one WebSocket key is required")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keys[0]))
	if err != nil || len(decoded) != 16 {
		return apperr.InvalidArgument("WebSocket key is invalid")
	}
	if !headerHasToken(r.Header, "Sec-WebSocket-Protocol", webSocketSubprotocol) {
		return apperr.InvalidArgument("required WebSocket subprotocol is missing")
	}
	return nil
}

func headerHasToken(h http.Header, name, want string) bool {
	for _, value := range h.Values(name) {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), want) {
				return true
			}
		}
	}
	return false
}

func writeWebSocketProblem(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	apperr.WriteProblem(w, apperr.From(err))
}

type wsFrame struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
	Code string          `json:"code,omitempty"`
}

func decodeWSFrame(raw []byte, maxBytes int64) (wsFrame, error) {
	var frame wsFrame
	if int64(len(raw)) > maxBytes+maxFrameOverhead {
		return frame, apperr.InvalidArgument("WebSocket frame exceeds the configured limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&frame); err != nil {
		return frame, apperr.InvalidArgument("WebSocket frame is not valid JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return frame, apperr.InvalidArgument("WebSocket frame has trailing data")
	}
	switch frame.Type {
	case "data":
		if len(frame.Data) == 0 || int64(len(frame.Data)) > maxBytes || !json.Valid(frame.Data) || frame.Code != "" {
			return frame, apperr.InvalidArgument("WebSocket data frame is invalid")
		}
	case "half_close":
		if len(frame.Data) != 0 || frame.Code != "" {
			return frame, apperr.InvalidArgument("WebSocket half-close frame is invalid")
		}
	case "end":
		if len(frame.Data) != 0 || frame.Code != "" {
			return frame, apperr.InvalidArgument("WebSocket terminal frame is invalid")
		}
	case "error":
		if len(frame.Data) != 0 || !validWireCode(frame.Code) {
			return frame, apperr.InvalidArgument("WebSocket error frame is invalid")
		}
	default:
		return frame, apperr.InvalidArgument("WebSocket frame type is unsupported")
	}
	return frame, nil
}

func encodeWSFrame(frame wsFrame, maxBytes int64) ([]byte, error) {
	raw, err := json.Marshal(frame)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	if int64(len(raw)) > maxBytes+maxFrameOverhead {
		return nil, apperr.InvalidArgument("WebSocket frame exceeds the configured limit")
	}
	return raw, nil
}

func writeWSFrame(ctx context.Context, conn *websocket.Conn, timeout time.Duration, frame wsFrame, maxBytes int64) error {
	raw, err := encodeWSFrame(frame, maxBytes)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageText, raw); err != nil {
		return apperr.Unavailable(nil)
	}
	return nil
}

func writeTerminalFrame(ctx context.Context, conn *websocket.Conn, timeout time.Duration, frame wsFrame) {
	writeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if raw, err := json.Marshal(frame); err == nil {
		_ = conn.Write(writeCtx, websocket.MessageText, raw)
	}
}

func readWebSocketRequests(
	ctx context.Context,
	conn *websocket.Conn,
	stream contract.ClientStream[json.RawMessage, json.RawMessage],
	record *managedWebSocket,
	maxBytes int64,
) error {
	halfClosed := false
	for {
		typ, raw, err := conn.Read(ctx)
		if err != nil {
			if record.draining.Load() || record.forced.Load() || record.expired.Load() {
				return err
			}
			if websocket.CloseStatus(err) != -1 {
				record.peerClose.Store(true)
				_ = stream.Close()
				return err
			}
			record.setTerminal(apperr.CodeUnavailable, websocket.StatusGoingAway)
			_ = stream.Close()
			return apperr.Unavailable(nil)
		}
		if record.draining.Load() || record.expired.Load() {
			continue
		}
		if typ != websocket.MessageText {
			record.setTerminal(apperr.CodeInvalidArgument, websocket.StatusProtocolError)
			_ = stream.Close()
			return apperr.InvalidArgument("WebSocket frames must use JSON text messages")
		}
		frame, err := decodeWSFrame(raw, maxBytes)
		if err != nil {
			code := apperr.CodeInvalidArgument
			if apperr.Is(err, apperr.CodeInternal) {
				code = apperr.CodeInternal
			}
			record.setTerminal(code, websocket.StatusPolicyViolation)
			_ = stream.Close()
			return err
		}
		if frame.Type != "data" && frame.Type != "half_close" {
			record.setTerminal(apperr.CodeInvalidArgument, websocket.StatusPolicyViolation)
			_ = stream.Close()
			return apperr.InvalidArgument("client sent a server-only WebSocket frame")
		}
		switch frame.Type {
		case "data":
			if halfClosed {
				record.setTerminal(apperr.CodeInvalidArgument, websocket.StatusProtocolError)
				_ = stream.Close()
				return apperr.InvalidArgument("data frame received after half-close")
			}
			if err := stream.Send(ctx, frame.Data); err != nil {
				if errors.Is(err, io.EOF) {
					return nil // The service ended before the client half-closed.
				}
				if record.draining.Load() || record.expired.Load() {
					return err
				}
				record.setTerminal(apperr.From(err).Code(), websocket.StatusPolicyViolation)
				return err
			}
		case "half_close":
			if halfClosed {
				record.setTerminal(apperr.CodeInvalidArgument, websocket.StatusProtocolError)
				_ = stream.Close()
				return apperr.InvalidArgument("duplicate half-close frame")
			}
			halfClosed = true
			if err := stream.CloseSend(ctx); err != nil {
				return err
			}
		}
	}
}

func writeWebSocketResponses(
	ctx context.Context,
	conn *websocket.Conn,
	stream contract.ClientStream[json.RawMessage, json.RawMessage],
	record *managedWebSocket,
	cfg WebSocketConfig,
) {
	for {
		value, err := stream.Recv(ctx)
		if err != nil {
			if record.forced.Load() || record.peerClose.Load() {
				return
			}
			if record.draining.Load() {
				return // Hub owns the GoingAway close handshake.
			}
			code, closeCode := record.terminal()
			if record.expired.Load() {
				code, closeCode = apperr.CodeUnauthenticated, websocket.StatusPolicyViolation
			}
			if code == "" {
				if errors.Is(err, io.EOF) {
					if writeWSFrame(ctx, conn, cfg.WriteTimeout, wsFrame{Type: "end"}, cfg.MaxMessageBytes) == nil {
						_ = conn.Close(websocket.StatusNormalClosure, "")
					}
					return
				}
				code = safeWireCode(apperr.From(err).Code())
				closeCode = websocket.StatusInternalError
			}
			writeTerminalFrame(ctx, conn, cfg.WriteTimeout, wsFrame{Type: "error", Code: safeWireCode(code)})
			_ = conn.Close(closeCode, "")
			return
		}
		if record.draining.Load() || record.expired.Load() {
			continue
		}
		if err := writeWSFrame(ctx, conn, cfg.WriteTimeout, wsFrame{Type: "data", Data: value}, cfg.MaxMessageBytes); err != nil {
			record.setTerminal(apperr.From(err).Code(), websocket.StatusInternalError)
			_ = conn.CloseNow()
			return
		}
	}
}

func watchServerWebSocketExpiry(
	ctx context.Context,
	record *managedWebSocket,
	stream contract.ClientStream[json.RawMessage, json.RawMessage],
	expiresAt time.Time,
) {
	delay := time.Until(expiresAt)
	if delay <= 0 {
		record.expired.Store(true)
		record.setTerminal(apperr.CodeUnauthenticated, websocket.StatusPolicyViolation)
		_ = stream.Close()
		return
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
		record.expired.Store(true)
		record.setTerminal(apperr.CodeUnauthenticated, websocket.StatusPolicyViolation)
		_ = stream.Close()
	}
}

type webSocketServerStream[Send, Receive any] struct {
	peer     contract.Stream[json.RawMessage, json.RawMessage]
	maxBytes int64
}

func (s webSocketServerStream[Send, Receive]) Send(ctx context.Context, value Receive) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return apperr.Internal(err)
	}
	if int64(len(raw)) > s.maxBytes {
		return apperr.InvalidArgument("WebSocket response exceeds the configured message limit")
	}
	return s.peer.Send(ctx, json.RawMessage(raw))
}

func (s webSocketServerStream[Send, Receive]) Recv(ctx context.Context) (Send, error) {
	var value Send
	raw, err := s.peer.Recv(ctx)
	if err != nil {
		return value, err
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, apperr.InvalidArgument("WebSocket request payload is invalid")
	}
	return value, nil
}

// DialSecureWebSocket opens a WSS-only, origin-bound service connection. The
// provider is called once for the handshake; the credential is never refreshed
// or resent after Upgrade. ClientStream contexts remain operation-scoped because
// only the stream root context is used for WebSocket IO.
func DialSecureWebSocket[Send, Receive any](
	ctx context.Context,
	address string,
	cfg WebSocketConfig,
	secure contract.SecureClientOptions,
) (contract.ClientStream[Send, Receive], error) {
	if ctx == nil {
		return nil, apperr.InvalidArgument("httpserver WebSocket dial: context must not be nil")
	}
	if tx.HasTx(ctx) {
		return nil, webSocketTransactionBoundaryError(cfg)
	}
	if err := ctx.Err(); err != nil {
		return nil, apperr.Unavailable(err)
	}
	if err := validateWebSocketClientConfig(cfg); err != nil {
		return nil, err
	}
	wssURL, origin, err := secureWebSocketAddress(address)
	if err != nil {
		return nil, err
	}
	captured := &capturedCredentialProvider{provider: secure.Credentials}
	secure.Credentials = captured
	hc, err := contract.NewSecureHTTPClient(origin, secure)
	if err != nil {
		return nil, err
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, cfg.HandshakeTimeout)
	defer cancelDial()
	conn, response, err := websocket.Dial(dialCtx, wssURL, &websocket.DialOptions{
		HTTPClient:   hc,
		Subprotocols: []string{webSocketSubprotocol},
	})
	if err != nil {
		if response != nil && response.Body != nil {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
			if response.StatusCode >= 400 {
				return nil, apperr.FromProblem(response.StatusCode, body)
			}
		}
		if apperr.Is(err, apperr.CodeUnauthenticated) || apperr.Is(err, apperr.CodePermissionDenied) || apperr.Is(err, apperr.CodeInvalidArgument) {
			return nil, apperr.From(err)
		}
		return nil, apperr.Unavailable(nil)
	}
	if conn.Subprotocol() != webSocketSubprotocol {
		_ = conn.CloseNow()
		return nil, apperr.InvalidArgument("remote WebSocket did not negotiate the AppKit bidi subprotocol")
	}
	expiresAt, ok := captured.lastExpiry()
	if !ok || !expiresAt.After(time.Now()) {
		_ = conn.CloseNow()
		return nil, apperr.Unauthenticated("service credential expiry is unavailable or expired")
	}
	conn.SetReadLimit(cfg.MaxMessageBytes + maxFrameOverhead)
	stream, err := contract.OpenLocal[json.RawMessage, json.RawMessage](
		ctx, cfg.System, cfg.Method, cfg.Stream,
		func(streamCtx context.Context, peer contract.Stream[json.RawMessage, json.RawMessage]) error {
			return runWebSocketClient(streamCtx, conn, peer, expiresAt, cfg)
		},
	)
	if err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	return webSocketClientStream[Send, Receive]{inner: stream, maxBytes: cfg.MaxMessageBytes}, nil
}

func webSocketTransactionBoundaryError(cfg WebSocketConfig) error {
	return apperr.New(apperr.CodeTxBoundary, http.StatusInternalServerError,
		"httpserver WebSocket: cannot open a stream inside a transaction").
		WithDetail("system", cfg.System).
		WithDetail("method", cfg.Method)
}

func validateWebSocketClientConfig(cfg WebSocketConfig) error {
	if strings.TrimSpace(cfg.System) == "" || strings.TrimSpace(cfg.Method) == "" {
		return apperr.InvalidArgument("httpserver WebSocket dial: System and Method are required")
	}
	if cfg.Stream.MaxDuration <= 0 || cfg.Stream.CloseTimeout <= 0 || cfg.Stream.IdleTimeout < 0 || cfg.Stream.QueueSize < 0 {
		return apperr.InvalidArgument("httpserver WebSocket dial: Stream config is invalid")
	}
	if cfg.MaxMessageBytes <= 0 || cfg.MaxMessageBytes > int64(^uint64(0)>>1)-maxFrameOverhead || cfg.HandshakeTimeout <= 0 || cfg.WriteTimeout <= 0 {
		return apperr.InvalidArgument("httpserver WebSocket dial: message and time budgets must be positive and bounded")
	}
	return nil
}

func secureWebSocketAddress(address string) (string, string, error) {
	u, err := url.Parse(address)
	if err != nil || u == nil || u.Scheme != "wss" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", "", apperr.InvalidArgument("secure WebSocket URL must be wss without credentials, query or fragment")
	}
	if u.Port() != "" {
		port := u.Port()
		if port == "" {
			return "", "", apperr.InvalidArgument("secure WebSocket URL has an invalid port")
		}
	}
	origin := (&url.URL{Scheme: "https", Host: u.Host}).String()
	return u.String(), origin, nil
}

type capturedCredentialProvider struct {
	provider  contract.ServiceCredentialProvider
	mu        sync.Mutex
	expiresAt time.Time
	hasValue  bool
}

func (p *capturedCredentialProvider) ServiceCredential(ctx context.Context, scope contract.ServiceScope) (contract.ServiceCredential, error) {
	if p.provider == nil {
		return contract.ServiceCredential{}, apperr.Unauthenticated("service credential provider is required")
	}
	credential, err := p.provider.ServiceCredential(ctx, scope)
	if err == nil {
		p.mu.Lock()
		p.expiresAt, p.hasValue = credential.ExpiresAt, true
		p.mu.Unlock()
	}
	return credential, err
}

func (p *capturedCredentialProvider) lastExpiry() (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.expiresAt, p.hasValue
}

type webSocketClientStream[Send, Receive any] struct {
	inner    contract.ClientStream[json.RawMessage, json.RawMessage]
	maxBytes int64
}

func (s webSocketClientStream[Send, Receive]) Send(ctx context.Context, value Send) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return apperr.Internal(err)
	}
	if int64(len(raw)) > s.maxBytes {
		return apperr.InvalidArgument("WebSocket request exceeds the configured message limit")
	}
	return s.inner.Send(ctx, json.RawMessage(raw))
}

func (s webSocketClientStream[Send, Receive]) Recv(ctx context.Context) (Receive, error) {
	var value Receive
	raw, err := s.inner.Recv(ctx)
	if err != nil {
		return value, err
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, apperr.Internal(err)
	}
	return value, nil
}

func (s webSocketClientStream[Send, Receive]) CloseSend(ctx context.Context) error {
	return s.inner.CloseSend(ctx)
}

func (s webSocketClientStream[Send, Receive]) Close() error { return s.inner.Close() }

func runWebSocketClient(
	ctx context.Context,
	conn *websocket.Conn,
	peer contract.Stream[json.RawMessage, json.RawMessage],
	expiresAt time.Time,
	cfg WebSocketConfig,
) (retErr error) {
	defer func() { _ = conn.CloseNow() }()
	gate := &wsClientWriteGate{conn: conn, timeout: cfg.WriteTimeout, maxBytes: cfg.MaxMessageBytes, expires: expiresAt}
	readDone := make(chan error, 1)
	go func() { readDone <- readWebSocketResponses(ctx, conn, peer, gate, expiresAt, cfg) }()
	timer := time.NewTimer(time.Until(expiresAt))
	defer timer.Stop()
	writeDone := make(chan error, 1)
	go func() { writeDone <- writeWebSocketRequests(ctx, peer, gate, expiresAt, cfg) }()
	for {
		select {
		case err := <-readDone:
			return err
		case err := <-writeDone:
			if err != nil && !errors.Is(err, errWebSocketHalfClosed) {
				return err
			}
			if errors.Is(err, errWebSocketHalfClosed) {
				writeDone = nil
			}
		case <-timer.C:
			gate.expire()
			closeDone := make(chan error, 1)
			go func() { closeDone <- conn.Close(websocket.StatusPolicyViolation, "") }()
			select {
			case <-closeDone:
			case <-time.After(cfg.Stream.CloseTimeout):
			}
			return apperr.Unauthenticated("service credential expired")
		case <-ctx.Done():
			return apperr.Unavailable(ctx.Err())
		}
	}
}

var errWebSocketHalfClosed = errors.New("websocket send direction half-closed")

type wsClientWriteGate struct {
	mu       sync.Mutex
	conn     *websocket.Conn
	timeout  time.Duration
	maxBytes int64
	expires  time.Time
	expired  bool
}

func (g *wsClientWriteGate) write(ctx context.Context, frame wsFrame) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.expired || !g.expires.After(time.Now()) {
		return apperr.Unauthenticated("service credential expired")
	}
	return writeWSFrame(ctx, g.conn, g.timeout, frame, g.maxBytes)
}

func (g *wsClientWriteGate) expire() {
	g.mu.Lock()
	g.expired = true
	g.mu.Unlock()
}

func writeWebSocketRequests(
	ctx context.Context,
	peer contract.Stream[json.RawMessage, json.RawMessage],
	gate *wsClientWriteGate,
	expiresAt time.Time,
	cfg WebSocketConfig,
) error {
	for {
		value, err := peer.Recv(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if err := gate.write(ctx, wsFrame{Type: "half_close"}); err != nil {
					return err
				}
				return errWebSocketHalfClosed
			}
			return err
		}
		if gate.isExpired() || !expiresAt.After(time.Now()) {
			return apperr.Unauthenticated("service credential expired")
		}
		if int64(len(value)) > cfg.MaxMessageBytes {
			return apperr.InvalidArgument("WebSocket request exceeds the configured message limit")
		}
		if err := gate.write(ctx, wsFrame{Type: "data", Data: value}); err != nil {
			return err
		}
	}
}

func (g *wsClientWriteGate) isExpired() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.expired
}

func readWebSocketResponses(
	ctx context.Context,
	conn *websocket.Conn,
	peer contract.Stream[json.RawMessage, json.RawMessage],
	gate *wsClientWriteGate,
	expiresAt time.Time,
	cfg WebSocketConfig,
) error {
	for {
		typ, raw, err := conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != -1 {
				return apperr.Unavailable(nil)
			}
			return apperr.Unavailable(nil)
		}
		if gate.isExpired() || !expiresAt.After(time.Now()) {
			return apperr.Unauthenticated("service credential expired")
		}
		if typ != websocket.MessageText {
			return apperr.InvalidArgument("WebSocket frames must use JSON text messages")
		}
		frame, err := decodeWSFrame(raw, cfg.MaxMessageBytes)
		if err != nil {
			return err
		}
		switch frame.Type {
		case "data":
			if err := peer.Send(ctx, frame.Data); err != nil {
				return err
			}
		case "end":
			return nil
		case "error":
			return remoteWireError(frame.Code)
		default:
			return apperr.InvalidArgument("unexpected WebSocket frame from server")
		}
	}
}

func remoteWireError(code string) error {
	if !validWireCode(code) {
		code = apperr.CodeInternal
	}
	return apperr.New(code, http.StatusInternalServerError, "remote stream failed")
}

func safeWireCode(code string) string {
	if validWireCode(code) {
		return code
	}
	return apperr.CodeInternal
}

func validWireCode(code string) bool {
	if code == "" || len(code) > 64 || code[0] < 'A' || code[0] > 'Z' {
		return false
	}
	for _, ch := range code[1:] {
		if !((ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_') {
			return false
		}
	}
	return true
}
