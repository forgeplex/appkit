package dbtest_test

import (
	"context"
	"testing"

	"github.com/forgeplex/appkit/internal/dbtest"
)

// TestSchemaAppliesMigration 用真库把 Schema 本身过一遍：迁移建得出的表
// 要真能写入。无 TEST_DATABASE_URL 时经 Pool 的 skip 路径跳过——那条路径
// 每次裸 `make check` 都在走，不需要单测再证一遍。
func TestSchemaAppliesMigration(t *testing.T) {
	pool := dbtest.Pool(t)
	schema := dbtest.Schema(t, pool, "dbtest", func(schema string) string {
		return `CREATE TABLE "` + schema + `".t (v int NOT NULL)`
	})
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO "`+schema+`".t (v) VALUES (1)`); err != nil {
		t.Fatalf("写入迁移建的表: %v", err)
	}
}

// TestSchemaDroppedAfterSubtest 锁住清理契约：schema 的拆除挂在注册它的
// 那个 t 上，子测试一结束就生效，不是等整个二进制跑完。清理不生效的话，
// 跑几轮 DB 测试就在公共测试库里留一堆孤儿 schema，而没人会立刻发现。
func TestSchemaDroppedAfterSubtest(t *testing.T) {
	pool := dbtest.Pool(t)
	var schema string
	t.Run("子测试里建 schema", func(t *testing.T) {
		schema = dbtest.Schema(t, pool, "dbtest", nil)
	})
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_namespace WHERE nspname = $1`, schema).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("子测试结束后 schema %q 仍存在——DROP CASCADE 清理没生效", schema)
	}
}
