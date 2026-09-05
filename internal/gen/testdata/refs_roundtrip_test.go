// This is handwritten test input copied next to freshly generated contract
// files by TestContractRefsGeneratedRoundTrip; it is not a generated artifact.
package refsfixture

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/refs"
	"github.com/forgeplex/appkit/tx"
)

type echoService struct{ calls atomic.Int32 }

func (s *echoService) Exchange(_ context.Context, req ExchangeRequest) (ExchangeReply, error) {
	s.calls.Add(1)
	return ExchangeReply{Refs: req.Refs, Batches: req.Batches, Entry: req.Entry, Entries: req.Entries}, nil
}

func (s *echoService) Load(context.Context) (LoadReply, error) {
	s.calls.Add(1)
	return LoadReply{}, nil
}

func (s *echoService) Store(context.Context, StoreRequest) error {
	s.calls.Add(1)
	return nil
}

func (s *echoService) Nested(context.Context, NestedRequest) error {
	s.calls.Add(1)
	return nil
}

func (s *echoService) Ping(context.Context) error {
	s.calls.Add(1)
	return nil
}

func (s *echoService) Legacy(_ context.Context, req LegacyRequest) (LegacyReply, error) {
	s.calls.Add(1)
	return LegacyReply{Scope: req.Scope}, nil
}

func TestReferencesRoundTrip(t *testing.T) {
	values, err := refs.NewValues(map[string]string{"merchant_id": "m1", "merchant_account_id": "a1", "channel_group_id": "g1", "channel_id": "c1"})
	if err != nil {
		t.Fatal(err)
	}
	stub := &echoService{}
	wrapped := WrapService(stub, 0)
	server := httptest.NewServer(NewHTTPHandler(wrapped))
	defer server.Close()
	client := NewClient(server.URL, "test", server.Client())
	req := ExchangeRequest{Refs: values, Batches: [][]refs.Values{{values, {}}}, Entry: Entry{Refs: values, History: []refs.Values{values}}, Entries: []Entry{{Refs: values}}}
	want := ExchangeReply{Refs: req.Refs, Batches: req.Batches, Entry: req.Entry, Entries: req.Entries}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range []Service{wrapped, client} {
		reply, err := binding.Exchange(context.Background(), req)
		replyJSON, encodeErr := json.Marshal(reply)
		if err != nil || encodeErr != nil || string(replyJSON) != string(wantJSON) {
			t.Fatalf("round trip = (%+v, %v), want %+v", reply, err, want)
		}
		if err := binding.Store(context.Background(), StoreRequest{Refs: values}); err != nil {
			t.Fatal(err)
		}
		if err := binding.Nested(context.Background(), NestedRequest{Entries: req.Entries}); err != nil {
			t.Fatal(err)
		}
		loaded, err := binding.Load(context.Background())
		if err != nil || loaded.Refs.Len() != 0 {
			t.Fatalf("empty refs round trip = %+v, %v", loaded, err)
		}
	}
	// Adding refs must not weaken the local transaction boundary.
	before := stub.calls.Load()
	if _, err := wrapped.Exchange(tx.With(context.Background(), "test-tx"), req); !apperr.Is(err, apperr.CodeTxBoundary) || stub.calls.Load() != before {
		t.Fatalf("transaction guard bypassed: %v", err)
	}
	encoded, err := json.Marshal(LoadReply{})
	if err != nil || string(encoded) != `{"refs":{}}` {
		t.Fatalf("zero refs encoded as %s (%v)", encoded, err)
	}
}

