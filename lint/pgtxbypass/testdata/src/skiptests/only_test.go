package skiptests

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

func f(ctx context.Context, pool *pgxpool.Pool) {
	_, _ = pool.Exec(ctx, "SELECT 1")
}
