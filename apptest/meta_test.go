package apptest

import (
	"context"
	"net/http"
	"testing"

	"github.com/forgeplex/appkit/callctx"
)

// Lock the legacy function type, not just ordinary calls. Changing Conform to a
// variadic function would silently break assignments in existing consumers.
var _ func(*testing.T, []Binding[echoService], []Case[echoService]) = Conform[echoService]

func TestConformWithDomainValidMetadata(t *testing.T) {
	spy := &metaSpy{inner: localEcho{}}
	srv := newEchoServer(t, spy)
	meta := callctx.Meta{RequestID: "test-request", Partition: "eu", TenantID: "b7777777-1111-4111-8111-111111111111", Caller: "gateway"}
	cases := []Case[echoService]{{Name: "echo", Do: func(ctx context.Context, svc echoService) (any, error) {
		if got := callctx.From(ctx); got != meta {
			t.Errorf("all calls must carry domain-valid metadata: got=%+v want=%+v", got, meta)
		}
		return svc.Echo(ctx, echoReq{Text: "hello"})
	}, Want: echoReply{Echo: "hello"}, Idempotent: true}}
	ConformWithMeta(t, []Binding[echoService]{
		{Name: "local", Service: wrappedEcho{inner: spy}, SeenMeta: spy.Last},
		{Name: "remote", Service: remoteEcho{base: srv.URL, client: &http.Client{Transport: callctx.Transport{Caller: "gateway"}}}, SeenMeta: spy.Last},
	}, cases, meta)
	if got := spy.Last(); got.Partition != "eu" || got.TenantID != meta.TenantID {
		t.Fatalf("custom metadata did not reach implementation: %+v", got)
	}
}

func TestMetaDiffDetectsPartitionLossAndIgnoresHopCaller(t *testing.T) {
	want := callctx.Meta{RequestID: "trace", Partition: "eu", TenantID: "tenant", Caller: "upstream"}
	got := want
	got.Caller = "gateway"
	if msg := metaDiff("remote", want, got); msg != "" {
		t.Fatal(msg)
	}
	got.Partition = ""
	if msg := metaDiff("remote", want, got); msg == "" {
		t.Fatal("partition loss must fail the propagation check")
	}
}
