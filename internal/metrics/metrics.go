// Package metrics 集中定义 appkit 自动埋点的指标。
//
// 指标名与标签集在这里钉死，业务代码不参与——这是刻意的：指标的成本不在
// 采集而在基数，一个把 URL 路径、错误消息或用户 id 当标签的埋点足以打爆
// 后端存储，而这类事故通常由散落各处的"顺手加一个标签"造成。所有标签值
// 要么是代码里的常量（系统名、方法名、topic、任务名），要么是本包收敛过的
// 枚举（outcome、SQL 动词），没有第三种。
//
// 埋点覆盖 RED 三件套（Rate / Errors / Duration，直方图一并给出）的五条路径：
// 契约调用、Local Stream、outbox 投递、周期任务、数据库查询；SSE/WSS 另记录传输帧、
// 应用载荷字节、协议错误与 WebSocket 强制关闭。Host ManagedService 另提供按稳定
// module/service 名与固定状态枚举聚合的当前实例数。HTTP 入站不在此列——otelhttp 已经
// 产出 http.server.request.duration（含 http.route），再埋一遍就是双重计数。
//
// 未配置 OTLP 端点时全局 MeterProvider 是 noop，各 Record 调用近乎零成本，
// 因此埋点无开关。
package metrics

import (
	"cmp"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/forgeplex/appkit/internal/hoststate"
)

// scope 是本框架全部指标的 instrumentation scope。
const scope = "github.com/forgeplex/appkit"

// 标签键。与 OTel 语义约定不冲突的一律加 appkit. 前缀，避免与
// 用户或其它库的同名标签混淆。
const (
	AttrSystem            = "appkit.contract.system"
	AttrMethod            = "appkit.contract.method"
	AttrTopic             = "appkit.outbox.topic"
	AttrSchema            = "appkit.outbox.schema"
	AttrJob               = "appkit.job.name"
	AttrOperation         = "db.operation"
	AttrOutcome           = "appkit.outcome"
	AttrErrorCode         = "appkit.error.code"
	AttrTransport         = "appkit.contract.stream.transport"
	AttrDirection         = "appkit.contract.stream.direction"
	AttrFrameType         = "appkit.contract.stream.frame.type"
	AttrHostServiceModule = "appkit.host.managed_service.module"
	AttrHostServiceName   = "appkit.host.managed_service.name"
	AttrHostServiceState  = "appkit.host.managed_service.state"
)

// outcome 的取值全集。
const (
	OutcomeOK       = "ok"
	OutcomeError    = "error"
	OutcomeSkipped  = "skipped"
	OutcomeCanceled = "canceled"
	OutcomeTimeout  = "timeout"
	OutcomeOther    = "other"

	TransportLocal          = "local"
	TransportSSE            = "sse"
	TransportWebSocket      = "websocket"
	DirectionClientToServer = "client_to_server"
	DirectionServerToClient = "server_to_client"
)

const (
	FrameEvent     = "event"
	FrameHeartbeat = "heartbeat"
	FrameError     = "error"
	FrameData      = "data"
	FrameHalfClose = "half_close"
	FrameEnd       = "end"
)

// durationBuckets 是秒为单位的桶边界（OTel 对 duration 类指标的推荐值）。
// 不用 SDK 默认桶：那套边界是给毫秒设计的，用在秒上几乎所有观测都落进第一个桶。
var durationBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10,
}

type instruments struct {
	contract           metric.Float64Histogram
	httpClient         metric.Int64Counter
	httpClientDuration metric.Float64Histogram
	outbox             metric.Float64Histogram
	dead               metric.Int64Counter
	job                metric.Float64Histogram
	db                 metric.Float64Histogram
	streamActive       metric.Int64UpDownCounter
	streamOpened       metric.Int64Counter
	streamClosed       metric.Int64Counter
	streamCanceled     metric.Int64Counter
	streamMessagesSent metric.Int64Counter
	streamMessagesRecv metric.Int64Counter
	streamDuration     metric.Float64Histogram
	streamFirstMessage metric.Float64Histogram
	streamBackpressure metric.Float64Histogram
	streamFramesSent   metric.Int64Counter
	streamFramesRecv   metric.Int64Counter
	streamBytesSent    metric.Int64Counter
	streamBytesRecv    metric.Int64Counter
	streamProtocolErr  metric.Int64Counter
	streamForcedClose  metric.Int64Counter
	hostServiceState   metric.Int64ObservableGauge
	hostStateCallback  metric.Registration
}

