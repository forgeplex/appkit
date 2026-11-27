package pgxpool

import "context"

type Pool struct{}

func (*Pool) Ping(ctx context.Context) error                     { return nil }
func (*Pool) Exec(ctx context.Context, sql string) (any, error)  { return nil, nil }
func (*Pool) Query(ctx context.Context, sql string) (any, error) { return nil, nil }
func (*Pool) QueryRow(ctx context.Context, sql string) any       { return nil }
func (*Pool) SendBatch(ctx context.Context, b any) any           { return nil }
func (*Pool) CopyFrom(ctx context.Context, t any) (int64, error) { return 0, nil }
func (*Pool) Begin(ctx context.Context) (any, error)             { return nil, nil }
