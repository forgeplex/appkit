package httpserver_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/contract"
	"github.com/forgeplex/appkit/contract/streamtest"
	"github.com/forgeplex/appkit/httpserver"
)

// The common suite compares receive-only behavior; the SSE adapter never gains
// a Send or CloseSend method. Cursor/parser assertions live in the SSE tests.
type sseDataStream struct {
	contract.ServerStream[httpserver.SSEEvent[string]]
}

func (s sseDataStream) Recv(ctx context.Context) (string, error) {
	event, err := s.ServerStream.Recv(ctx)
	return event.Data, err
}

func TestSSEServerStreamConformance(t *testing.T) {
	streamtest.VerifyServer(t, func(ctx context.Context, cfg contract.StreamConfig,
		produce func(context.Context, func(context.Context, string) error) error,
	) (contract.ServerStream[string], error) {
		handler, err := httpserver.NewSSEHandler[struct{}, string](httpserver.SSEConfig{
			System: "conformance", Method: "Watch", Stream: cfg,
			MaxRequestBodyBytes: 1024, WriteTimeout: time.Second,
			HeartbeatInterval: 5 * time.Millisecond,
		}, func(ctx context.Context, _ string, peer contract.Stream[httpserver.SSEEvent[string], struct{}]) error {
			if _, err := peer.Recv(ctx); err != nil {
				return err
			}
			return produce(ctx, func(ctx context.Context, value string) error {
				return peer.Send(ctx, httpserver.SSEEvent[string]{Data: value})
			})
		})
		if err != nil {
			return nil, err
		}
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(contract.HeaderServiceAuthorization) != "Bearer test-credential" {
				apperr.WriteProblem(w, apperr.Unauthenticated("service credential required"))
				return
			}
			handler.ServeHTTP(w, r)
		}))
		t.Cleanup(server.Close)
		stream, err := httpserver.DialSecureSSE[struct{}, string](ctx, server.URL,
			httpserver.SSEClientConfig{System: "conformance", Method: "Watch", Stream: cfg,
				MaxRequestBodyBytes: 1024, MaxEventBytes: 1024},
			contract.SecureClientOptions{Audience: "sse-conformance", HTTPClient: server.Client(),
				Credentials: contract.ServiceCredentialProviderFunc(func(context.Context, contract.ServiceScope) (contract.ServiceCredential, error) {
					return contract.ServiceCredential{Token: "test-credential", ExpiresAt: time.Now().Add(time.Hour)}, nil
				})}, struct{}{})
		if err != nil {
			return nil, err
		}
		return sseDataStream{stream}, nil
	}, contract.StreamConfig{MaxDuration: 3 * time.Second, CloseTimeout: time.Second, QueueSize: 2},
		[]string{"first", "second", "third"})
}

func TestWebSocketServerStreamConformance(t *testing.T) {
	streamtest.VerifyServer(t, func(ctx context.Context, cfg contract.StreamConfig,
		produce func(context.Context, func(context.Context, string) error) error,
	) (contract.ServerStream[string], error) {
		return openSecureWebSocketStream(t, ctx, cfg,
			func(ctx context.Context, peer contract.Stream[string, string]) error {
				return produce(ctx, peer.Send)
			})
	}, contract.StreamConfig{MaxDuration: 3 * time.Second, CloseTimeout: time.Second, QueueSize: 2},
		[]string{"first", "second", "third"})
}
