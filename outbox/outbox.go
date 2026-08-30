// Package outbox 实现事务性事件外发（transactional outbox）与幂等消费（inbox）。
//
// 写路径：业务在 tx.Do 事务内经 Publisher.Publish（或低层 Publish）落表，
// 业务写与事件落表同事务提交——「忘发事件」与「业务回滚了事件却发出去」
// 都不可能发生（DESIGN §7）。业务包只需依赖自定义的单方法发布接口，
// 装配时注入 *Publisher 即可，无须 import 任何 infra 包。
//
// 读路径（Relay）按 claim/lease 两段式投递：
//   - claim：短事务内以 FOR UPDATE SKIP LOCKED 选中到期未发布事件并写
//     claimed_until 租约，立即提交——投递期间不占用连接池连接；
//   - 投递：逐条调 Bus.Publish（进程内 DirectBus，或未来的 NATS/Kafka 适配），
//     handler panic 会被恢复并按失败处理；
//   - 收尾：成功者标记 published_at；失败者记 attempts 与指数退避
//     next_attempt_at（封顶 5 分钟），超过重试上限置 failed_at 进入死信，
//     移出投递热路径等待人工介入。
//
// 投递语义：至少一次；批内按 created_at 尽力保序——单条失败即停止本批后续
// 投递，但失败者随退避让位，不会永久阻塞后续事件。进程在投递与收尾之间
// 崩溃时，租约到期后由任一副本接管重投。消费方用 Inbox 按 (consumer,
// event_id) 去重，整体达成「至少一次投递、至多一次生效」。
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/callctx"
	"github.com/forgeplex/appkit/pgtx"
	"github.com/forgeplex/appkit/tx"
)

// schemaRe 是全框架统一的 schema 名约束。schema 会被拼进 SQL 语句，
// 此校验同时杜绝注入。
var schemaRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// mustSchema 校验失败即 panic：schema 名在构造期写死，属编程错误；
// Register/Setup 阶段的 panic 由 appkit 捕获为启动错误（fail-fast）。
func mustSchema(schema string) {
	if !schemaRe.MatchString(schema) {
		panic(fmt.Sprintf("outbox: schema 名 %q 不合法（须匹配 %s）", schema, schemaRe))
	}
}

// ident 给已通过校验的 schema 加双引号，避免与 SQL 保留字冲突。
func ident(schema string) string { return `"` + schema + `"` }

const migrationTemplate = `CREATE TABLE IF NOT EXISTS %[1]s.outbox (
    id              uuid        PRIMARY KEY,
    topic           text        NOT NULL,
    payload         jsonb       NOT NULL,
    meta            jsonb,
    attempts        int         NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    claimed_until   timestamptz,
    failed_at       timestamptz,
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    published_at    timestamptz
);

CREATE INDEX IF NOT EXISTS outbox_unpublished_idx
    ON %[1]s.outbox (created_at) WHERE published_at IS NULL AND failed_at IS NULL;

CREATE TABLE IF NOT EXISTS %[1]s.inbox (
    consumer     text        NOT NULL,
    event_id     uuid        NOT NULL,
    topic        text        NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);
`

// MigrationSQL 返回 outbox/inbox 两张表的 DDL，域 repo 把它写进自己 schema
// 的迁移（DESIGN §8：outbox/inbox 每 schema 一套）。语句幂等，可重复执行。
// 待投递行走部分索引（排除已发布与死信），扫描代价只随积压量增长而不随
// 历史总量增长。inbox 主键为 (consumer, event_id)：同一事件可被多个消费者
// 各自消费一次。schema 不合法时 panic。
func MigrationSQL(schema string) string {
	mustSchema(schema)
	return fmt.Sprintf(migrationTemplate, ident(schema))
}

// errNoTx：Publish 的运行时守卫。与 contract 的「事务内禁跨模块调用」互为镜像：
// 事件落表必须在业务事务内，同码便于两类边界违规统一告警。
var errNoTx = apperr.New(apperr.CodeTxBoundary, http.StatusInternalServerError,
	"outbox.Publish 必须在事务内调用（tx.Do）：业务写与事件落表必须同事务提交")

// Publish 是发布的低层入口，供 Publisher 与自带 pgtx.DB 的装配代码复用；
// 业务包应经 Publisher（依赖自定义单方法接口）发事件，避免 import infra。
// 在当前事务内把事件写入 {schema}.outbox。db 传 pgtx.From(ctx, pool)
// 的结果（Do 之内它就是当前事务）。evt.ID 为空时生成新 uuid，非空时必须是 uuid。
// Payload 须为 JSON（jsonb 列），为空时落 JSON null。
func Publish(ctx context.Context, db pgtx.DB, schema string, evt appkit.Event) error {
	if !tx.HasTx(ctx) {
		return errNoTx.WithDetail("topic", evt.Topic)
	}
	if !schemaRe.MatchString(schema) {
		return apperr.InvalidArgument("outbox: schema 名 %q 不合法", schema)
	}
	if evt.Topic == "" {
		return apperr.InvalidArgument("outbox: 事件缺少 topic")
	}
	id := evt.ID
	if id == "" {
		id = uuid.NewString()
	} else if _, err := uuid.Parse(id); err != nil {
		return apperr.InvalidArgument("outbox: 事件 ID %q 不是合法 uuid", id).WithCause(err)
	}

	payload := evt.Payload
	if len(payload) == 0 {
		payload = []byte("null")
	}
	// 把 callctx 白名单快照进事件 meta：事件是异步的，投递时原请求的 ctx
	// 早没了，不在这里存下来，链路就断在 outbox 表这一行上。
	// 写副本而不是改 evt.Meta——调用方传进来的 map 不该被我们改。
	metaMap := callctx.ToMap(callctx.From(ctx), maps.Clone(evt.Meta))
	var meta []byte
	if len(metaMap) > 0 {
		m, err := json.Marshal(metaMap)
		if err != nil {
			return fmt.Errorf("outbox: 序列化事件 meta: %w", err)
		}
		meta = m
	}

	insertSQL := fmt.Sprintf(
		`INSERT INTO %s.outbox (id, topic, payload, meta) VALUES ($1, $2, $3, $4)`,
		ident(schema))
	if _, err := db.Exec(ctx, insertSQL, id, evt.Topic, payload, meta); err != nil {
		return fmt.Errorf("outbox: 写入 outbox（topic %q）: %w", evt.Topic, err)
	}
	return nil
}

// Publisher 是面向业务层的事件发布器。业务包自定义单方法接口（方法签名
// Publish(ctx context.Context, evt appkit.Event) error）并声明依赖，装配时
// 注入 *Publisher——业务代码由此做到零 infra import（不见 pgtx/pgx）。
type Publisher struct {
	pool   *pgxpool.Pool
	schema string
}

// NewPublisher 构造 Publisher。schema 不合法时 panic。
func NewPublisher(pool *pgxpool.Pool, schema string) *Publisher {
	mustSchema(schema)
	return &Publisher{pool: pool, schema: schema}
}

// Publish 在当前事务内把事件写入 {schema}.outbox。必须在 tx.Do 之内调用，
// 否则返回 TX_BOUNDARY 错误——语义与包级 Publish 完全一致。
func (p *Publisher) Publish(ctx context.Context, evt appkit.Event) error {
	return Publish(ctx, pgtx.From(ctx, p.pool), p.schema, evt)
}
