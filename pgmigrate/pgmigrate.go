// Package pgmigrate 是配合 appkit.Migrator 选项的极简 schema 级迁移 runner。
//
// 每个 MigrationSet 对应一个域独占的 Postgres schema（DESIGN §8）：runner 负责
// 建 schema、维护 {schema}.schema_migrations 历史表，并把 fs.FS 根下的 *.sql
// 按文件名升序应用。version 就是文件名。多副本并发启动安全：所有检查/应用都在
// 持有 pg_advisory_xact_lock(hashtext(schema)) 的事务内进行，同 schema 串行、
// 异 schema 互不阻塞。
//
// 已应用的迁移是不可变的：历史表记录内容的 sha256，启动时逐个比对，
// 改动已应用的文件会以 MIGRATION_DRIFT 拒绝启动（见 verifyChecksum）。
package pgmigrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/apperr"
)

// schema 名会被拼进 DDL（标识符无法参数化），必须白名单校验防注入。
var schemaRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// historyTable 返回带引号的 {schema}.schema_migrations 标识符。
// 所有拼接点统一走带引号形态：保留字 schema 名（如 order）才能正常工作，
// 与 outbox/idem 的 pgx.Identifier 风格一致。
func historyTable(schema string) string {
	return pgx.Identifier{schema, "schema_migrations"}.Sanitize()
}

// Runner 返回可直接传给 appkit.Migrator 的迁移执行器。
//
// 对每个 MigrationSet：CREATE SCHEMA IF NOT EXISTS → 确保 schema_migrations
// 历史表 → 逐个应用未记录的 *.sql。每个文件在自己的事务里执行并记录版本：
// 坏 SQL 整体回滚，版本不会被记录，修复后重跑即可续上。
func Runner(pool *pgxpool.Pool) func(ctx context.Context, sets []appkit.MigrationSet) error {
	return func(ctx context.Context, sets []appkit.MigrationSet) error {
		for _, set := range sets {
			if err := applySet(ctx, pool, set); err != nil {
				return fmt.Errorf("pgmigrate: 模块 %q schema %q: %w", set.Module, set.Schema, err)
			}
		}
		return nil
	}
}

func applySet(ctx context.Context, pool *pgxpool.Pool, set appkit.MigrationSet) error {
	if !schemaRe.MatchString(set.Schema) {
		return apperr.InvalidArgument("schema 名 %q 不合法：须匹配 %s", set.Schema, schemaRe)
	}
	if err := ensureSchema(ctx, pool, set.Schema); err != nil {
		return err
	}
	files, err := fs.Glob(set.FS, "*.sql")
	if err != nil {
		return fmt.Errorf("列出迁移文件: %w", err)
	}
	sort.Strings(files)
	for _, name := range files {
		if err := applyFile(ctx, pool, set.FS, set.Schema, name); err != nil {
			return fmt.Errorf("迁移 %s: %w", name, err)
		}
	}
	return nil
}

// inLockedTx 在一个持有 schema 级 advisory lock 的事务内执行 fn。
// 锁是 xact 级：提交/回滚即释放，不需要显式 unlock，崩溃也不会遗留锁。
func inLockedTx(ctx context.Context, pool *pgxpool.Pool, schema string, fn func(ptx pgx.Tx) error) error {
	ptx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启事务: %w", err)
	}
	// 提交成功后 Rollback 返回 ErrTxClosed，忽略即可。
	// 剥离取消信号：ROLLBACK 必须尽力发出，否则连接被整个废弃（同 pgtx）。
	defer func() { _ = ptx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := ptx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", schema); err != nil {
		return fmt.Errorf("获取 advisory lock: %w", err)
	}
	if err := fn(ptx); err != nil {
		return err
	}
	if err := ptx.Commit(ctx); err != nil {
		return fmt.Errorf("提交: %w", err)
	}
	return nil
}