// inst 懒初始化：telemetry.Init 在装配全局 MeterProvider 后会显式预热；
// 未使用该初始化入口的调用方则在首次埋点时绑定当时的全局 provider。
var inst = sync.OnceValue(func() *instruments {
	m := otel.Meter(scope)
	result := &instruments{
		contract: histogram(m, "appkit.contract.call.duration",
			"跨模块契约调用耗时（进程内与远程同口径）"),
		httpClient: counter(m, "appkit.http.client.request", "出站 HTTP 请求数量"),
		httpClientDuration: histogram(m, "appkit.http.client.request.duration",
			"出站 HTTP 请求耗时"),
		outbox: histogram(m, "appkit.outbox.delivery.duration",
			"outbox relay 单条事件的投递耗时"),
		dead: counter(m, "appkit.outbox.dead", "投递重试达上限、转入死信的事件数"),
		job:  histogram(m, "appkit.job.run.duration", "周期任务单轮执行耗时"),
		db:   histogram(m, "appkit.db.query.duration", "数据库查询耗时"),
		streamActive: upDownCounter(m, "appkit.contract.stream.active",
			"当前活动的本地契约流数量", "{stream}"),
		streamOpened: counter(m, "appkit.contract.stream.opened", "成功建立的本地契约流数量"),
		streamClosed: counter(m, "appkit.contract.stream.closed", "已结束的本地契约流数量"),
		streamCanceled: counter(m, "appkit.contract.stream.canceled",
			"因取消而结束的本地契约流数量"),
		streamMessagesSent: counter(m, "appkit.contract.stream.message.sent",
			"本地契约流成功发送的消息数量"),
		streamMessagesRecv: counter(m, "appkit.contract.stream.message.received",
			"本地契约流成功接收的消息数量"),
		streamDuration: histogram(m, "appkit.contract.stream.duration",
			"本地契约流从建立到终态的时长"),
		streamFirstMessage: histogram(m, "appkit.contract.stream.time_to_first_message",
			"本地契约流每个接收方向从建立到首条消息的时长"),
		streamBackpressure: histogram(m, "appkit.contract.stream.send.backpressure.wait",
			"本地契约流发送遇到满队列时的等待时长"),
		streamFramesSent: counter(m, "appkit.contract.stream.transport.frame.sent",
			"流传输层成功写出的应用帧数量"),
		streamFramesRecv: counter(m, "appkit.contract.stream.transport.frame.received",
			"流传输层成功读取的应用帧数量"),
		streamBytesSent: byteCounter(m, "appkit.contract.stream.transport.bytes.sent",
			"流传输层成功交给底层写入器的应用载荷字节数"),
		streamBytesRecv: byteCounter(m, "appkit.contract.stream.transport.bytes.received",
			"流传输层成功读取的应用载荷字节数"),
		streamProtocolErr: counter(m, "appkit.contract.stream.protocol.error",
			"流传输层拒绝的无效应用协议帧数量"),
		streamForcedClose: counter(m, "appkit.contract.stream.drain.forced_close",
			"由 Host 管理的 WebSocket Hub 强制关闭的连接数量"),
	}
	result.hostServiceState, result.hostStateCallback = observeManagedServiceState(m)
	return result
})

// Initialize eagerly binds instruments to the current global MeterProvider.
// telemetry.Init calls this immediately after installing its provider.
func Initialize() { _ = inst() }

func observeManagedServiceState(m metric.Meter) (metric.Int64ObservableGauge, metric.Registration) {
	gauge, err := m.Int64ObservableGauge("appkit.host.managed_service.state",
		metric.WithDescription("当前处于各 ManagedService 生命周期状态的实例数"),
		metric.WithUnit("{service}"))
	if err != nil {
		otel.Handle(err)
		gauge, _ = noop.Meter{}.Int64ObservableGauge("appkit.host.managed_service.state")
		return gauge, nil
	}
	registration, err := m.RegisterCallback(func(ctx context.Context, observer metric.Observer) error {
		for _, count := range hoststate.Snapshot() {
			observer.ObserveInt64(gauge, count.Count, metric.WithAttributes(
				attribute.String(AttrHostServiceModule, count.Module),
				attribute.String(AttrHostServiceName, count.Service),
				attribute.String(AttrHostServiceState, managedServiceState(count.State)),
			))
		}
		return nil
	}, gauge)
	if err != nil {
		otel.Handle(err)
		return gauge, nil
	}
	return gauge, registration
}

