// Package outboundstate shares request observation state between transport
// instrumentation and contract security policies without exposing public APIs.
package outboundstate

import (
	"context"
	"net/http"
	"sync/atomic"
)

type key struct{}

// State belongs to one HTTP request, including its redirect decision.
type State struct{ RedirectRefused atomic.Bool }

func FromContext(ctx context.Context) *State {
	state, _ := ctx.Value(key{}).(*State)
	return state
}

func WithContext(ctx context.Context, state *State) context.Context {
	return context.WithValue(ctx, key{}, state)
}

// RefuseRedirect marks the response that triggered CheckRedirect. The state is
// on Response.Request because http.Client.Timeout wraps response bodies with a
// private type and redirects inherit the original (unwrapped) request context.
func RefuseRedirect(request *http.Request) {
	if request.Response != nil && request.Response.Request != nil {
		if state := FromContext(request.Response.Request.Context()); state != nil {
			state.RedirectRefused.Store(true)
			return
		}
	}
	if state := FromContext(request.Context()); state != nil {
		state.RedirectRefused.Store(true)
	}
}
