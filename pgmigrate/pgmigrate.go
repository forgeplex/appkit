// Package pgmigrate 是配合 appkit.Migrator 选项的极简 schema 级迁移 runner。
//
// 每个 MigrationSet 对应一个域独占的 Postgres schema（DESIGN §8）：runner 负责
// 建 schema、维护 {schema}.schema_migrations 历史表，并把 fs.FS 根下的 *.sql
// 按文件名升序应用。version 就是文件名。多副本并发启动安全：所有检查/应用都在
// 持有 pg_advisory_xact_lock(hashtext(schema)) 的事务内进行，同 schema 串行、
// 异 schema 互不阻塞。
package pgmigrate

import (
	"context"
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
		return nil
	})
}

func applyFile(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, schema, name string) error {
	body, err := fs.ReadFile(fsys, name)
	if err != nil {
		return fmt.Errorf("读取: %w", err)
	}
	return inLockedTx(ctx, pool, schema, func(ptx pgx.Tx) error {
		// 已应用检查必须在锁内：并发副本在锁上排队，后到者在此看到先到者的记录。
		var applied bool
		if err := ptx.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM "+historyTable(schema)+" WHERE version = $1)", name,
		).Scan(&applied); err != nil {
			return fmt.Errorf("查询已应用版本: %w", err)
		}
		if applied {
			return nil
		}
		// 无参 Exec 走 simple protocol，迁移文件可含多条语句。
		if _, err := ptx.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("执行: %w", err)
		}
		if _, err := ptx.Exec(ctx,
			"INSERT INTO "+historyTable(schema)+" (version) VALUES ($1)", name); err != nil {
			return fmt.Errorf("记录版本: %w", err)
		}
		return nil
	})
}
