package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/contract"
	"github.com/forgeplex/appkit/internal/metrics"
)

var transportMetricsReader = sdkmetric.NewManualReader()
var transportMetricTestSequence atomic.Uint64

func init() {
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(transportMetricsReader)))
}

func TestSSETransportMetricsRecordFramesAndPartialBytes(t *testing.T) {
	const system = "transport-metrics-sse"
	method := nextTransportMetricMethod("frames-and-bytes")
	cfg := SSEConfig{System: system, Method: method, WriteTimeout: time.Second}
	ctx := context.Background()
	writer := &metricsSSEWriter{header: make(http.Header)}
	controller := http.NewResponseController(writer)
	committed := false

	for _, frame := range []struct {
		body string
		kind string
	}{
		{"id: c1\ndata: {\"token\":\"not-a-label\"}\n\n", metrics.FrameEvent},
		{": keep-alive\n\n", metrics.FrameHeartbeat},
		{"event: error\ndata: {\"code\":\"INTERNAL\"}\n\n", metrics.FrameError},
	} {
		if err := writeSSEFrame(ctx, cfg, writer, controller, frame.body, frame.kind, &committed); err != nil {
			t.Fatalf("writeSSEFrame(%q): %v", frame.kind, err)
		}
	}

	base := map[string]string{
		metrics.AttrSystem: system, metrics.AttrMethod: method,
		metrics.AttrTransport: metrics.TransportSSE,
		metrics.AttrDirection: metrics.DirectionServerToClient,
	}
	for _, kind := range []string{metrics.FrameEvent, metrics.FrameHeartbeat, metrics.FrameError} {
		attrs := copyMetricAttrs(base)
		attrs[metrics.AttrFrameType] = kind
		if got := transportMetricValue(t, "appkit.contract.stream.transport.frame.sent", attrs); got != 1 {
			t.Errorf("SSE %s frames = %d, want 1", kind, got)
		}
	}
	if got := transportMetricValue(t, "appkit.contract.stream.transport.bytes.sent", base); got != int64(len(writer.body)) {
		t.Errorf("SSE bytes = %d, want actual ResponseWriter bytes %d", got, len(writer.body))
	}

	partialMethod := nextTransportMetricMethod("partial-write")
	partialCfg := cfg
	partialCfg.Method = partialMethod
	partialWriter := &metricsSSEWriter{header: make(http.Header), maxWrite: 5}
	partialCommitted := false
	err := writeSSEFrame(ctx, partialCfg, partialWriter, http.NewResponseController(partialWriter), "data: partial\n\n", metrics.FrameEvent, &partialCommitted)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write error = %v, want io.ErrShortWrite", err)
	}
	partialAttrs := map[string]string{
		metrics.AttrSystem: system, metrics.AttrMethod: partialMethod,
		metrics.AttrTransport: metrics.TransportSSE,
		metrics.AttrDirection: metrics.DirectionServerToClient,
	}
	if got := transportMetricValue(t, "appkit.contract.stream.transport.bytes.sent", partialAttrs); got != 5 {
		t.Errorf("partial SSE bytes = %d, want 5", got)
	}
	partialFrameAttrs := copyMetricAttrs(partialAttrs)
	partialFrameAttrs[metrics.AttrFrameType] = metrics.FrameEvent
	if got := transportMetricValue(t, "appkit.contract.stream.transport.frame.sent", partialFrameAttrs); got != 0 {
		t.Errorf("failed SSE frame count = %d, want 0", got)
	}
}

