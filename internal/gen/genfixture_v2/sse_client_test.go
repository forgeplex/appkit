package fixturev2

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/contract"
	"github.com/forgeplex/appkit/httpserver"
	"github.com/forgeplex/appkit/tx"
)

func TestV2GeneratedSecureSSE(t *testing.T) {
	serverConfig := httpserver.SSEConfig{
		Stream: fixtureConfig(), MaxRequestBodyBytes: 4096, WriteTimeout: time.Second,
	}
	watch, err := NewWatchSSEHandlerV2(serverConfig, fixtureService{})
	if err != nil {
		t.Fatal(err)
	}
	tail, err := NewTailSSEHandlerV2(serverConfig, fixtureService{})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v2/watch", watch)
	mux.Handle("/v2/tail", tail)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get(contract.HeaderServiceAuthorization) != "Bearer fixture-sse-service" {
			apperr.WriteProblem(w, apperr.Unauthenticated("service credential required"))
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	var credentials atomic.Int32
	secure := contract.SecureClientOptions{
		Audience: "greet", HTTPClient: server.Client(),
		Credentials: contract.ServiceCredentialProviderFunc(func(_ context.Context, scope contract.ServiceScope) (contract.ServiceCredential, error) {
			credentials.Add(1)
			if scope.Audience != "greet" {
				return contract.ServiceCredential{}, apperr.PermissionDenied("unexpected audience")
			}
			return contract.ServiceCredential{Token: "fixture-sse-service", ExpiresAt: time.Now().Add(time.Minute)}, nil
		}),
	}
	clientConfig := httpserver.SSEClientConfig{
		Stream: fixtureConfig(), MaxRequestBodyBytes: 4096, MaxEventBytes: 4096,
		LastEventID: "opaque-cursor",
	}

	t.Run("request and cursor", func(t *testing.T) {
		reader, err := DialWatchSSEV2(context.Background(), server.URL+"/v2/watch", clientConfig, secure, WatchRequestV2{Topic: "secure-sse"})
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		var receiveOnly contract.ServerStream[httpserver.SSEEvent[WatchResponseV2]] = reader
		if _, bidi := any(receiveOnly).(contract.ClientStream[WatchRequestV2, httpserver.SSEEvent[WatchResponseV2]]); bidi {
			t.Fatal("SSE client exposes bidirectional streaming capability")
		}
		event, err := reader.Recv(context.Background())
		if err != nil || event.ID != "event-1" || event.Data.EventID != "event-1" || event.Data.Event != "update" || event.Data.Meta.Source != "secure-sse" {
			t.Fatalf("Recv = %+v, %v", event, err)
		}
		if _, err := reader.Recv(context.Background()); !errors.Is(err, io.EOF) {
			t.Fatalf("terminal = %v, want EOF", err)
		}
	})
	t.Run("no request and cursor", func(t *testing.T) {
		cfg := clientConfig
		cfg.LastEventID = "tail-cursor"
		reader, err := DialTailSSEV2(context.Background(), server.URL+"/v2/tail", cfg, secure)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		event, err := reader.Recv(context.Background())
		if err != nil || event.ID != "next" || event.Data.Sequence != "next" {
			t.Fatalf("Recv = %+v, %v", event, err)
		}
		if _, err := reader.Recv(context.Background()); !errors.Is(err, io.EOF) {
			t.Fatalf("terminal = %v, want EOF", err)
		}
	})
	t.Run("application failure", func(t *testing.T) {
		cfg := clientConfig
		cfg.LastEventID = "invalid-cursor"
		_, err := DialWatchSSEV2(context.Background(), server.URL+"/v2/watch", cfg, secure, WatchRequestV2{Topic: "secure-sse"})
		if !apperr.Is(err, apperr.CodeInternal) {
			t.Fatalf("application error = %v, want INTERNAL", err)
		}
	})
	t.Run("transaction guard", func(t *testing.T) {
		before := requests.Load()
		_, err := DialWatchSSEV2(tx.With(context.Background(), struct{}{}), server.URL+"/v2/watch", clientConfig, secure, WatchRequestV2{Topic: "blocked"})
		if !apperr.Is(err, apperr.CodeTxBoundary) {
			t.Fatalf("guard = %v", err)
		}
		if requests.Load() != before {
			t.Fatal("transaction-bound SSE dial made an HTTP request")
		}
	})
	if requests.Load() != 3 || credentials.Load() != 3 {
		t.Fatalf("requests/credentials = %d/%d, want three initial calls without retries", requests.Load(), credentials.Load())
	}
}