func ensureSchema(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	return inLockedTx(ctx, pool, schema, func(ptx pgx.Tx) error {
		if _, err := ptx.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize()); err != nil {
			return fmt.Errorf("创建 schema: %w", err)
		}
		if _, err := ptx.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+historyTable(schema)+
			" (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())"); err != nil {
			return fmt.Errorf("创建迁移历史表: %w", err)
		}
		// checksum 列是后加的：老库补列，历史行留 NULL（内容未知），
		// 由 applyFile 首次遇到时回填。升级 appkit 不需要人工干预。
		if _, err := ptx.Exec(ctx, "ALTER TABLE "+historyTable(schema)+
			" ADD COLUMN IF NOT EXISTS checksum text"); err != nil {
			return fmt.Errorf("补齐迁移历史表 checksum 列: %w", err)
		}
		// 历史表是本框架唯一不经迁移文件建出来的表，说明也就只能落在这里。
		// 语句幂等，每次启动重设一遍等价于不变。
		if _, err := ptx.Exec(ctx, "COMMENT ON TABLE "+historyTable(schema)+
			" IS '迁移历史：version + 内容 sha256，启动期逐个比对，不符即拒绝启动。'"); err != nil {
			return fmt.Errorf("写迁移历史表说明: %w", err)
		}
		return nil
	})
}

// checksum 返回迁移内容的 sha256（hex）。内容按字节取摘要，不做换行归一化——
// 生成的 .gitattributes 用 "*.sql text eol=lf" 保证跨平台 checkout 一致。
func checksum(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func applyFile(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, schema, name string) error {
	body, err := fs.ReadFile(fsys, name)
	if err != nil {
		return fmt.Errorf("读取: %w", err)
	}
	sum := checksum(body)
	return inLockedTx(ctx, pool, schema, func(ptx pgx.Tx) error {
		// 已应用检查必须在锁内：并发副本在锁上排队，后到者在此看到先到者的记录。
		var prev *string // NULL = 旧版本 appkit 写入的行，内容未知
		err := ptx.QueryRow(ctx,
			"SELECT checksum FROM "+historyTable(schema)+" WHERE version = $1", name,
		).Scan(&prev)
		switch {
		case err == nil:
			return verifyChecksum(ctx, ptx, schema, name, sum, prev)
		case errors.Is(err, pgx.ErrNoRows): // 未应用，往下执行
		default:
			return fmt.Errorf("查询已应用版本: %w", err)
		}
		// 无参 Exec 走 simple protocol，迁移文件可含多条语句。
		if _, err := ptx.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("执行: %w", err)
		}
		if _, err := ptx.Exec(ctx,
			"INSERT INTO "+historyTable(schema)+" (version, checksum) VALUES ($1, $2)",
			name, sum); err != nil {
			return fmt.Errorf("记录版本: %w", err)
		}
		return nil
	})
}

// verifyChecksum 守卫已应用迁移的不可变性。
//
// 改动已应用的迁移不会让库跟着变——只会让库与代码静默分叉，且通常到生产
// 才暴露。这里在启动期拦住：唯一正确的改法是新增一个迁移文件。
// prev 为 NULL 表示该行由旧版本 appkit 写入，无从追溯，回填当前值。
func verifyChecksum(ctx context.Context, ptx pgx.Tx, schema, name, sum string, prev *string) error {
	if prev == nil {
		if _, err := ptx.Exec(ctx,
			"UPDATE "+historyTable(schema)+" SET checksum = $1 WHERE version = $2",
			sum, name); err != nil {
			return fmt.Errorf("回填 checksum: %w", err)
		}
		return nil
	}
	if *prev == sum {
		return nil
	}
	// 修复方法写进 message 而不是只放 details：这个错误只在启动期出现，
	// 唯一的读者是盯着 stderr 的人，而 Error() 不含 details。
	return apperr.New(apperr.CodeMigrationDrift, 500, fmt.Sprintf(
		"已应用的迁移 %s 内容被改动（库中 %s，文件 %s）。要改结构请新增迁移文件；"+
			"确认本次改动不影响 schema（如仅改注释）可执行："+
			"UPDATE %s SET checksum = '%s' WHERE version = '%s';",
		name, short(*prev), short(sum), historyTable(schema), sum, name)).
		WithDetail("version", name).
		WithDetail("applied_checksum", *prev).
		WithDetail("file_checksum", sum)
}

// short 截短 checksum，只用于给人看的消息里做区分。
func short(sum string) string {
	if len(sum) <= 12 {
		return sum
	}
	return sum[:12]
}
