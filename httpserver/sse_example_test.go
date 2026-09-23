package httpserver_test

import (
	"context"
	"time"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/contract"
	"github.com/forgeplex/appkit/httpserver"
)

func ExampleNewSSEHandler() {
	type Request struct{ Prompt string }
	type Event struct{ Text string }

	streamHandler, err := httpserver.NewSSEHandler[Request, Event](
		httpserver.SSEConfig{
			System: "assistant",
			Method: "Generate",
			Stream: contract.StreamConfig{
				MaxDuration:  2 * time.Minute,
				IdleTimeout:  30 * time.Second,
				CloseTimeout: 3 * time.Second,
				QueueSize:    16,
			},
			MaxRequestBodyBytes: 1 << 20,
			WriteTimeout:        10 * time.Second,
			HeartbeatInterval:   15 * time.Second,
		},
		func(ctx context.Context, _ string, peer contract.Stream[httpserver.SSEEvent[Event], Request]) error {
			request, err := peer.Recv(ctx)
			if err != nil {
				return err
			}
			return peer.Send(ctx, httpserver.SSEEvent[Event]{
				ID:   "application-cursor-1",
				Data: Event{Text: request.Prompt},
			})
		},
	)
	if err != nil {
		panic(err)
	}

	module := appkit.ModuleFunc("assistant", func(reg *appkit.Registry) error {
		reg.MountAuthenticated("/events", streamHandler)
		return nil
	})
	// The app explicitly selects a security profile. Its normal Run/Start owner
	// controls the listener lifecycle; the SSE route stays behind that root.
	_ = appkit.New([]appkit.Module{module}, appkit.Security(appkit.SecurityUserFacing))
}
