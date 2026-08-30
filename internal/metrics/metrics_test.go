package metrics

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
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

// TestRecordedAttributes 钉住四条路径的标签集。标签集就是基数契约：
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
}

// TestDurationBucketsFitSeconds 验证桶边界是给秒设计的：SDK 默认那套
// （0..10000）用在秒上会让几乎所有观测挤进第一个桶，p99 直接失真。
func TestDurationBucketsFitSeconds(t *testing.T) {
	// 一次 50ms 的调用应落进 0.05~0.075 那一档，而不是"小于第一个边界"。
	const method = "BucketProbe"
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
