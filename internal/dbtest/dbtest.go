// Package dbtest 收拢 appkit 自身需要 Postgres 的集成测试样板：
// 连池、建随机 schema、应用迁移、测试结束整体拆除。
//
// 这套「$TEST_DATABASE_URL 守卫 + 随机 schema + DROP CASCADE」此前在
// outbox/idem/audit/scaffold/job/pgtx 各写一遍，细节已经开始漂移：有的
// pgx.Identifier 转义、有的裸拼标识符，随机名有取时间戳的、有取 rand 的。
// 收敛到一处，转义与清理语义只有一份实现可对。
//
// 约束：本包不得 import appkit 的任何包——outbox/pgtx 的内部测试包
// （package outbox / package job）要用这里，反向依赖会成环。池就用裸
// pgxpool，够用；走 pgtx.NewPool 的测试差异只在 tracer，与被测行为无关。
package dbtest

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool 返回连到 $TEST_DATABASE_URL 的池（建好即 ping），测试结束自动关闭；
// 环境变量未设置时 skip——这正是 `make check` 不带库跑、`make test-db`
// 才会跑到 DB 用例的那道门。
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过需要 Postgres 的测试")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("建池: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

var schemaSeq atomic.Int64

// Migration 把 schema 名翻译成要应用的 DDL。各基础设施包的
// MigrationSQL(schema) 天生就是这个形状，直接传函数值即可。
type Migration func(schema string) string

// Schema 建一个以 prefix 开头的随机 schema、应用 migration、返回 schema
// 名；测试结束 DROP SCHEMA ... CASCADE 整体拆除。migration 可为 nil
// （只借一个干净的空 schema，scaffold 的真库读写就是自建表）。
//
// schema 名是纳秒时间戳加进程内序号：跨进程（go test 每个包一个进程）
// 与同进程并发用例之间都不撞。标识符拼接统一过 pgx.Identifier 转义。
func Schema(t testing.TB, pool *pgxpool.Pool, prefix string, migration Migration) string {
	t.Helper()
	name := fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixNano(), schemaSeq.Add(1))
	ident := pgx.Identifier{name}.Sanitize()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		t.Fatalf("建 schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+ident+" CASCADE")
	})
	if migration != nil {
		if _, err := pool.Exec(ctx, migration(name)); err != nil {
			t.Fatalf("应用迁移: %v", err)
		}
	}
	return name
}