func TestWebSocketTransportMetricsRecordBidirectionalFramesAndBytes(t *testing.T) {
	const system = "transport-metrics-websocket"
	method := nextTransportMetricMethod("round-trip")
	hub := NewWebSocketHub()
	if err := hub.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := transportMetricsWebSocketConfig(hub, system, method)
	handler, err := NewWebSocketHandler[string, string](cfg,
		func(context.Context) (WebSocketIdentity, error) {
			return WebSocketIdentity{Subject: "do-not-record"}, nil
		},
		func(ctx context.Context, peer contract.Stream[string, string]) error {
			request, err := peer.Recv(ctx)
			if err != nil {
				return err
			}
			if _, err := peer.Recv(ctx); !errors.Is(err, io.EOF) {
				return err
			}
			return peer.Send(ctx, "reply:"+request)
		})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = hub.Close(ctx)
	}()

	conn, response, err := websocket.Dial(context.Background(), server.URL+"/bidi", &websocket.DialOptions{
		HTTPClient: server.Client(), Subprotocols: []string{webSocketSubprotocol},
	})
	if err != nil {
		t.Fatalf("WebSocket Dial: %v (response=%v)", err, response)
	}
	defer conn.CloseNow()
	dataRaw, _ := json.Marshal(wsFrame{Type: metrics.FrameData, Data: json.RawMessage(`"hello"`)})
	halfCloseRaw, _ := json.Marshal(wsFrame{Type: metrics.FrameHalfClose})
	readCtx, cancelRead := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelRead()
	if err := conn.Write(readCtx, websocket.MessageText, dataRaw); err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(readCtx, websocket.MessageText, halfCloseRaw); err != nil {
		t.Fatal(err)
	}
	_, responseDataRaw, err := conn.Read(readCtx)
	if err != nil {
		t.Fatalf("read response frame: %v", err)
	}
	var responseFrame wsFrame
	if err := json.Unmarshal(responseDataRaw, &responseFrame); err != nil {
		t.Fatalf("decode response frame: %v", err)
	}
	if responseFrame.Type != metrics.FrameData || string(responseFrame.Data) != `"reply:hello"` {
		t.Fatalf("response frame = %+v", responseFrame)
	}
	_, endRaw, err := conn.Read(readCtx)
	if err != nil {
		t.Fatalf("read terminal frame: %v", err)
	}
	var endFrame wsFrame
	if err := json.Unmarshal(endRaw, &endFrame); err != nil {
		t.Fatalf("decode terminal frame: %v", err)
	}
	if endFrame.Type != metrics.FrameEnd {
		t.Fatalf("terminal frame = %+v, want end", endFrame)
	}
	// The server records the successful end-frame write immediately after
	// conn.Write returns. Receiving the following close handshake proves that
	// write path finished before observing the asynchronous metric.
	if _, _, err := conn.Read(readCtx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("WebSocket close handshake error = %v, want normal closure", err)
	}

	clientToServer := map[string]string{
		metrics.AttrSystem: system, metrics.AttrMethod: method,
		metrics.AttrTransport: metrics.TransportWebSocket,
		metrics.AttrDirection: metrics.DirectionClientToServer,
	}
	serverToClient := copyMetricAttrs(clientToServer)
	serverToClient[metrics.AttrDirection] = metrics.DirectionServerToClient
	if got := transportMetricValue(t, "appkit.contract.stream.transport.bytes.received", clientToServer); got != int64(len(dataRaw)+len(halfCloseRaw)) {
		t.Errorf("received WebSocket payload bytes = %d, want %d", got, len(dataRaw)+len(halfCloseRaw))
	}
	if got := transportMetricValue(t, "appkit.contract.stream.transport.bytes.sent", serverToClient); got != int64(len(responseDataRaw)+len(endRaw)) {
		t.Errorf("sent WebSocket payload bytes = %d, want %d", got, len(responseDataRaw)+len(endRaw))
	}
	for _, item := range []struct {
		name      string
		attrs     map[string]string
		frameType string
	}{
		{"appkit.contract.stream.transport.frame.received", clientToServer, metrics.FrameData},
		{"appkit.contract.stream.transport.frame.received", clientToServer, metrics.FrameHalfClose},
		{"appkit.contract.stream.transport.frame.sent", serverToClient, metrics.FrameData},
		{"appkit.contract.stream.transport.frame.sent", serverToClient, metrics.FrameEnd},
	} {
		attrs := copyMetricAttrs(item.attrs)
		attrs[metrics.AttrFrameType] = item.frameType
		if got := transportMetricValue(t, item.name, attrs); got != 1 {
			t.Errorf("%s %s frames = %d, want 1", item.name, item.frameType, got)
		}
	}
}

