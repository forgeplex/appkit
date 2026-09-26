package metrics

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/forgeplex/appkit/internal/hoststate"
)

// TestOperationBoundsCardinality 是本包最重要的单测：SQL 动词是唯一由
// 「业务写的字符串」推导出的标签值，只要它能逃逸出白名单，指标基数就无界。
func TestOperationBoundsCardinality(t *testing.T) {
	t.Parallel()
	tests := []struct{ sql, want string }{
		{"SELECT 1", "SELECT"},
		{"select id from t", "SELECT"},
		{"  \n\tINSERT INTO t VALUES ($1)", "INSERT"},
		{"UPDATE t SET a = 1", "UPDATE"},
		{"DELETE FROM t", "DELETE"},
		{"WITH x AS (SELECT 1) SELECT * FROM x", "WITH"},
		{"begin", "BEGIN"},
		{"COMMIT;", "COMMIT"},
		{"(SELECT 1)", "SELECT"},
		{"SELECT\n1", "SELECT"},
		// sqlc 生成的语句带前导注释，动词在第二行。
		{"-- name: GetUser :one\nSELECT * FROM users WHERE id = $1", "SELECT"},
		{"-- a\n-- b\nUPDATE t SET x = 1", "UPDATE"},
		// 逃逸尝试：任何不在白名单里的东西都必须塌缩成同一个值。
		{"", "other"},
		{"-- 只有注释没有语句", "other"},
		{"DROP TABLE IF EXISTS x", "DROP"},
		{"用户输入的鬼东西", "other"},
		{"SELECTX 1", "other"},
		{"/* 块注释 */ SELECT 1", "other"}, // 保守收敛，不做块注释解析
	}
	for _, tc := range tests {
		if got := Operation(tc.sql); got != tc.want {
			t.Errorf("Operation(%q) = %q, want %q", tc.sql, got, tc.want)
		}
	}
}

// 全局 MeterProvider 只能设一次（otel 的 global 装载语义），因此整个测试
// 二进制共用一个 ManualReader；读数是累积的，各用例用自己的标签值区分。
var testReader = sdkmetric.NewManualReader()
var streamMetricTestSequence atomic.Uint64

func nextStreamMetricMethod(prefix string) string {
	return prefix + "_" + strconv.FormatUint(streamMetricTestSequence.Add(1), 10)
}

func TestMain(m *testing.M) {
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(testReader)))
	os.Exit(m.Run())
}

// collect 采集本框架 scope 下的全部指标。
func collect(t *testing.T) []metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("采集: %v", err)
	}
	var out []metricdata.Metrics
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name == scope {
			out = append(out, sm.Metrics...)
		}
	}
	return out
}

func find(t *testing.T, ms []metricdata.Metrics, name string) metricdata.Metrics {
	t.Helper()
	for _, m := range ms {
		if m.Name == name {
			return m
		}
	}
	var names []string
	for _, m := range ms {
		names = append(names, m.Name)
	}
	t.Fatalf("未找到指标 %q，实际有 %v", name, names)
	return metricdata.Metrics{}
}

// attrsOf 汇总一个指标全部数据点的标签集合（每个点一份 key=value 列表）。
func attrsOf(t *testing.T, m metricdata.Metrics) []map[string]string {
	t.Helper()
	var out []map[string]string
	add := func(set attribute.Set) {
		kv := map[string]string{}
		for _, a := range set.ToSlice() {
			kv[string(a.Key)] = a.Value.String()
		}
		out = append(out, kv)
	}
	switch d := m.Data.(type) {
	case metricdata.Histogram[float64]:
		for _, dp := range d.DataPoints {
			add(dp.Attributes)
		}
	case metricdata.Sum[int64]:
		for _, dp := range d.DataPoints {
			add(dp.Attributes)
		}
	case metricdata.Gauge[int64]:
		for _, dp := range d.DataPoints {
			add(dp.Attributes)
		}
	case metricdata.Gauge[float64]:
		for _, dp := range d.DataPoints {
			add(dp.Attributes)
		}
	default:
		t.Fatalf("指标 %q 的数据类型未覆盖: %T", m.Name, m.Data)
	}
	return out
}

