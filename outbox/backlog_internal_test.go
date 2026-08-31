package outbox

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit/internal/dbtest"
)

// TestBacklogSQLMatchesPartialIndex 是无需数据库的漂移守卫：积压查询的
// WHERE 必须与 outbox_unpublished_idx 的部分索引条件逐字一致，否则
// Postgres 用不上该索引，这条每分钟一次的观测查询会退化成全表扫描——
// 而且是随历史增长越来越慢的那种，没人会立刻发现。
func TestBacklogSQLMatchesPartialIndex(t *testing.T) {
	t.Parallel()
	_, idxWhere, ok := strings.Cut(migrationTemplate, "ON %[1]s.outbox (created_at) WHERE ")
	if !ok {
		t.Fatal("migrationTemplate 里找不到 outbox_unpublished_idx 的部分索引条件")
	}
	idxWhere, _, _ = strings.Cut(idxWhere, ";")

	sql := NewRelay(nil, "s", nil).backlogSQL
	if !strings.Contains(sql, "WHERE "+idxWhere) {
		t.Errorf("积压查询的 WHERE 与部分索引条件不一致\n索引: %q\n查询: %q", idxWhere, sql)
	}
}

// backlogTestSchema 建随机 schema 并应用 MigrationSQL，结束后整体 DROP CASCADE。
func backlogTestSchema(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool := dbtest.Pool(t)
	return pool, dbtest.Schema(t, pool, "outbox_backlog_test", MigrationSQL)
}

// insertAged 直插一条事件并指定"多少秒前创建"，用来构造确定的积压年龄。
// markCol 为 published_at 或 failed_at 时该列置 now()，空串表示仍待投递。
func insertAged(t *testing.T, pool *pgxpool.Pool, schema string, ageSec float64, markCol string) {
	t.Helper()
	col, val := "", ""
	if markCol != "" {
		col, val = ", "+markCol, ", now()"
	}
	sql := fmt.Sprintf(
		`INSERT INTO %s.outbox (id, topic, payload, created_at%s)
		 VALUES (gen_random_uuid(), 'test.topic', '{}'::jsonb, now() - $1 * interval '1 second'%s)`,
		ident(schema), col, val)
	if _, err := pool.Exec(context.Background(), sql, ageSec); err != nil {
		t.Fatalf("插入事件: %v", err)
	}
}

// TestRelayBacklog 用真库跑一遍积压查询。这条 SQL 平时没有测试覆盖：
// 它只在采集周期里被 gauge 回调调用，而未配置 exporter 时全局 provider
// 是 noop，回调根本不执行——写错了也不会有任何人报错。
func TestRelayBacklog(t *testing.T) {
	pool, schema := backlogTestSchema(t)
	r := NewRelay(pool, schema, nil)
	ctx := context.Background()

	// 空表：0 条、年龄 0（而不是 NULL 扫描失败）。
	b, err := r.backlog(ctx)
	if err != nil {
		t.Fatalf("空表积压: %v", err)
	}
	if b.Pending != 0 || b.OldestAge != 0 {
		t.Fatalf("空表应为零积压，得到 %+v", b)
	}

	insertAged(t, pool, schema, 90, "")               // 最老的待投递
	insertAged(t, pool, schema, 30, "")               // 待投递
	insertAged(t, pool, schema, 5, "")                // 待投递
	insertAged(t, pool, schema, 3600, "published_at") // 已投递，不算积压
	insertAged(t, pool, schema, 7200, "failed_at")    // 死信，不算积压

	b, err = r.backlog(ctx)
	if err != nil {
		t.Fatalf("积压查询: %v", err)
	}
	if b.Pending != 3 {
		t.Errorf("Pending = %d, want 3（已投递与死信不计入）", b.Pending)
	}
	// 年龄取最老一条，不是最新一条——搞反了会让告警永远不触发。
	if b.OldestAge < 89*time.Second || b.OldestAge > 150*time.Second {
		t.Errorf("OldestAge = %v, want ≈90s", b.OldestAge)
	}
}