func managedServiceState(state hoststate.State) string {
	switch state {
	case hoststate.StateCreated, hoststate.StateStarting, hoststate.StateRunning,
		hoststate.StateReady, hoststate.StateFailed, hoststate.StateDraining,
		hoststate.StateStopping, hoststate.StateStopped, hoststate.StateNotStarted:
		return string(state)
	default:
		return OutcomeOther
	}
}

func histogram(m metric.Meter, name, desc string) metric.Float64Histogram {
	h, err := m.Float64Histogram(name,
		metric.WithDescription(desc),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(durationBuckets...))
	if err != nil {
		// 埋点失败绝不能拖垮业务路径：报给 OTel 的错误处理器，退回 noop。
		otel.Handle(err)
		h, _ = noop.Meter{}.Float64Histogram(name)
	}
	return h
}

func counter(m metric.Meter, name, desc string) metric.Int64Counter {
	c, err := m.Int64Counter(name, metric.WithDescription(desc))
	if err != nil {
		otel.Handle(err)
		c, _ = noop.Meter{}.Int64Counter(name)
	}
	return c
}

func byteCounter(m metric.Meter, name, desc string) metric.Int64Counter {
	c, err := m.Int64Counter(name, metric.WithDescription(desc), metric.WithUnit("By"))
	if err != nil {
		otel.Handle(err)
		c, _ = noop.Meter{}.Int64Counter(name)
	}
	return c
}

func upDownCounter(m metric.Meter, name, desc, unit string) metric.Int64UpDownCounter {
	c, err := m.Int64UpDownCounter(name,
		metric.WithDescription(desc), metric.WithUnit(unit))
	if err != nil {
		otel.Handle(err)
		c, _ = noop.Meter{}.Int64UpDownCounter(name)
	}
	return c
}

// Backlog 是 outbox 待投递积压的一次观测。
type Backlog struct {
	// Pending 是待投递事件数。
	Pending int64
	// OldestAge 是最老一条待投递事件的滞留时长；无积压时为 0。
	// 比条数更适合做告警阈值：积压 1000 条但都在 200ms 内清掉是正常的，
	// 积压 3 条但最老的躺了 20 分钟说明投递已经停了。
	OldestAge time.Duration
}

// ObserveOutboxBacklog 注册 outbox 积压的观测量，返回注销函数。
// observe 在采集周期（默认 60s）被调用，未配置 exporter 时全局 provider
// 是 noop，回调根本不会执行——也就不会有那次数据库查询。
func ObserveOutboxBacklog(schema string, observe func(context.Context) (Backlog, error)) func() {
	m := Meter()
	attrs := metric.WithAttributes(attribute.String(AttrSchema, schema))
	pending, err1 := m.Int64ObservableGauge("appkit.outbox.pending",
		metric.WithDescription("outbox 中待投递的事件数"))
	age, err2 := m.Float64ObservableGauge("appkit.outbox.oldest_pending.age",
		metric.WithDescription("最老一条待投递事件的滞留时长"), metric.WithUnit("s"))
	if err1 != nil || err2 != nil {
		otel.Handle(cmp.Or(err1, err2))
		return func() {}
	}
	reg, err := m.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		b, err := observe(ctx)
		if err != nil {
			return err
		}
		o.ObserveInt64(pending, b.Pending, attrs)
		o.ObserveFloat64(age, b.OldestAge.Seconds(), attrs)
		return nil
	}, pending, age)
	if err != nil {
		otel.Handle(err)
		return func() {}
	}
	return func() { _ = reg.Unregister() }
}

// Meter 返回框架 scope 下的 meter，供框架内需要自定义仪器的地方使用。
func Meter() metric.Meter { return otel.Meter(scope) }

