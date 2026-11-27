package http

import (
	"context"

	"github.com/forgeplex/appkit/callctx"
)

func handle(ctx context.Context) context.Context {
	_ = callctx.From(ctx)
	//appkit:ignore callctxwrite -- 仅测试网关注入
	_ = callctx.With(ctx, callctx.Meta{TenantID: "ok"})
	return callctx.With(ctx, callctx.Meta{TenantID: "forged"}) // want `禁止在 HTTP 层直接调用 callctx\.With 覆写身份上下文`
}
