package outbox

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/pgtx"
)

// Inbox 把 next 包装为幂等消费者：以 INSERT … ON CONFLICT DO NOTHING 在
// {schema}.inbox 占位，占位冲突说明该事件已处理过，直接返回 nil；占位成功才调
// next。去重键是 (consumer, event_id)：同一事件可被多个消费者各自处理一次，
// 只在同一 consumer 内去重——同 topic 的第二个消费者不会被静默跳过。
// 该兼容入口使用普通 pgtx.Transactor 开启事务。若业务使用租户、分区域或双层
// 隔离，应改用 InboxWithTransactor，并传入同一套 pgtx Transactor；这样 Inbox
// 占位、Service 的嵌套 Do/savepoint 和 repository 写入都在同一个已验证作用域内。
// schema 不合法或 consumer 为空时 panic（构造期编程错误，fail-fast）。
func Inbox(pool *pgxpool.Pool, schema, consumer string, next appkit.EventHandler) appkit.EventHandler {
	return InboxWithTransactor(pool, pgtx.New(pool), schema, consumer, next)
}

// InboxWithTransactor 把 next 包装为幂等消费者，并使用 transactor 划定事务边界。
// transactor 必须与 pool 连接到同一个数据库；pool 仅用于在事务上下文内取得
// pgtx DBTX，实际查询会落到 transactor 建立的当前 pgx.Tx。
//
// 事务由 pgtx.Do 建立而不是手工把 raw pgx.Tx 塞入 ctx：因此仍然保留 pgtx
// v0.9.3 的私有 marker、事务句柄身份和 tenant/route/read-all 作用域校验。
// next 收到的 ctx 携带该事务，业务层再次调用任意同作用域 Transactor.Do 会
// 开 savepoint；next 失败或 panic 时 Inbox 占位与业务写一并回滚。
// plain/tenant Transactor 要求 schema 非空并使用限定表名；routed/routed+tenant
// Transactor 要求 schema 为空并使用无前缀 inbox，让 Do 设置的事务级 search_path
// 决定当前分区。路由形态若传固定 schema 会 panic，避免去重表与业务表落在不同分区。
func InboxWithTransactor(pool *pgxpool.Pool, transactor *pgtx.Transactor, schema, consumer string, next appkit.EventHandler) appkit.EventHandler {
	if consumer == "" {
		panic("outbox: Inbox 的 consumer 不能为空（去重按 (consumer, event_id)）")
	}
	if transactor == nil {
		panic("outbox: InboxWithTransactor 的 transactor 不能为空")
	}
	var insertSQL string
	if transactor.IsRouted() {
		if schema != "" {
			panic("outbox: routed InboxWithTransactor 的 schema 必须为空（由 search_path 路由）")
		}
		insertSQL = `INSERT INTO inbox (consumer, event_id, topic) VALUES ($1, $2, $3) ON CONFLICT (consumer, event_id) DO NOTHING`
	} else {
		mustSchema(schema)
		insertSQL = fmt.Sprintf(
			`INSERT INTO %s.inbox (consumer, event_id, topic) VALUES ($1, $2, $3) ON CONFLICT (consumer, event_id) DO NOTHING`,
			ident(schema))
	}

	return func(ctx context.Context, evt appkit.Event) error {
		if evt.ID == "" {
			// 无 ID 无从去重；重试也不会好，直接判为坏事件。
			return apperr.InvalidArgument("outbox: 事件缺少 ID，无法去重（topic %q）", evt.Topic)
		}
		if err := transactor.Do(ctx, func(txctx context.Context) error {
			tag, err := pgtx.From(txctx, pool).Exec(txctx, insertSQL, consumer, evt.ID, evt.Topic)
			if err != nil {
				return fmt.Errorf("outbox: 写入 inbox（事件 %s）: %w", evt.ID, err)
			}
			if tag.RowsAffected() == 0 {
				return nil
			}
			return next(txctx, evt)
		}); err != nil {
			return fmt.Errorf("outbox: 处理 inbox 事件 %s: %w", evt.ID, err)
		}
		return nil
	}
}
