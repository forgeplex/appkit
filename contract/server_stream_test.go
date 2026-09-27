package contract_test

import (
	"context"
	"testing"
	"time"

	"github.com/forgeplex/appkit/contract"
	"github.com/forgeplex/appkit/contract/streamtest"
)

func TestLocalServerStreamConformance(t *testing.T) {
	streamtest.VerifyServer(t, func(ctx context.Context, cfg contract.StreamConfig,
		produce func(context.Context, func(context.Context, string) error) error,
	) (contract.ServerStream[string], error) {
		return contract.OpenLocal[struct{}, string](ctx, "test", "Watch", cfg,
			func(ctx context.Context, peer contract.Stream[string, struct{}]) error {
				return produce(ctx, peer.Send)
			})
	}, contract.StreamConfig{MaxDuration: 3 * time.Second, CloseTimeout: time.Second, QueueSize: 2},
		[]string{"first", "second", "third"})
}