func hasAttrs(got []map[string]string, want map[string]string) bool {
	for _, kv := range got {
		match := len(kv) == len(want)
		for k, v := range want {
			if kv[k] != v {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// TestRecordedAttributes 钉住非 Stream 指标路径的标签集。标签集就是基数契约：
// 多一个自由维度就是一次事故，这里的断言用的是全等而非包含。
func TestRecordedAttributes(t *testing.T) {
	ctx := context.Background()
	start := time.Now().Add(-20 * time.Millisecond)
	boom := errors.New("boom")

	ContractCall(ctx, "ledger", "PostEntries", "", start)
	ContractCall(ctx, "ledger", "PostEntries", "NOT_FOUND", start)
	OutboxDelivery(ctx, "ledger.entry.posted", nil, start)
	OutboxDelivery(ctx, "ledger.entry.posted", boom, start)
	OutboxDead(ctx, "ledger.entry.posted")
	JobRun(ctx, "ledger.cleanup", OutcomeSkipped, start)
	DBQueryOp(ctx, Operation("SELECT 1"), nil, start)
	HTTPClientCall(ctx, "private-method", "unexpected-status", context.DeadlineExceeded, start)

	ms := collect(t)

	contract := find(t, ms, "appkit.contract.call.duration")
	if contract.Unit != "s" {
		t.Errorf("契约指标单位应为秒，得到 %q", contract.Unit)
	}
	cattrs := attrsOf(t, contract)
	// 成功不带错误码，失败才带——错误码是有限常量集，可以安全当标签。
	if !hasAttrs(cattrs, map[string]string{
		AttrSystem: "ledger", AttrMethod: "PostEntries", AttrOutcome: OutcomeOK,
	}) {
		t.Errorf("成功调用的标签集不符: %v", cattrs)
	}
	if !hasAttrs(cattrs, map[string]string{
		AttrSystem: "ledger", AttrMethod: "PostEntries",
		AttrOutcome: OutcomeError, AttrErrorCode: "NOT_FOUND",
	}) {
		t.Errorf("失败调用的标签集不符: %v", cattrs)
	}

	ob := attrsOf(t, find(t, ms, "appkit.outbox.delivery.duration"))
	for _, oc := range []string{OutcomeOK, OutcomeError} {
		if !hasAttrs(ob, map[string]string{AttrTopic: "ledger.entry.posted", AttrOutcome: oc}) {
			t.Errorf("outbox 投递指标缺 outcome=%s: %v", oc, ob)
		}
	}

	if !hasAttrs(attrsOf(t, find(t, ms, "appkit.outbox.dead")),
		map[string]string{AttrTopic: "ledger.entry.posted"}) {
		t.Error("死信计数器标签集不符")
	}
	if !hasAttrs(attrsOf(t, find(t, ms, "appkit.job.run.duration")),
		map[string]string{AttrJob: "ledger.cleanup", AttrOutcome: OutcomeSkipped}) {
		t.Error("任务指标标签集不符")
	}
	if !hasAttrs(attrsOf(t, find(t, ms, "appkit.db.query.duration")),
		map[string]string{AttrOperation: "SELECT", AttrOutcome: OutcomeOK}) {
		t.Error("数据库指标标签集不符")
	}
	httpAttrs := map[string]string{
		"http.request.method":        "other",
		"http.response.status_class": "other",
		AttrOutcome:                  OutcomeTimeout,
	}
	if !hasAttrs(attrsOf(t, find(t, ms, "appkit.http.client.request")), httpAttrs) {
		t.Error("HTTP client counter 标签集不符")
	}
	if !hasAttrs(attrsOf(t, find(t, ms, "appkit.http.client.request.duration")), httpAttrs) {
		t.Error("HTTP client duration 标签集不符")
	}
}

func TestStreamMetricsHaveBoundedAttributes(t *testing.T) {
	ctx := context.Background()
	const system = "metrics-stream-probe"
	method := nextStreamMetricMethod("Lifecycle")
	start := time.Now().Add(-25 * time.Millisecond)

	StreamOpened(ctx, system, method)
	StreamMessageSent(ctx, system, method, DirectionClientToServer)
	StreamMessageSent(ctx, system, method, "untrusted-dynamic-direction")
	StreamMessageReceived(ctx, system, method, DirectionServerToClient)
	StreamFirstMessage(ctx, system, method, DirectionServerToClient, 15*time.Millisecond)
	StreamBackpressureWait(ctx, system, method, DirectionClientToServer, 5*time.Millisecond)
	StreamClosed(ctx, system, method, errors.New("private payload-like error"), time.Since(start), false)

	ms := collect(t)
	base := map[string]string{
		AttrSystem: system, AttrMethod: method, AttrTransport: TransportLocal,
	}
	if got := sumValueFor(t, find(t, ms, "appkit.contract.stream.active"), base); got != 0 {
		t.Errorf("active after close = %d, want 0", got)
	}
	if got := sumValueFor(t, find(t, ms, "appkit.contract.stream.opened"), base); got != 1 {
		t.Errorf("opened = %d, want 1", got)
	}
	closedAttrs := map[string]string{
		AttrSystem: system, AttrMethod: method, AttrTransport: TransportLocal,
		AttrOutcome: OutcomeError, AttrErrorCode: OutcomeOther,
	}
	if got := sumValueFor(t, find(t, ms, "appkit.contract.stream.closed"), closedAttrs); got != 1 {
		t.Errorf("closed(error) = %d, want 1", got)
	}
	if !hasAttrs(attrsOf(t, find(t, ms, "appkit.contract.stream.duration")), closedAttrs) {
		t.Error("duration must use the same bounded terminal attributes")
	}
	if !hasAttrs(attrsOf(t, find(t, ms, "appkit.contract.stream.message.sent")), map[string]string{
		AttrSystem: system, AttrMethod: method, AttrTransport: TransportLocal,
		AttrDirection: DirectionClientToServer,
	}) || !hasAttrs(attrsOf(t, find(t, ms, "appkit.contract.stream.message.sent")), map[string]string{
		AttrSystem: system, AttrMethod: method, AttrTransport: TransportLocal,
		AttrDirection: OutcomeOther,
	}) {
		t.Error("message direction labels must be finite and unknown values must collapse")
	}
	if !hasAttrs(attrsOf(t, find(t, ms, "appkit.contract.stream.message.received")), map[string]string{
		AttrSystem: system, AttrMethod: method, AttrTransport: TransportLocal,
		AttrDirection: DirectionServerToClient,
	}) {
		t.Error("received-message attributes do not match the low-cardinality contract")
	}
	for _, name := range []string{
		"appkit.contract.stream.time_to_first_message",
		"appkit.contract.stream.send.backpressure.wait",
	} {
		if len(attrsOf(t, find(t, ms, name))) == 0 {
			t.Errorf("%s has no data points", name)
		}
	}
}

func TestStreamTransportMetricsNormalizeAttributes(t *testing.T) {
	ctx := context.Background()
	const system = "metrics-stream-transport-probe"
	method := nextStreamMetricMethod("Transport")

	StreamTransportFrameSent(ctx, system, method, TransportSSE, DirectionServerToClient, FrameHeartbeat)
	StreamTransportFrameReceived(ctx, system, method, "session-123", "user-direction", "run-specific-frame")
	StreamTransportBytesSent(ctx, system, method, "dynamic-transport", DirectionServerToClient, 17)
	StreamTransportBytesReceived(ctx, system, method, TransportWebSocket, "user-direction", 23)
	StreamProtocolError(ctx, system, method, TransportWebSocket, DirectionClientToServer, errors.New("private payload detail"))
	StreamDrainForcedClose(ctx, system, method, TransportWebSocket)

	ms := collect(t)
	if got := sumValueFor(t, find(t, ms, "appkit.contract.stream.transport.frame.sent"), map[string]string{
		AttrSystem: system, AttrMethod: method, AttrTransport: TransportSSE,
		AttrDirection: DirectionServerToClient, AttrFrameType: FrameHeartbeat,
	}); got != 1 {
		t.Errorf("SSE heartbeat frames sent = %d, want 1", got)
	}
	if got := sumValueFor(t, find(t, ms, "appkit.contract.stream.transport.frame.received"), map[string]string{
		AttrSystem: system, AttrMethod: method, AttrTransport: OutcomeOther,
		AttrDirection: OutcomeOther, AttrFrameType: OutcomeOther,
	}); got != 1 {
		t.Errorf("untrusted received frame attributes did not collapse: got %d", got)
	}
	if got := sumValueFor(t, find(t, ms, "appkit.contract.stream.transport.bytes.sent"), map[string]string{
		AttrSystem: system, AttrMethod: method, AttrTransport: OutcomeOther,
		AttrDirection: DirectionServerToClient,
	}); got != 17 {
		t.Errorf("normalized bytes sent = %d, want 17", got)
	}
	if got := sumValueFor(t, find(t, ms, "appkit.contract.stream.transport.bytes.received"), map[string]string{
		AttrSystem: system, AttrMethod: method, AttrTransport: TransportWebSocket,
		AttrDirection: OutcomeOther,
	}); got != 23 {
		t.Errorf("normalized bytes received = %d, want 23", got)
	}
	if got := sumValueFor(t, find(t, ms, "appkit.contract.stream.protocol.error"), map[string]string{
		AttrSystem: system, AttrMethod: method, AttrTransport: TransportWebSocket,
		AttrDirection: DirectionClientToServer, AttrErrorCode: OutcomeOther,
	}); got != 1 {
		t.Errorf("unknown protocol error code = %d, want 1", got)
	}
	if got := sumValueFor(t, find(t, ms, "appkit.contract.stream.drain.forced_close"), map[string]string{
		AttrSystem: system, AttrMethod: method, AttrTransport: TransportWebSocket,
	}); got != 1 {
		t.Errorf("forced closes = %d, want 1", got)
	}
}

func TestManagedServiceStateGaugeAggregatesCurrentInstances(t *testing.T) {
	module := "metrics-host-" + nextStreamMetricMethod("state")
	const serviceName = "relay"
	first := hoststate.NewService(module, serviceName)
	second := hoststate.NewService(module, serviceName)
	Initialize()

	createdAttrs := map[string]string{
		AttrHostServiceModule: module,
		AttrHostServiceName:   serviceName,
		AttrHostServiceState:  string(hoststate.StateCreated),
	}
	created := find(t, collect(t), "appkit.host.managed_service.state")
	if created.Unit != "{service}" {
		t.Fatalf("ManagedService gauge unit = %q, want {service}", created.Unit)
	}
	if got := gaugeValueFor(t, created, createdAttrs); got != 2 {
		t.Fatalf("created gauge = %d, want 2 aggregated instances", got)
	}

	first.Transition(hoststate.StateReady)
	second.Transition(hoststate.StateFailed)
	ms := collect(t)
	gauge := find(t, ms, "appkit.host.managed_service.state")
	if got := gaugeValueFor(t, gauge, map[string]string{
		AttrHostServiceModule: module,
		AttrHostServiceName:   serviceName,
		AttrHostServiceState:  string(hoststate.StateReady),
	}); got != 1 {
		t.Errorf("ready gauge = %d, want 1", got)
	}
	if got := gaugeValueFor(t, gauge, map[string]string{
		AttrHostServiceModule: module,
		AttrHostServiceName:   serviceName,
		AttrHostServiceState:  string(hoststate.StateFailed),
	}); got != 1 {
		t.Errorf("failed gauge = %d, want 1", got)
	}
	for _, attrs := range attrsOf(t, gauge) {
		if attrs[AttrHostServiceModule] != module {
			continue
		}
		if len(attrs) != 3 {
			t.Errorf("ManagedService gauge attributes must be exactly module/service/state: %v", attrs)
		}
	}
	for _, state := range []hoststate.State{
		hoststate.StateCreated, hoststate.StateStarting, hoststate.StateRunning,
		hoststate.StateReady, hoststate.StateFailed, hoststate.StateDraining,
		hoststate.StateStopping, hoststate.StateStopped, hoststate.StateNotStarted,
	} {
		if got := managedServiceState(state); got != string(state) {
			t.Errorf("lifecycle state label %q normalized to %q", state, got)
		}
	}
	if got := managedServiceState(hoststate.State("unbounded-value")); got != OutcomeOther {
		t.Errorf("unknown lifecycle state = %q, want bounded %q", got, OutcomeOther)
	}
}

func gaugeValueFor(t *testing.T, m metricdata.Metrics, want map[string]string) int64 {
	t.Helper()
	gauge, ok := m.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("指标 %q 不是 int64 gauge: %T", m.Name, m.Data)
	}
	for _, dp := range gauge.DataPoints {
		got := map[string]string{}
		for _, attr := range dp.Attributes.ToSlice() {
			got[string(attr.Key)] = attr.Value.AsString()
		}
		if len(got) != len(want) {
			continue
		}
		match := true
		for key, value := range want {
			if got[key] != value {
				match = false
				break
			}
		}
		if match {
			return dp.Value
		}
	}
	t.Fatalf("指标 %q 没有属性集 %v 的数据点", m.Name, want)
	return 0
}

func sumValueFor(t *testing.T, m metricdata.Metrics, want map[string]string) int64 {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("指标 %q 不是 int64 sum: %T", m.Name, m.Data)
	}
	for _, dp := range sum.DataPoints {
		got := map[string]string{}
		for _, attr := range dp.Attributes.ToSlice() {
			got[string(attr.Key)] = attr.Value.AsString()
		}
		if len(got) != len(want) {
			continue
		}
		match := true
		for key, value := range want {
			if got[key] != value {
				match = false
				break
			}
		}
		if match {
			return dp.Value
		}
	}
	t.Fatalf("指标 %q 没有属性集 %v 的数据点", m.Name, want)
	return 0
}

// TestDurationBucketsFitSeconds 验证桶边界是给秒设计的：SDK 默认那套
// （0..10000）用在秒上会让几乎所有观测挤进第一个桶，p99 直接失真。
func TestDurationBucketsFitSeconds(t *testing.T) {
	// 一次 50ms 的调用应落进 0.05~0.075 那一档，而不是"小于第一个边界"。
	method := nextStreamMetricMethod("BucketProbe")
	ContractCall(context.Background(), "ledger", method, "", time.Now().Add(-50*time.Millisecond))

	h, ok := find(t, collect(t), "appkit.contract.call.duration").Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatal("契约指标不是直方图")
	}
	// 读数是累积的，按方法名挑出本用例那一个数据点。
	var dp metricdata.HistogramDataPoint[float64]
	for _, p := range h.DataPoints {
		if v, ok := p.Attributes.Value(AttrMethod); ok && v.AsString() == method {
			dp = p
		}
	}
	if dp.Count != 1 {
		t.Fatalf("未找到本用例的数据点（count=%d）", dp.Count)
	}
	if len(dp.Bounds) == 0 || dp.Bounds[0] >= 1 {
		t.Fatalf("桶边界不像是秒级: %v", dp.Bounds)
	}
	var idx int
	for i, c := range dp.BucketCounts {
		if c > 0 {
			idx = i
		}
	}
	if idx == 0 {
		t.Errorf("50ms 落进了第一个桶（<%v），分辨率不足: %v", dp.Bounds[0], dp.BucketCounts)
	}
}

// TestObserveBacklogUnregisters 验证注销后回调不再执行——relay 停了还继续
// 查库，等于关不掉的后台查询。
func TestObserveBacklogUnregisters(t *testing.T) {
	calls := 0
	stop := ObserveOutboxBacklog("ledger", func(context.Context) (Backlog, error) {
		calls++
		return Backlog{Pending: 7, OldestAge: 90 * time.Second}, nil
	})

	ms := collect(t)
	if calls != 1 {
		t.Fatalf("采集应触发一次回调，实际 %d 次", calls)
	}
	if !hasAttrs(attrsOf(t, find(t, ms, "appkit.outbox.pending")),
		map[string]string{AttrSchema: "ledger"}) {
		t.Error("积压观测量标签集不符")
	}
	find(t, ms, "appkit.outbox.oldest_pending.age")

	stop()
	collect(t)
	if calls != 1 {
		t.Errorf("注销后回调仍被调用 %d 次", calls)
	}
}
