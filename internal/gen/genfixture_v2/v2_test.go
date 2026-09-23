package fixturev2

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/forgeplex/appkit/contract"
	"github.com/forgeplex/appkit/httpserver"
)

type fixtureService struct{}

func (fixtureService) Ping(_ context.Context, req PingRequestV2) (PingResponseV2, error) {
	return PingResponseV2{Message: "hello " + req.Name}, nil
}

func (fixtureService) Watch(ctx context.Context, cursor string, req WatchRequestV2, sender StreamSenderV2[WatchResponseV2]) error {
	if cursor != "opaque-cursor" && cursor != "local-cursor" {
		return errors.New("cursor was not forwarded")
	}
	return sender.Send(ctx, WatchResponseV2{
		EventID: "event-1",
		Event:   "update",
		Meta:    EventMetaV2{Source: req.Topic},
	})
}

func (fixtureService) Tail(ctx context.Context, cursor string, sender StreamSenderV2[TailResponseV2]) error {
	if cursor != "tail-cursor" && cursor != "local-tail-cursor" {
		return errors.New("tail cursor was not forwarded")
	}
	return sender.Send(ctx, TailResponseV2{Sequence: "next"})
}

func (fixtureService) Chat(ctx context.Context, stream contract.Stream[ChatResponseV2, ChatRequestV2]) error {
	for {
		request, err := stream.Recv(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(ctx, ChatResponseV2{Text: "echo: " + request.Text}); err != nil {
			return err
		}
	}
}

func fixtureConfig() contract.StreamConfig {
	return contract.StreamConfig{
		MaxDuration:  time.Minute,
		IdleTimeout:  time.Minute,
		CloseTimeout: time.Second,
		QueueSize:    1,
	}
}

func TestV2UnaryHTTPClientServer(t *testing.T) {
	service := WrapServiceV2(fixtureService{}, 0)
	server := httptest.NewServer(NewHTTPHandlerV2(service))
	defer server.Close()
	client := NewClientV2(server.URL, "fixture", nil)
	got, err := client.Ping(context.Background(), PingRequestV2{Name: "AppKit"})
	if err != nil || got.Message != "hello AppKit" {
		t.Fatalf("Ping() = %+v, %v", got, err)
	}
}

func TestV2LocalServerStream(t *testing.T) {
	reader, err := OpenWatchLocalV2(context.Background(), "local-cursor", fixtureConfig(), fixtureService{}, WatchRequestV2{Topic: "local"})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	event, err := reader.Recv(context.Background())
	if err != nil || event.Event != "update" || event.Meta.Source != "local" {
		t.Fatalf("Recv() = %+v, %v", event, err)
	}
}

func TestV2LocalServerStreamWithoutRequest(t *testing.T) {
	reader, err := OpenTailLocalV2(context.Background(), "local-tail-cursor", fixtureConfig(), fixtureService{})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	event, err := reader.Recv(context.Background())
	if err != nil || event.Sequence != "next" {
		t.Fatalf("Recv() = %+v, %v", event, err)
	}
}

func TestV2SSEAdapter(t *testing.T) {
	cfg := httpserver.SSEConfig{
		Stream:              fixtureConfig(),
		MaxRequestBodyBytes: 4096,
		WriteTimeout:        time.Second,
	}
	handler, err := NewWatchSSEHandlerV2(cfg, fixtureService{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v2/watch", strings.NewReader(`{"topic":"http"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Last-Event-ID", "opaque-cursor")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), "id: event-1\n") || !strings.Contains(string(body), `"event":"update"`) {
		t.Fatalf("unexpected SSE body: %s", body)
	}
}

func TestV2SSEAdapterWithoutRequest(t *testing.T) {
	cfg := httpserver.SSEConfig{
		Stream:              fixtureConfig(),
		MaxRequestBodyBytes: 4096,
		WriteTimeout:        time.Second,
	}
	handler, err := NewTailSSEHandlerV2(cfg, fixtureService{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v2/tail", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Last-Event-ID", "tail-cursor")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"sequence":"next"`) {
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
}

func TestV2LocalBidiStream(t *testing.T) {
	stream, err := OpenChatLocalV2(context.Background(), fixtureConfig(), fixtureService{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	ctx := context.Background()
	if err := stream.Send(ctx, ChatRequestV2{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := stream.Recv(ctx)
	if err != nil || got.Text != "echo: hello" {
		t.Fatalf("Recv() = %+v, %v", got, err)
	}
	if _, err := stream.Recv(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal Recv() error = %v, want EOF", err)
	}
}