func TestWebSocketProtocolErrorAndDrainForcedCloseMetrics(t *testing.T) {
	const system = "transport-metrics-websocket-errors"
	method := nextTransportMetricMethod("protocol-error")
	hub := NewWebSocketHub()
	if err := hub.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := transportMetricsWebSocketConfig(hub, system, method)
	handler, err := NewWebSocketHandler[string, string](cfg,
		func(context.Context) (WebSocketIdentity, error) {
			return WebSocketIdentity{Subject: "do-not-record"}, nil
		},
		func(ctx context.Context, peer contract.Stream[string, string]) error {
			_, err := peer.Recv(ctx)
			return err
		})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	conn, response, err := websocket.Dial(context.Background(), server.URL+"/bidi", &websocket.DialOptions{
		HTTPClient: server.Client(), Subprotocols: []string{webSocketSubprotocol},
	})
	if err != nil {
		t.Fatalf("WebSocket Dial: %v (response=%v)", err, response)
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	invalidRaw, _ := json.Marshal(wsFrame{Type: metrics.FrameEnd})
	if err := conn.Write(ctx, websocket.MessageText, invalidRaw); err != nil {
		t.Fatal(err)
	}
	_, terminalRaw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read sanitized protocol terminal: %v", err)
	}
	var terminal wsFrame
	if err := json.Unmarshal(terminalRaw, &terminal); err != nil {
		t.Fatalf("decode protocol terminal: %v", err)
	}
	if terminal.Type != metrics.FrameError || terminal.Code != apperr.CodeInvalidArgument {
		t.Fatalf("protocol terminal = %+v", terminal)
	}
	protocolAttrs := map[string]string{
		metrics.AttrSystem: system, metrics.AttrMethod: method,
		metrics.AttrTransport: metrics.TransportWebSocket,
		metrics.AttrDirection: metrics.DirectionClientToServer,
		metrics.AttrErrorCode: apperr.CodeInvalidArgument,
	}
	if got := transportMetricValue(t, "appkit.contract.stream.protocol.error", protocolAttrs); got != 1 {
		t.Errorf("protocol errors = %d, want 1", got)
	}

	const forceSystem = "transport-metrics-forced-close"
	forceMethod := nextTransportMetricMethod("deduplicate")
	local, peer := net.Pipe()
	defer peer.Close()
	connection := &managedWebSocket{
		raw: local, cancelIO: func() {}, system: forceSystem, method: forceMethod,
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			connection.forceClose()
		}()
	}
	wg.Wait()
	forceAttrs := map[string]string{
		metrics.AttrSystem: forceSystem, metrics.AttrMethod: forceMethod,
		metrics.AttrTransport: metrics.TransportWebSocket,
	}
	if got := transportMetricValue(t, "appkit.contract.stream.drain.forced_close", forceAttrs); got != 1 {
		t.Errorf("duplicate concurrent forced closes = %d, want 1", got)
	}

	const pendingSystem = "transport-metrics-pending-upgrade"
	pendingMethod := nextTransportMetricMethod("not-managed")
	pendingRaw, pendingPeer := net.Pipe()
	defer pendingPeer.Close()
	reservation := &webSocketReservation{raw: pendingRaw}
	reservation.force()
	pendingAttrs := map[string]string{
		metrics.AttrSystem: pendingSystem, metrics.AttrMethod: pendingMethod,
		metrics.AttrTransport: metrics.TransportWebSocket,
	}
	if got := transportMetricValue(t, "appkit.contract.stream.drain.forced_close", pendingAttrs); got != 0 {
		t.Errorf("pending upgrade forced closes = %d, want 0", got)
	}
}