// Outcome 把 error 收敛为 outcome 枚举。
func Outcome(err error) string {
	if err != nil {
		return OutcomeError
	}
	return OutcomeOK
}

// ContractCall 记录一次契约调用。code 是失败时的 apperr 错误码，
// 成功传空串——错误码是有限的常量集合，可以安全地当标签。
func ContractCall(ctx context.Context, system, method, code string, start time.Time) {
	attrs := []attribute.KeyValue{
		attribute.String(AttrSystem, system),
		attribute.String(AttrMethod, method),
		attribute.String(AttrOutcome, outcomeOfCode(code)),
	}
	if code != "" {
		attrs = append(attrs, attribute.String(AttrErrorCode, code))
	}
	inst().contract.Record(ctx, since(start), metric.WithAttributes(attrs...))
}

// HTTPClientCall records one outbound request using only bounded attributes.
func HTTPClientCall(ctx context.Context, method, statusClass string, err error, start time.Time) {
	method = boundedHTTPMethod(method)
	if statusClass != "1xx" && statusClass != "2xx" && statusClass != "3xx" &&
		statusClass != "4xx" && statusClass != "5xx" {
		statusClass = OutcomeOther
	}
	outcome := OutcomeOK
	if err != nil {
		outcome = OutcomeError
		if errors.Is(err, context.Canceled) {
			outcome = OutcomeCanceled
		} else if errors.Is(err, context.DeadlineExceeded) {
			outcome = OutcomeTimeout
		}
	}
	attrs := metric.WithAttributes(
		attribute.String("http.request.method", method),
		attribute.String("http.response.status_class", statusClass),
		attribute.String(AttrOutcome, outcome),
	)
	inst().httpClient.Add(ctx, 1, attrs)
	inst().httpClientDuration.Record(ctx, since(start), attrs)
}

func boundedHTTPMethod(method string) string {
	switch strings.ToUpper(method) {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE":
		return strings.ToUpper(method)
	default:
		return OutcomeOther
	}
}

