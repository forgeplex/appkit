// Package audit 提供写操作审计原语：审计记录与业务写在同一事务内落库，
// 事务回滚则审计一并消失——审计反映的是"已发生的事实"。
//
// PSP 约定：资金相关的每次状态变更（记账、退款、改配置）都应留一条审计。
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/pgtx"
	"github.com/forgeplex/appkit/tx"
)

var validSchema = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// MigrationSQL 返回审计表 DDL（幂等），域 repo 把它并入自己的基础迁移。
func MigrationSQL(schema string) string {
	if !validSchema.MatchString(schema) {
		panic(fmt.Sprintf("audit: 非法 schema 名 %q", schema))
	}
	q := `"` + schema + `"`
	return `
CREATE TABLE IF NOT EXISTS ` + q + `.audit_log (
    id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at        timestamptz NOT NULL DEFAULT now(),
    actor     text        NOT NULL,
    action    text        NOT NULL,
    entity    text        NOT NULL,
    entity_id text        NOT NULL,
    before    jsonb,
    after     jsonb,
    meta      jsonb
);
CREATE INDEX IF NOT EXISTS audit_log_entity_idx
    ON ` + q + `.audit_log (entity, entity_id, at);

COMMENT ON TABLE ` + q + `.audit_log IS '审计流水：与业务写同事务落库，记录 actor/action/before/after。';
`
}

// Entry 是一条审计记录。Before/After/Meta 会被 JSON 序列化（可为 nil）。
type Entry struct {
	Actor    string // 操作者（用户 ID、服务名、"system"）
	Action   string // 动作（如 "entry.post"、"refund.approve"）
	Entity   string // 实体类型（如 "ledger_entry"）
	EntityID string
	Before   any
	After    any
	Meta     any
}

// Recorder 绑定 pool 与 schema，在 wiring 期构造后注入业务层
// （业务层按"接口放消费方"依赖单方法接口，不 import 本包）。
type Recorder struct {
	pool   *pgxpool.Pool
	schema string
}

func NewRecorder(pool *pgxpool.Pool, schema string) *Recorder {
	if !validSchema.MatchString(schema) {
		panic(fmt.Sprintf("audit: 非法 schema 名 %q", schema))
	}
	return &Recorder{pool: pool, schema: schema}
}

// Record 在当前事务内写入一条审计。必须在事务内调用——审计脱离业务事务
// 就失去了"与事实同生共死"的意义。
func (r *Recorder) Record(ctx context.Context, e Entry) error {
	if !tx.HasTx(ctx) {
		return apperr.New(apperr.CodeTxBoundary, 500,
			"审计必须与业务写在同一事务内（在 tx.Do 中调用 Record）")
	}
	if e.Actor == "" || e.Action == "" || e.Entity == "" {
		return apperr.InvalidArgument("审计记录缺少必填字段（actor/action/entity）")
	}
	before, err := marshalNullable(e.Before)
	if err != nil {
		return fmt.Errorf("audit: 序列化 before: %w", err)
	}
	after, err := marshalNullable(e.After)
	if err != nil {
		return fmt.Errorf("audit: 序列化 after: %w", err)
	}
	meta, err := marshalNullable(e.Meta)
	if err != nil {
		return fmt.Errorf("audit: 序列化 meta: %w", err)
	}

	db := pgtx.From(ctx, r.pool)
	_, err = db.Exec(ctx,
		`INSERT INTO "`+r.schema+`".audit_log (actor, action, entity, entity_id, before, after, meta)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		e.Actor, e.Action, e.Entity, e.EntityID, before, after, meta)
	if err != nil {
		return fmt.Errorf("audit: 写入审计: %w", err)
	}
	return nil
}

// marshalNullable 把 nil 保持为 SQL NULL，其余 JSON 序列化。
func marshalNullable(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}
