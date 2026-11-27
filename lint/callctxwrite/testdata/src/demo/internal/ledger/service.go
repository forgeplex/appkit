package ledger

import (
	"context"

	"github.com/forgeplex/appkit/callctx"
)

func adminBackfill(ctx context.Context, tenant string) context.Context {
	// 业务层受权代租户操作放行。
	return callctx.With(ctx, callctx.Meta{TenantID: tenant})
}
