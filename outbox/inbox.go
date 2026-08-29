package outbox

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/tx"
)

// Inbox 把 next 包装为幂等消费者：以 INSERT … ON CONFLICT DO NOTHING 在
// {schema}.inbox 占位，占位冲突说明该事件已处理过，直接返回 nil；占位成功才调
// next。去重键是 (consumer, event_id)：同一事件可被多个消费者各自处理一次，
// 只在同一 consumer 内去重——同 topic 的第二个消费者不会被静默跳过。
// inbox 记录与 next 的处理同事务——next 收到的 ctx 携带该事务句柄，
// 其内经 pgtx.From 的写（含嵌套 tx.Do 的 savepoint）与去重记录原子提交，
// next 失败则一并回滚，重投时得以完整重试。
// schema 不合法或 consumer 为空时 panic（构造期编程错误，fail-fast）。
func Inbox(pool *pgxpool.Pool, schema, consumer string, next appkit.EventHandler) appkit.EventHandler {
	mustSchema(schema)
	if consumer == "" {
		panic("outbox: Inbox 的 consumer 不能为空（去重按 (consumer, event_id)）")
	}
	insertSQL := fmt.Sprintf(
		`INSERT INTO %s.inbox (consumer, event_id, topic) VALUES ($1, $2, $3) ON CONFLICT (consumer, event_id) DO NOTHING`,
		ident(schema))

	return func(ctx context.Context, evt appkit.Event) error {
		if evt.ID == "" {
			// 无 ID 无从去重；重试也不会好，直接判为坏事件。
			return apperr.InvalidArgument("outbox: 事件缺少 ID，无法去重（topic %q）", evt.Topic)
		}
		ptx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("outbox: 开启 inbox 事务: %w", err)
		}
		// 提交成功后的 Rollback 是空操作；next panic 时同样兜底回滚。
		defer func() { _ = ptx.Rollback(context.WithoutCancel(ctx)) }()

		tag, err := ptx.Exec(ctx, insertSQL, consumer, evt.ID, evt.Topic)
		if err != nil {
			return fmt.Errorf("outbox: 写入 inbox（事件 %s）: %w", evt.ID, err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		if err := next(tx.With(ctx, ptx), evt); err != nil {
			return err
		}
		if err := ptx.Commit(ctx); err != nil {
			return fmt.Errorf("outbox: 提交 inbox 事务（事件 %s）: %w", evt.ID, err)
		}
		return nil
	}
}
