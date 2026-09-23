package contract_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/forgeplex/appkit/contract"
)

type exampleStreamRequest struct{ Text string }
type exampleStreamReply struct{ Text string }

func ExampleOpenLocal() {
	stream, err := contract.OpenLocal(context.Background(), "greeter", "GreetStream",
		contract.StreamConfig{
			MaxDuration:  time.Minute,
			CloseTimeout: time.Second,
			QueueSize:    1,
		},
		func(ctx context.Context, peer contract.Stream[exampleStreamReply, exampleStreamRequest]) error {
			request, err := peer.Recv(ctx)
			if err != nil {
				return err
			}
			return peer.Send(ctx, exampleStreamReply{Text: request.Text})
		},
	)
	if err != nil {
		panic(err)
	}
	if err := stream.Send(context.Background(), exampleStreamRequest{Text: "hello"}); err != nil {
		panic(err)
	}
	if err := stream.CloseSend(context.Background()); err != nil {
		panic(err)
	}
	reply, err := stream.Recv(context.Background())
	if err != nil {
		panic(err)
	}
	fmt.Println(reply.Text)
	_, err = stream.Recv(context.Background())
	fmt.Println(errors.Is(err, io.EOF))
	if err := stream.Close(); err != nil {
		panic(err)
	}
	// Output:
	// hello
	// true
}
