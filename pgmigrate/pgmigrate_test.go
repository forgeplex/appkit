package pgmigrate_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/pgmigrate"
)

// ---- 不需要 Postgres 的单测 ----

// 校验发生在任何数据库访问之前，因此 pool 传 nil 即可。
func TestRunnerRejectsBadSchema(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		schema string
	}{
		{name: "空串", schema: ""},
		{name: "数字开头", schema: "1ledger"},
		{name: "大写字母", schema: "Ledger"},
		{name: "连字符", schema: "led-ger"},
		{name: "带点跨 schema", schema: "public.x"},
		{name: "带空格", schema: "led ger"},
		{name: "带引号", schema: `led"ger`},
		{name: "SQL 注入", schema: "x; drop schema public cascade; --"},
		{name: "非 ASCII", schema: "账本"},
	}
	run := pgmigrate.Runner(nil)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := run(context.Background(), []appkit.MigrationSet{
				{Schema: tc.schema, FS: fstest.MapFS{}, Module: "m"},
			})
			if err == nil {
				t.Fatalf("schema %q 应被拒绝", tc.schema)
			}
			if !apperr.Is(err, apperr.CodeInvalidArgument) {
				t.Fatalf("错误码 = %v, want %s", err, apperr.CodeInvalidArgument)
			}
		})
	}
}

// ---- 需要 Postgres 的测试（TEST_DATABASE_URL）----

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过需要 Postgres 的测试")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("建池: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

var schemaSeq atomic.Int64

// testSchema 生成随机 schema 名并登记 DROP ... CASCADE 清理。
func testSchema(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	name := fmt.Sprintf("pgmigrate_test_%d_%d", time.Now().UnixNano(), schemaSeq.Add(1))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{name}.Sanitize()+" CASCADE")
	})
	return name
}

// mapFS 把文件内容里的 {schema} 占位符替换为真实 schema 名后构造 fstest.MapFS。
func mapFS(files map[string]string, schema string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(strings.ReplaceAll(body, "{schema}", schema))}
	}
	return fsys
}

func appliedVersions(t *testing.T, pool *pgxpool.Pool, schema string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		"SELECT version FROM "+pgx.Identifier{schema, "schema_migrations"}.Sanitize()+" ORDER BY version")
	if err != nil {
		t.Fatalf("查询 schema_migrations: %v", err)
	}
	defer rows.Close()
	var versions []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("扫描版本: %v", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历版本: %v", err)
	}
	return versions
}

func countRows(t *testing.T, pool *pgxpool.Pool, schema, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM "+pgx.Identifier{schema, table}.Sanitize()).Scan(&n); err != nil {
		t.Fatalf("count %s.%s: %v", schema, table, err)
	}
	return n
}

func tableExists(t *testing.T, pool *pgxpool.Pool, schema, table string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		"SELECT to_regclass($1) IS NOT NULL", pgx.Identifier{schema, table}.Sanitize()).Scan(&exists); err != nil {
		t.Fatalf("to_regclass: %v", err)
	}
	return exists
}

func TestRunnerPostgres(t *testing.T) {
	// seed 迁移每应用一次插入一行：行数即实际应用次数，用于验证零重复应用。
	twoMigrations := map[string]string{
		"001_init.sql": "CREATE TABLE {schema}.items (v text NOT NULL);",
		"002_seed.sql": "INSERT INTO {schema}.items (v) VALUES ('seed');",
	}
	tests := []struct {
		name         string
		files        map[string]string
		runs         int
		wantErr      bool
		wantVersions []string
		verify       func(t *testing.T, pool *pgxpool.Pool, schema string)
	}{
		{
			name:         "两个迁移跑一遍",
			files:        twoMigrations,
			runs:         1,
			wantVersions: []string{"001_init.sql", "002_seed.sql"},
			verify: func(t *testing.T, pool *pgxpool.Pool, schema string) {
				if n := countRows(t, pool, schema, "items"); n != 1 {
					t.Errorf("seed 行数 = %d, want 1", n)
				}
			},
		},
		{
			name:         "跑两遍第二遍零重复应用",
			files:        twoMigrations,
			runs:         2,
			wantVersions: []string{"001_init.sql", "002_seed.sql"},
			verify: func(t *testing.T, pool *pgxpool.Pool, schema string) {
				if n := countRows(t, pool, schema, "items"); n != 1 {
					t.Errorf("seed 行数 = %d, want 1（第二遍不得重复应用）", n)
				}
			},
		},
		{
			name: "坏 SQL 报错且 version 不被记录",
			files: map[string]string{
				"001_init.sql": "CREATE TABLE {schema}.items (v text NOT NULL);",
				"002_bad.sql":  "THIS IS NOT SQL;",
			},
			runs:         1,
			wantErr:      true,
			wantVersions: []string{"001_init.sql"},
		},
		{
			name: "坏文件内已执行的语句同事务回滚",
			files: map[string]string{
				"001_bad.sql": "CREATE TABLE {schema}.orphan (v text); THIS IS NOT SQL;",
			},
			runs:         1,
			wantErr:      true,
			wantVersions: nil,
			verify: func(t *testing.T, pool *pgxpool.Pool, schema string) {
				if tableExists(t, pool, schema, "orphan") {
					t.Error("坏文件里先执行的 CREATE TABLE 应随事务回滚")
				}
			},
		},
		{
			name: "非 sql 与子目录文件被忽略",
			files: map[string]string{
				"001_init.sql":   "CREATE TABLE {schema}.items (v text NOT NULL);",
				"README.md":      "not sql",
				"sub/003_no.sql": "THIS IS NOT SQL;",
			},
			runs:         1,
			wantVersions: []string{"001_init.sql"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pool := testPool(t)
			schema := testSchema(t, pool)
			run := pgmigrate.Runner(pool)
			sets := []appkit.MigrationSet{{Schema: schema, FS: mapFS(tc.files, schema), Module: "m"}}

			var lastErr error
			for i := 0; i < tc.runs; i++ {
				lastErr = run(context.Background(), sets)
			}
			if (lastErr != nil) != tc.wantErr {
				t.Fatalf("Runner err = %v, wantErr = %v", lastErr, tc.wantErr)
			}
			got := appliedVersions(t, pool, schema)
			want := append([]string(nil), tc.wantVersions...)
			sort.Strings(want)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("已应用版本 = %v, want %v", got, want)
			}
			if tc.verify != nil {
				tc.verify(t, pool, schema)
			}
		})
	}
}