// StreamOpened 记录一次成功建立的 Local Contract Stream。
func StreamOpened(ctx context.Context, system, method string) {
	attrs := streamAttrs(system, method)
	inst().streamOpened.Add(ctx, 1, metric.WithAttributes(attrs...))
	inst().streamActive.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// StreamClosed 记录唯一终态。错误码只保留框架已知的有限集合，其余值折叠为 other。
func StreamClosed(ctx context.Context, system, method string, err error, duration time.Duration, timedOut bool) {
	outcome := streamOutcome(err)
	if timedOut {
		outcome = OutcomeTimeout
	}
	attrs := append(streamAttrs(system, method), attribute.String(AttrOutcome, outcome))
	if err != nil && !errors.Is(err, io.EOF) {
		attrs = append(attrs, attribute.String(AttrErrorCode, streamErrorCode(err)))
	}
	inst().streamActive.Add(ctx, -1, metric.WithAttributes(streamAttrs(system, method)...))
	inst().streamClosed.Add(ctx, 1, metric.WithAttributes(attrs...))
	inst().streamDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
	if outcome == OutcomeCanceled {
		inst().streamCanceled.Add(ctx, 1, metric.WithAttributes(attrs...))
	}
}

// StreamMessageSent 记录成功进入有界队列的消息，不读取或序列化消息内容。
func StreamMessageSent(ctx context.Context, system, method, direction string) {
	inst().streamMessagesSent.Add(ctx, 1,
		metric.WithAttributes(streamDirectionAttrs(system, method, direction)...))
}

// StreamMessageReceived 记录成功从有界队列取出的消息。
func StreamMessageReceived(ctx context.Context, system, method, direction string) {
	inst().streamMessagesRecv.Add(ctx, 1,
		metric.WithAttributes(streamDirectionAttrs(system, method, direction)...))
}

// StreamFirstMessage 记录某一接收方向首条成功消息的延迟；同一方向只由调用方记录一次。
func StreamFirstMessage(ctx context.Context, system, method, direction string, duration time.Duration) {
	inst().streamFirstMessage.Record(ctx, duration.Seconds(),
		metric.WithAttributes(streamDirectionAttrs(system, method, direction)...))
}

// StreamBackpressureWait 记录发送开始时队列已满后的等待时长。
func StreamBackpressureWait(ctx context.Context, system, method, direction string, duration time.Duration) {
	inst().streamBackpressure.Record(ctx, duration.Seconds(),
		metric.WithAttributes(streamDirectionAttrs(system, method, direction)...))
}

// StreamTransportFrameSent records one successfully written application frame.
// frameType and transport are normalized to fixed transport-specific enums.
func StreamTransportFrameSent(ctx context.Context, system, method, transport, direction, frameType string) {
	inst().streamFramesSent.Add(ctx, 1, metric.WithAttributes(
		streamFrameAttrs(system, method, transport, direction, frameType)...))
}

// StreamTransportFrameReceived records one successfully read application frame.
func StreamTransportFrameReceived(ctx context.Context, system, method, transport, direction, frameType string) {
	inst().streamFramesRecv.Add(ctx, 1, metric.WithAttributes(
		streamFrameAttrs(system, method, transport, direction, frameType)...))
}

// StreamTransportBytesSent records bytes accepted by the transport writer. A
// partial write is recorded by its actual positive byte count.
func StreamTransportBytesSent(ctx context.Context, system, method, transport, direction string, n int64) {
	if n <= 0 {
		return
	}
	inst().streamBytesSent.Add(ctx, n, metric.WithAttributes(
		streamTransportDirectionAttrs(system, method, transport, direction)...))
}

// StreamTransportBytesReceived records the payload bytes returned by a
// successful transport read.
func StreamTransportBytesReceived(ctx context.Context, system, method, transport, direction string, n int64) {
	if n <= 0 {
		return
	}
	inst().streamBytesRecv.Add(ctx, n, metric.WithAttributes(
		streamTransportDirectionAttrs(system, method, transport, direction)...))
}

// StreamProtocolError records a rejected inbound application frame. Error codes
// are restricted to the framework whitelist; unknown values collapse to other.
func StreamProtocolError(ctx context.Context, system, method, transport, direction string, err error) {
	attrs := streamTransportDirectionAttrs(system, method, transport, direction)
	attrs = append(attrs, attribute.String(AttrErrorCode, streamErrorCode(err)))
	inst().streamProtocolErr.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// StreamDrainForcedClose records the first forced close of one managed stream
// connection. Callers are responsible for deduplicating per connection.
func StreamDrainForcedClose(ctx context.Context, system, method, transport string) {
	inst().streamForcedClose.Add(ctx, 1,
		metric.WithAttributes(streamTransportAttrs(system, method, transport)...))
}

func streamAttrs(system, method string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String(AttrSystem, system),
		attribute.String(AttrMethod, method),
		attribute.String(AttrTransport, TransportLocal),
	}
}

func streamDirectionAttrs(system, method, direction string) []attribute.KeyValue {
	if direction != DirectionClientToServer && direction != DirectionServerToClient {
		direction = OutcomeOther
	}
	attrs := streamAttrs(system, method)
	return append(attrs, attribute.String(AttrDirection, direction))
}

func streamTransportAttrs(system, method, transport string) []attribute.KeyValue {
	if transport != TransportSSE && transport != TransportWebSocket {
		transport = OutcomeOther
	}
	return []attribute.KeyValue{
		attribute.String(AttrSystem, system),
		attribute.String(AttrMethod, method),
		attribute.String(AttrTransport, transport),
	}
}

func streamTransportDirectionAttrs(system, method, transport, direction string) []attribute.KeyValue {
	if direction != DirectionClientToServer && direction != DirectionServerToClient {
		direction = OutcomeOther
	}
	attrs := streamTransportAttrs(system, method, transport)
	return append(attrs, attribute.String(AttrDirection, direction))
}

func streamFrameAttrs(system, method, transport, direction, frameType string) []attribute.KeyValue {
	attrs := streamTransportDirectionAttrs(system, method, transport, direction)
	attrs = append(attrs, attribute.String(AttrFrameType, streamFrameType(transport, frameType)))
	return attrs
}

func streamFrameType(transport, frameType string) string {
	switch transport {
	case TransportSSE:
		switch frameType {
		case FrameEvent, FrameHeartbeat, FrameError:
			return frameType
		}
	case TransportWebSocket:
		switch frameType {
		case FrameData, FrameHalfClose, FrameEnd, FrameError:
			return frameType
		}
	}
	return OutcomeOther
}

func streamOutcome(err error) string {
	switch {
	case err == nil, errors.Is(err, io.EOF):
		return OutcomeOK
	case errors.Is(err, context.Canceled):
		return OutcomeCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return OutcomeTimeout
	default:
		return OutcomeError
	}
}

func streamErrorCode(err error) string {
	var coded interface{ Code() string }
	if errors.As(err, &coded) {
		switch coded.Code() {
		case "INTERNAL", "INVALID_ARGUMENT", "NOT_FOUND", "CONFLICT", "UNAUTHENTICATED",
			"PERMISSION_DENIED", "UNAVAILABLE", "TX_BOUNDARY", "IDEMPOTENCY_CONFLICT",
			"IDEMPOTENCY_RESULT_UNAVAILABLE", "MIGRATION_DRIFT", "STEP_UP_REQUIRED":
			return coded.Code()
		}
	}
	return OutcomeOther
}

func outcomeOfCode(code string) string {
	if code == "" {
		return OutcomeOK
	}
	return OutcomeError
}

// OutboxDelivery 记录一条事件的投递结果。事件停摆是最难发现的故障
// （探针依然绿着），这条曲线归零就是告警条件。
func OutboxDelivery(ctx context.Context, topic string, err error, start time.Time) {
	inst().outbox.Record(ctx, since(start), metric.WithAttributes(
		attribute.String(AttrTopic, topic),
		attribute.String(AttrOutcome, Outcome(err)),
	))
}

// OutboxDead 记录一条转入死信的事件。这条曲线非零就该有人看。
func OutboxDead(ctx context.Context, topic string) {
	inst().dead.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrTopic, topic)))
}

