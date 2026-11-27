package a

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type badStore struct {
	tx pgx.Tx // want `struct 字段禁止出现 pgx\.Tx`
}

func badParam(ctx context.Context, tx pgx.Tx) error { // want `函数参数禁止出现 pgx\.Tx`
	_ = ctx
	_ = tx
	return nil
}

//appkit:ignore txparam -- 底层事务适配器
func exempt(tx pgx.Tx) { _ = tx }

func good(ctx context.Context) error { return nil }