// 复现评审场景：保留字 schema 名（如 order）必须整流程可用——建 schema、
// 建历史表、已应用检查、记录版本四处拼接都要带引号，否则启动即失败。
func TestRunnerReservedWordSchema(t *testing.T) {
	pool := testPool(t)
	const schema = "order"
	drop := func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS "order" CASCADE`)
	}
	drop() // 预清理上次失败可能的残留
	t.Cleanup(drop)

	// 迁移文件里的 schema 引用同样需要引号（{schema} 占位符替换为带引号形态）。
	fsys := mapFS(map[string]string{
		"001_init.sql": "CREATE TABLE {schema}.items (v text NOT NULL);",
		"002_seed.sql": "INSERT INTO {schema}.items (v) VALUES ('seed');",
	}, pgx.Identifier{schema}.Sanitize())
	sets := []appkit.MigrationSet{{Schema: schema, FS: fsys, Module: "m"}}

	run := pgmigrate.Runner(pool)
	if err := run(context.Background(), sets); err != nil {
		t.Fatalf("保留字 schema 首跑失败: %v", err)
	}
	// 第二遍走"已应用检查"路径，同样必须带引号可用且零重复应用。
	if err := run(context.Background(), sets); err != nil {
		t.Fatalf("保留字 schema 重跑失败: %v", err)
	}
	if got := appliedVersions(t, pool, schema); fmt.Sprint(got) != fmt.Sprint([]string{"001_init.sql", "002_seed.sql"}) {
		t.Fatalf("已应用版本 = %v", got)
	}
	if n := countRows(t, pool, schema, "items"); n != 1 {
		t.Fatalf("seed 行数 = %d, want 1", n)
	}
}

// 坏迁移修复后重跑：已应用的续上、修好的补上。
func TestRunnerResumesAfterFix(t *testing.T) {
	pool := testPool(t)
	schema := testSchema(t, pool)
	run := pgmigrate.Runner(pool)

	broken := mapFS(map[string]string{
		"001_init.sql": "CREATE TABLE {schema}.items (v text NOT NULL);",
		"002_bad.sql":  "THIS IS NOT SQL;",
	}, schema)
	if err := run(context.Background(), []appkit.MigrationSet{{Schema: schema, FS: broken, Module: "m"}}); err == nil {
		t.Fatal("坏 SQL 应报错")
	}

	fixed := mapFS(map[string]string{
		"001_init.sql": "CREATE TABLE {schema}.items (v text NOT NULL);",
		"002_bad.sql":  "INSERT INTO {schema}.items (v) VALUES ('fixed');",
	}, schema)
	if err := run(context.Background(), []appkit.MigrationSet{{Schema: schema, FS: fixed, Module: "m"}}); err != nil {
		t.Fatalf("修复后重跑: %v", err)
	}
	if got := appliedVersions(t, pool, schema); fmt.Sprint(got) != fmt.Sprint([]string{"001_init.sql", "002_bad.sql"}) {
		t.Fatalf("已应用版本 = %v", got)
	}
	if n := countRows(t, pool, schema, "items"); n != 1 {
		t.Fatalf("行数 = %d, want 1", n)
	}
}

// 模拟多副本并发启动：advisory lock 串行化后每个迁移只生效一次且全部成功。
func TestRunnerConcurrentReplicas(t *testing.T) {
	pool := testPool(t)
	schema := testSchema(t, pool)
	fsys := mapFS(map[string]string{
		"001_init.sql": "CREATE TABLE {schema}.items (v text NOT NULL);",
		"002_seed.sql": "INSERT INTO {schema}.items (v) VALUES ('seed');",
	}, schema)
	sets := []appkit.MigrationSet{{Schema: schema, FS: fsys, Module: "m"}}

	const replicas = 4
	errs := make([]error, replicas)
	var wg sync.WaitGroup
	for i := range replicas {
		wg.Go(func() {
			errs[i] = pgmigrate.Runner(pool)(context.Background(), sets)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("副本 %d: %v", i, err)
		}
	}
	if n := countRows(t, pool, schema, "items"); n != 1 {
		t.Fatalf("seed 行数 = %d, want 1（并发下不得重复应用）", n)
	}
	if got := appliedVersions(t, pool, schema); len(got) != 2 {
		t.Fatalf("已应用版本 = %v, want 2 条", got)
	}
}

func storedChecksum(t *testing.T, pool *pgxpool.Pool, schema, version string) *string {
	t.Helper()
	var sum *string
	if err := pool.QueryRow(context.Background(),
		"SELECT checksum FROM "+pgx.Identifier{schema, "schema_migrations"}.Sanitize()+
			" WHERE version = $1", version).Scan(&sum); err != nil {
		t.Fatalf("查询 checksum: %v", err)
	}
	return sum
}

func columnExists(t *testing.T, pool *pgxpool.Pool, schema, table, column string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = $2 AND column_name = $3)`,
		schema, table, column).Scan(&exists); err != nil {
		t.Fatalf("查询列存在性: %v", err)
	}
	return exists
}