// JobRun 记录周期任务的一轮。outcome 取 OutcomeOK/Error/Skipped
// （skipped = 锁被其它副本持有，属正常）。
func JobRun(ctx context.Context, name, outcome string, start time.Time) {
	inst().job.Record(ctx, since(start), metric.WithAttributes(
		attribute.String(AttrJob, name),
		attribute.String(AttrOutcome, outcome),
	))
}

// DBQueryOp 记录一次数据库查询。op 必须来自 Operation——完整 SQL 不进标签，
// 那等于把指标基数交给业务代码写的每一条语句。
func DBQueryOp(ctx context.Context, op string, err error, start time.Time) {
	inst().db.Record(ctx, since(start), metric.WithAttributes(
		attribute.String(AttrOperation, op),
		attribute.String(AttrOutcome, Outcome(err)),
	))
}

func since(start time.Time) float64 { return time.Since(start).Seconds() }

// knownOps 是允许出现在标签里的 SQL 动词白名单。白名单而非"取第一个词"：
// 后者会把 CTE 名、注释、拼错的语句原样送进标签，基数无界。
var knownOps = map[string]string{
	"SELECT": "SELECT", "INSERT": "INSERT", "UPDATE": "UPDATE", "DELETE": "DELETE",
	"WITH": "WITH", "BEGIN": "BEGIN", "COMMIT": "COMMIT", "ROLLBACK": "ROLLBACK",
	"CREATE": "CREATE", "ALTER": "ALTER", "DROP": "DROP", "TRUNCATE": "TRUNCATE",
	"COPY": "COPY", "CALL": "CALL", "SET": "SET", "SHOW": "SHOW", "EXPLAIN": "EXPLAIN",
}

// Operation 从 SQL 提取动词，未识别的一律归为 "other"。
func Operation(sql string) string {
	s := strings.TrimLeft(sql, " \t\r\n(")
	// 跳过前导的 -- 行注释（sqlc 生成的语句常带 "-- name: Xxx :one"）。
	for strings.HasPrefix(s, "--") {
		_, rest, found := strings.Cut(s, "\n")
		if !found {
			return "other"
		}
		s = strings.TrimLeft(rest, " \t\r\n(")
	}
	word, _, _ := strings.Cut(s, " ")
	if i := strings.IndexAny(word, "\t\r\n(;"); i >= 0 {
		word = word[:i]
	}
	if op, ok := knownOps[strings.ToUpper(word)]; ok {
		return op
	}
	return "other"
}
