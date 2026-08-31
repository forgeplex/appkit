// appkit outbox —— 死信运维子命令：列出死信（缺省动作）或按事件 ID 放回
// 投递队列。操作目标与实现见 outbox.DeadLetters。
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit/outbox"
	"github.com/forgeplex/appkit/pgtx"
)

// allAtOnce 是 -all 单次放回的上限：死信堆积到这个量级时先修根因
// （为什么每个事件都在死），而不是无界地一把放回。
const allAtOnce = 10000

func init() {
	register(Command{
		Name:    "outbox",
		Summary: "死信运维：列出死信（缺省），按事件 ID 放回投递队列（-all 全放）",
		Run:     runOutbox,
	})
}

func runOutbox(args []string) error {
	fs := flag.NewFlagSet("outbox", flag.ContinueOnError)
	dsn := fs.String("dsn", "", "Postgres 连接串，缺省取 $TEST_DATABASE_URL")
	schema := fs.String("schema", "", "域 schema 名（如 ledger）")
	all := fs.Bool("all", false, "把全部死信放回投递队列")
	limit := fs.Int("limit", 50, "列出死信的最大条数")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ids := fs.Args()
	if *schema == "" {
		return errors.New("缺少 -schema（域 schema 名，如 ledger）")
	}
	if *all && len(ids) > 0 {
		return errors.New("-all 与按 ID 放回不能同时使用")
	}
	if *dsn == "" {
		*dsn = os.Getenv("TEST_DATABASE_URL")
	}
	if *dsn == "" {
		return errors.New("缺少数据库连接串：加 -dsn 或设置 TEST_DATABASE_URL（本地可用 make dev-db 起一个）")
	}

	ctx := context.Background()
	pool, err := pgtx.NewPool(ctx, *dsn)
	if err != nil {
		return fmt.Errorf("连接数据库: %w", err)
	}
	defer pool.Close()
	dl, err := newDeadLetters(pool, *schema)
	if err != nil {
		return err
	}

	switch {
	case *all:
		letters, err := dl.List(ctx, allAtOnce)
		if err != nil {
			return err
		}
		if len(letters) == allAtOnce {
			fmt.Printf("::warning::死信达到单次放回上限 %d 条，其余留待下一次\n", allAtOnce)
		}
		return retryDeadLetters(ctx, dl, letters)
	case len(ids) > 0:
		n, err := dl.Retry(ctx, ids...)
		if err != nil {
			return err
		}
		fmt.Printf("放回 %d 条死信（不存在的 ID 不计入）\n", n)
		return nil
	default:
		letters, err := dl.List(ctx, *limit)
		if err != nil {
			return err
		}
		if len(letters) == 0 {
			fmt.Println("无死信")
			return nil
		}
		fmt.Printf("%-38s %-20s %8s  %-19s %s\n", "EVENT_ID", "TOPIC", "ATTEMPTS", "FAILED_AT", "LAST_ERROR")
		for _, l := range letters {
			err := l.LastError
			if len(err) > 60 {
				err = err[:60] + "…"
			}
			fmt.Printf("%-38s %-20s %8d  %-19s %s\n",
				l.ID, l.Topic, l.Attempts, l.FailedAt.Local().Format(time.DateTime), err)
		}
		fmt.Printf("\n共 %d 条。修复消费方后放回：appkit outbox -schema %s -dsn ... <EVENT_ID>…\n", len(letters), *schema)
		return nil
	}
}

func retryDeadLetters(ctx context.Context, dl *outbox.DeadLetters, letters []outbox.DeadLetter) error {
	ids := make([]string, len(letters))
	for i, l := range letters {
		ids[i] = l.ID
	}
	n, err := dl.Retry(ctx, ids...)
	if err != nil {
		return err
	}
	fmt.Printf("放回 %d 条死信\n", n)
	return nil
}

// newDeadLetters 包一层 recover：NewDeadLetters 对非法 schema panic
// （编程错误语义），CLI 的 schema 是用户输入，得转成错误而不是崩进程。
func newDeadLetters(pool *pgxpool.Pool, schema string) (dl *outbox.DeadLetters, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%v", p)
		}
	}()
	return outbox.NewDeadLetters(pool, schema), nil
}