func TestWebSocketClientAdapterRecordsTransportMetrics(t *testing.T) {
	const system = "transport-metrics-websocket-client"
	method := nextTransportMetricMethod("secure-client")
	hub := NewWebSocketHub()
	if err := hub.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	serverCfg := transportMetricsWebSocketConfig(hub, system, method)
	handler, err := NewWebSocketHandler[string, string](serverCfg,
		func(context.Context) (WebSocketIdentity, error) {
			return WebSocketIdentity{Subject: "do-not-record"}, nil
		},
		func(ctx context.Context, peer contract.Stream[string, string]) error {
			request, err := peer.Recv(ctx)
			if err != nil {
				return err
			}
			if _, err := peer.Recv(ctx); !errors.Is(err, io.EOF) {
				return err
			}
			return peer.Send(ctx, "reply:"+request)
		})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = hub.Close(ctx)
	}()

	address := "wss" + strings.TrimPrefix(server.URL, "https") + "/bidi"
	clientCfg := serverCfg
	clientCfg.Hub = nil
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := DialSecureWebSocket[string, string](ctx, address, clientCfg, contract.SecureClientOptions{
		Audience: "transport-metrics-test",
		Credentials: contract.ServiceCredentialProviderFunc(func(context.Context, contract.ServiceScope) (contract.ServiceCredential, error) {
			return contract.ServiceCredential{Token: "local-test-credential", ExpiresAt: time.Now().Add(time.Minute)}, nil
		}),
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("DialSecureWebSocket: %v", err)
	}
	defer stream.Close()
	if err := stream.Send(ctx, "hello"); err != nil {
		t.Fatalf("stream.Send: %v", err)
	}
	if err := stream.CloseSend(ctx); err != nil {
		t.Fatalf("stream.CloseSend: %v", err)
	}
	if got, err := stream.Recv(ctx); err != nil || got != "reply:hello" {
		t.Fatalf("stream.Recv = %q, %v; want reply", got, err)
	}
	if _, err := stream.Recv(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal Recv error = %v, want EOF", err)
	}

	clientToServer := map[string]string{
		metrics.AttrSystem: system, metrics.AttrMethod: method,
		metrics.AttrTransport: metrics.TransportWebSocket,
		metrics.AttrDirection: metrics.DirectionClientToServer,
	}
	serverToClient := copyMetricAttrs(clientToServer)
	serverToClient[metrics.AttrDirection] = metrics.DirectionServerToClient
	dataWire, _ := encodeWSFrame(wsFrame{Type: metrics.FrameData, Data: json.RawMessage(`"hello"`)}, clientCfg.MaxMessageBytes)
	halfCloseWire, _ := encodeWSFrame(wsFrame{Type: metrics.FrameHalfClose}, clientCfg.MaxMessageBytes)
	replyWire, _ := encodeWSFrame(wsFrame{Type: metrics.FrameData, Data: json.RawMessage(`"reply:hello"`)}, clientCfg.MaxMessageBytes)
	endWire, _ := encodeWSFrame(wsFrame{Type: metrics.FrameEnd}, clientCfg.MaxMessageBytes)
	if got := transportMetricValue(t, "appkit.contract.stream.transport.bytes.sent", clientToServer); got != int64(len(dataWire)+len(halfCloseWire)) {
		t.Errorf("client sent payload bytes = %d, want %d", got, len(dataWire)+len(halfCloseWire))
	}
	if got := transportMetricValue(t, "appkit.contract.stream.transport.bytes.received", serverToClient); got != int64(len(replyWire)+len(endWire)) {
		t.Errorf("client received payload bytes = %d, want %d", got, len(replyWire)+len(endWire))
	}
	for _, item := range []struct {
		name      string
		attrs     map[string]string
		frameType string
	}{
		{"appkit.contract.stream.transport.frame.sent", clientToServer, metrics.FrameData},
		{"appkit.contract.stream.transport.frame.sent", clientToServer, metrics.FrameHalfClose},
		{"appkit.contract.stream.transport.frame.received", serverToClient, metrics.FrameData},
		{"appkit.contract.stream.transport.frame.received", serverToClient, metrics.FrameEnd},
	} {
		attrs := copyMetricAttrs(item.attrs)
		attrs[metrics.AttrFrameType] = item.frameType
		if got := transportMetricValue(t, item.name, attrs); got != 1 {
			t.Errorf("client %s %s frames = %d, want 1", item.name, item.frameType, got)
		}
	}
}

func nextTransportMetricMethod(prefix string) string {
	return prefix + "-" + strconv.FormatUint(transportMetricTestSequence.Add(1), 10)
}

func transportMetricsWebSocketConfig(hub *WebSocketHub, system, method string) WebSocketConfig {
	return WebSocketConfig{
		System: system, Method: method,
		Stream:          contract.StreamConfig{MaxDuration: time.Second, CloseTimeout: time.Second, QueueSize: 1},
		MaxMessageBytes: 1024, HandshakeTimeout: time.Second, WriteTimeout: time.Second,
		Hub: hub,
	}
}

type metricsSSEWriter struct {
	header   http.Header
	body     []byte
	maxWrite int
}

func (w *metricsSSEWriter) Header() http.Header              { return w.header }
func (w *metricsSSEWriter) WriteHeader(int)                  {}
func (w *metricsSSEWriter) Flush()                           {}
func (w *metricsSSEWriter) SetWriteDeadline(time.Time) error { return nil }
func (w *metricsSSEWriter) Write(p []byte) (int, error) {
	n := len(p)
	if w.maxWrite > 0 && n > w.maxWrite {
		n = w.maxWrite
	}
	w.body = append(w.body, p[:n]...)
	return n, nil
}

func transportMetricValue(t *testing.T, name string, want map[string]string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := transportMetricsReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect transport metrics: %v", err)
	}
	for _, scope := range rm.ScopeMetrics {
		if scope.Scope.Name != "github.com/forgeplex/appkit" {
			continue
		}
		for _, metric := range scope.Metrics {
			if metric.Name != name {
				continue
			}
			sum, ok := metric.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %q data = %T, want int64 sum", name, metric.Data)
			}
			for _, point := range sum.DataPoints {
				got := metricAttributeMap(point.Attributes)
				if equalMetricAttributes(got, want) {
					return point.Value
				}
			}
		}
	}
	return 0
}

func metricAttributeMap(set attribute.Set) map[string]string {
	got := make(map[string]string)
	for _, attr := range set.ToSlice() {
		got[string(attr.Key)] = attr.Value.AsString()
	}
	return got
}

func equalMetricAttributes(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for key, value := range want {
		if got[key] != value {
			return false
		}
	}
	return true
}

func copyMetricAttrs(attrs map[string]string) map[string]string {
	copy := make(map[string]string, len(attrs))
	for key, value := range attrs {
		copy[key] = value
	}
	return copy
}
