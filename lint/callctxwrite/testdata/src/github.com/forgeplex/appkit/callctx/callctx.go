package callctx

import "context"

type Meta struct{ TenantID string }

func With(ctx context.Context, m Meta) context.Context { return ctx }
func From(ctx context.Context) Meta                    { return Meta{} }