// 改动已应用的迁移：启动期以 MIGRATION_DRIFT 拒绝。
// 没有这道守卫时 runner 只比对 version，改动会被静默跳过，库与代码就此分叉。
func TestRunnerRejectsModifiedMigration(t *testing.T) {
	pool := testPool(t)
	schema := testSchema(t, pool)
	run := pgmigrate.Runner(pool)
	sets := func(fsys fstest.MapFS) []appkit.MigrationSet {
		return []appkit.MigrationSet{{Schema: schema, FS: fsys, Module: "m"}}
	}

	orig := mapFS(map[string]string{
		"001_init.sql": "CREATE TABLE {schema}.items (v text NOT NULL);",
	}, schema)
	if err := run(context.Background(), sets(orig)); err != nil {
		t.Fatalf("首次应用: %v", err)
	}

	edited := mapFS(map[string]string{
		"001_init.sql": "CREATE TABLE {schema}.items (v text NOT NULL, extra int);",
	}, schema)
	err := run(context.Background(), sets(edited))
	if err == nil {
		t.Fatal("改动已应用的迁移必须报错")
	}
	if !apperr.Is(err, apperr.CodeMigrationDrift) {
		t.Fatalf("错误码不符: %v, want %s", err, apperr.CodeMigrationDrift)
	}
	// 修复方法必须出现在 Error() 里：这个错误唯一的读者是盯着 stderr 的人，
	// 而 apperr 的 details 不进 Error() 字符串。
	for _, want := range []string{"001_init.sql", "新增迁移文件", "UPDATE "} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息缺少 %q，运维只能看到这一行: %v", want, err)
		}
	}
	if columnExists(t, pool, schema, "items", "extra") {
		t.Fatal("被拒的迁移不得改动 schema")
	}
	// 改回原内容即可继续——守卫拦的是不一致，不是拦人。
	if err := run(context.Background(), sets(orig)); err != nil {
		t.Fatalf("恢复原内容后应放行: %v", err)
	}
}

// 旧版本 appkit 写入的历史行没有 checksum：回填而非报错，升级不需人工干预。
func TestRunnerBackfillsLegacyChecksum(t *testing.T) {
	pool := testPool(t)
	schema := testSchema(t, pool)
	run := pgmigrate.Runner(pool)
	fsys := mapFS(map[string]string{
		"001_init.sql": "CREATE TABLE {schema}.items (v text NOT NULL);",
	}, schema)
	sets := []appkit.MigrationSet{{Schema: schema, FS: fsys, Module: "m"}}

	if err := run(context.Background(), sets); err != nil {
		t.Fatalf("首次应用: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		"UPDATE "+pgx.Identifier{schema, "schema_migrations"}.Sanitize()+
			" SET checksum = NULL"); err != nil {
		t.Fatalf("模拟旧版本历史行: %v", err)
	}
	if err := run(context.Background(), sets); err != nil {
		t.Fatalf("老库应放行: %v", err)
	}
	if got := storedChecksum(t, pool, schema, "001_init.sql"); got == nil {
		t.Fatal("checksum 应被回填")
	}
}