func TestReferencesRejectAmbiguousRequests(t *testing.T) {
	stub := &echoService{}
	handler := NewHTTPHandler(WrapService(stub, 0))
	for name, body := range map[string]string{
		"null_request":         `null`,
		"array_request":        `[]`,
		"null_refs":            `{"refs":null}`,
		"null_id":              `{"refs":{"merchant_id":null}}`,
		"numeric_id":           `{"refs":{"merchant_id":42}}`,
		"empty_id":             `{"refs":{"merchant_id":""}}`,
		"invalid_key":          `{"refs":{"MerchantID":"m1"}}`,
		"nested_id":            `{"refs":{"merchant_id":{"id":"m1"}}}`,
		"duplicate_id":         `{"refs":{"merchant_id":"m1","merchant_id":"m2"}}`,
		"escaped_duplicate":    `{"refs":{"merchant_id":"m1","merchant_\u0069d":"m2"}}`,
		"duplicate_field":      `{"refs":{"merchant_id":"m1"},"refs":{"merchant_id":"m2"}}`,
		"duplicate_case_field": `{"refs":{"merchant_id":"m1"},"Refs":{"merchant_id":"m2"}}`,
		"escaped_field":        `{"refs":{},"\u0072efs":{}}`,
		"unicode_case_alias":   `{"links":{},"lin\u212As":{}}`,
		"second_document":      `{"refs":{}} {"refs":{}}`,
		"trailing_garbage":     `{"refs":{}} garbage`,
		"leading_nbsp":         "\u00a0" + `{"refs":{}}`,
		"trailing_nbsp":        `{"refs":{}}` + "\u00a0",
		"leading_line_sep":     "\u2028" + `{"refs":{}}`,
		"trailing_line_sep":    `{"refs":{}}` + "\u2028",
		"surrogate_id":         `{"refs":{"merchant_id":"\ud800"}}`,
		"invalid_utf8":         "{\"refs\":{\"merchant_id\":\"\xff\"}}",
		"nested_dto":           `{"entry":{"refs":null}}`,
		"nested_dto_array":     `{"entries":[{"refs":{"merchant_id":"m1","merchant_id":"m2"}}]}`,
		"nested_array":         `{"batches":[[null]]}`,
		"too_deep":             `{"refs":{},"unknown":` + strings.Repeat("[", maxRefsRequestDepth+1) + `0` + strings.Repeat("]", maxRefsRequestDepth+1) + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			before := stub.calls.Load()
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/exchange", strings.NewReader(body)))
			if res.Code != http.StatusUnprocessableEntity || !strings.Contains(res.Header().Get("Content-Type"), "application/problem+json") {
				t.Fatalf("invalid request returned %d: %s", res.Code, res.Body.String())
			}
			if stub.calls.Load() != before {
				t.Fatal("invalid request reached service")
			}
		})
	}
	// A DTO-only request must select the same strict decoder as a direct refs field.
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/nested", strings.NewReader(`{"entries":[{"refs":{}}],"entries":[]}`)))
	if res.Code != http.StatusUnprocessableEntity {
		t.Fatalf("DTO-only duplicate request = %d", res.Code)
	}
}

func TestReferencesEnvelopeBoundaries(t *testing.T) {
	handler := NewHTTPHandler(WrapService(&echoService{}, 0))
	for _, depth := range []int{maxRefsRequestDepth, maxRefsRequestDepth + 1} {
		// The root object counts as one JSON container level.
		body := `{"refs":{},"unknown":` + strings.Repeat("[", depth-1) + `0` + strings.Repeat("]", depth-1) + `}`
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/exchange", strings.NewReader(body)))
		want := http.StatusOK
		if depth > maxRefsRequestDepth {
			want = http.StatusUnprocessableEntity
		}
		if res.Code != want {
			t.Fatalf("request of %d nesting levels = %d, want %d", depth, res.Code, want)
		}
	}
	for _, size := range []int{maxRefsRequestBytes, maxRefsRequestBytes + 1} {
		prefix, suffix := `{"refs":{},"padding":"`, `"}`
		body := prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/exchange", strings.NewReader(body)))
		want := http.StatusOK
		if size > maxRefsRequestBytes {
			want = http.StatusUnprocessableEntity
		}
		if res.Code != want {
			t.Fatalf("request of %d bytes = %d, want %d", size, res.Code, want)
		}
	}
	for _, body := range []string{`{}`, `{"refs":{}}`, `{"refs":{"unregistered_key":"opaque1"},"future_field":true}`} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/exchange", strings.NewReader(body)))
		if res.Code != http.StatusOK {
			t.Fatalf("valid request rejected: %s: %d", body, res.Code)
		}
	}
	// No resource Schema is injected by codegen. Unknown reference keys are
	// structurally valid here; the domain must separately validate its rules.
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/legacy", strings.NewReader(`{"scope":"first","scope":"second"} {}`)))
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "second") {
		t.Fatalf("non-refs decoding behavior changed: %d %s", res.Code, res.Body.String())
	}
}

func TestReferencesClientRejectsMalformedValue(t *testing.T) {
	for _, body := range []string{`{"refs":null}`, `{"refs":{"merchant_id":"a","merchant_id":"b"}}`, `{"refs":{"merchant_id":42}}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		}))
		client := NewClient(server.URL, "test", server.Client())
		if _, err := client.Load(context.Background()); !apperr.Is(err, apperr.CodeInternal) {
			t.Errorf("malformed refs response = %v", err)
		}
		server.Close()
	}
}
