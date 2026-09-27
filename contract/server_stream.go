package contract

import "context"

// ServerStream receives a server-to-client stream after its single request has
// been sent. It deliberately exposes neither Send nor CloseSend. Recv contexts
// affect only that operation; Close cancels the whole stream and is idempotent.
// As with ClientStream, overlapping Recv calls are rejected, accepted messages
// drain before the terminal error, and Close has the configured close budget.
// Streaming APIs remain experimental until explicitly promoted under ADR-0047.
type ServerStream[T any] interface {
	Recv(context.Context) (T, error)
	Close() error
}
