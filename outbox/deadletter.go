package outbox

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit/apperr"
)

// DeadLetter 是一条死信的快照：relay 重试达上限（failed_at 置位）后事件
// 移出投递热路径，等在这里等待人工处理。
type DeadLetter struct {
	ID        string
	Topic     string
	Attempts  int
	LastError string
	CreatedAt time.Time
	FailedAt  time.Time
}

// DeadLetters 是死信的运维面：列出与放回。供 `appkit outbox` 子命令与
// 域仓库自己的运维工具使用；业务代码不该出现在这里。
type DeadLetters struct {
	pool   *pgxpool.Pool
	schema string

	listSQL  string
	retrySQL string
}

// NewDeadLetters 构造 DeadLetters。schema 不合法时 panic（与包内其他构造器
// 同规：schema 在构造期写死属编程错误；接收用户输入的调用方先自行校验，
// 见 internal/cli/outbox.go）。
func NewDeadLetters(pool *pgxpool.Pool, schema string) *DeadLetters {
	mustSchema(schema)
	return &DeadLetters{
		pool:   pool,
		schema: schema,
		listSQL: fmt.Sprintf(
			`SELECT id, topic, attempts, COALESCE(last_error, ''), created_at, failed_at
			 FROM %s.outbox WHERE failed_at IS NOT NULL
			 ORDER BY failed_at DESC LIMIT $1`,
			ident(schema)),
		retrySQL: fmt.Sprintf(
			`UPDATE %s.outbox
			    SET failed_at = NULL, attempts = 0, next_attempt_at = now(), claimed_until = NULL
			  WHERE id = ANY($1) AND failed_at IS NOT NULL AND published_at IS NULL`,
			ident(schema)),
	}
}

// List 返回最新的 limit 条死信（按 failed_at 倒序）。limit 非正时取 50。
func (d *DeadLetters) List(ctx context.Context, limit int) ([]DeadLetter, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.pool.Query(ctx, d.listSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("outbox: 查询死信: %w", err)
	}
	defer rows.Close()
	var out []DeadLetter
	for rows.Next() {
		var dl DeadLetter
		if err := rows.Scan(&dl.ID, &dl.Topic, &dl.Attempts, &dl.LastError,
			&dl.CreatedAt, &dl.FailedAt); err != nil {
			return nil, fmt.Errorf("outbox: 读取死信行: %w", err)
		}
		out = append(out, dl)
	}
	return out, rows.Err()
}

// Retry 把死信放回投递队列：清 failed_at、attempts 归零、next_attempt_at
// 立即到期、释放残留租约。只动死信态（failed_at 非空且未发布）的行，
// 返回实际放回条数——0 表示 id 不存在、已发布或尚未死信。
//
// 语义：修好消费方 bug 之后的一步闭环。bug 没修好也不破坏任何东西——
// 事件按正常退避重新走满整个重试预算后再次死信，与首次如出一辙。
// attempts 归零意味着「这次人工放回」不占用自动重试的预算；last_error
// 保留（投递成功后仍留在行上，与既有行为一致）。
func (d *DeadLetters) Retry(ctx context.Context, ids ...string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	for _, id := range ids {
		if _, err := uuid.Parse(id); err != nil {
			return 0, apperr.InvalidArgument("outbox: 事件 ID %q 不是合法 uuid", id).WithCause(err)
		}
	}
	tag, err := d.pool.Exec(ctx, d.retrySQL, ids)
	if err != nil {
		return 0, fmt.Errorf("outbox: 放回死信: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
