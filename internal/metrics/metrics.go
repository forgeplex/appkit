// Package metrics 集中定义 appkit 自动埋点的指标。
//
// 指标名与标签集在这里钉死，业务代码不参与——这是刻意的：指标的成本不在
// 采集而在基数，一个把 URL 路径、错误消息或用户 id 当标签的埋点足以打爆
// 后端存储，而这类事故通常由散落各处的"顺手加一个标签"造成。所有标签值
// 要么是代码里的常量（系统名、方法名、topic、任务名），要么是本包收敛过的
// 枚举（outcome、SQL 动词），没有第三种。
//
// 埋点覆盖 RED 三件套（Rate / Errors / Duration，直方图一并给出）的四条路径：
// 契约调用、outbox 投递、周期任务、数据库查询。HTTP 入站不在此列——
// otelhttp 已经产出 http.server.request.duration（含 http.route），
// 再埋一遍就是双重计数。
//
// 未配置 OTLP 端点时全局 MeterProvider 是 noop，各 Record 调用近乎零成本，
// 因此埋点无开关。
package metrics

import (
	"cmp"
	"context"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// scope 是本框架全部指标的 instrumentation scope。
const scope = "github.com/forgeplex/appkit"

// 标签键。与 OTel 语义约定不冲突的一律加 appkit. 前缀，避免与
// 用户或其它库的同名标签混淆。
const (
	AttrSystem    = "appkit.contract.system"
	AttrMethod    = "appkit.contract.method"
	AttrTopic     = "appkit.outbox.topic"
	AttrSchema    = "appkit.outbox.schema"
	AttrJob       = "appkit.job.name"
	AttrOperation = "db.operation"
	AttrOutcome   = "appkit.outcome"
	AttrErrorCode = "appkit.error.code"
)

// outcome 的取值全集。四条路径共用同一套，仪表盘只需写一遍。
const (
	OutcomeOK      = "ok"
	OutcomeError   = "error"
	OutcomeSkipped = "skipped"
)

// durationBuckets 是秒为单位的桶边界（OTel 对 duration 类指标的推荐值）。
// 不用 SDK 默认桶：那套边界是给毫秒设计的，用在秒上几乎所有观测都落进第一个桶。
var durationBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10,
}

type instruments struct {
	contract metric.Float64Histogram
	outbox   metric.Float64Histogram
	dead     metric.Int64Counter
	job      metric.Float64Histogram
	db       metric.Float64Histogram
}

// inst 懒初始化：全局 MeterProvider 由 telemetry.Init 装配，而 Init 在
// 包级变量初始化之后才跑。首次埋点必然发生在服务就绪之后，此时 provider 已就位。
var inst = sync.OnceValue(func() *instruments {
	m := otel.Meter(scope)
	return &instruments{
		contract: histogram(m, "appkit.contract.call.duration",
			"跨模块契约调用耗时（进程内与远程同口径）"),
		outbox: histogram(m, "appkit.outbox.delivery.duration",
			"outbox relay 单条事件的投递耗时"),
		dead: counter(m, "appkit.outbox.dead", "投递重试达上限、转入死信的事件数"),
		job:  histogram(m, "appkit.job.run.duration", "周期任务单轮执行耗时"),
		db:   histogram(m, "appkit.db.query.duration", "数据库查询耗时"),
	}
})

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
