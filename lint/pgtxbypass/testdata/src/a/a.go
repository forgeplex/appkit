package a

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type dbtx interface {
	QueryRow(ctx context.Context, sql string) any
}

func f(ctx context.Context, pool *pgxpool.Pool, db dbtx) {
	_, _ = pool.Exec(ctx, "DELETE FROM x") // want `禁止绕过 pgtx\.From\(ctx, pool\) 直接调用 \(\*pgxpool\.Pool\)\.Exec`
	_, _ = pool.Query(ctx, "SELECT 1")     // want `禁止绕过 pgtx\.From\(ctx, pool\) 直接调用 \(\*pgxpool\.Pool\)\.Query`
	_ = pool.QueryRow(ctx, "SELECT 1")     // want `禁止绕过 pgtx\.From\(ctx, pool\) 直接调用 \(\*pgxpool\.Pool\)\.QueryRow`
	_ = pool.SendBatch(ctx, nil)           // want `禁止绕过 pgtx\.From\(ctx, pool\) 直接调用 \(\*pgxpool\.Pool\)\.SendBatch`
	_, _ = pool.CopyFrom(ctx, nil)         // want `禁止绕过 pgtx\.From\(ctx, pool\) 直接调用 \(\*pgxpool\.Pool\)\.CopyFrom`

	// Ping / Begin 与经 pgtx.From 返回的 DBTX 接口均合法。
	_ = pool.Ping(ctx)
	_, _ = pool.Begin(ctx)
	_ = db.QueryRow(ctx, "SELECT 1")

	//appkit:ignore pgtxbypass -- 启动期表结构校验
	_, _ = pool.Exec(ctx, "SELECT 1")
}
