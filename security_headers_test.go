package appkit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/forgeplex/appkit/callctx"
	"github.com/forgeplex/appkit/contract"
)

func TestUntrustedIdentitySnapshotIsIsolatedAndNotPropagated(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header["x-tenant-id"] = []string{"tenant-a", "tenant-b"}
	req.Header.Set(callctx.HeaderPartition, "partition-a")
	req.Header.Set(callctx.HeaderCaller, "gateway")
	req.Header.Set("X-Merchant-Id", "merchant-a")
	req.Header.Set("Authorization", "Bearer secret-not-in-snapshot")
	want := http.Header{
		"X-Tenant-Id":   {"tenant-a", "tenant-b"},
		"X-Partition":   {"partition-a"},
		"X-Caller":      {"gateway"},
		"X-Merchant-Id": {"merchant-a"},
	}
	identityBoundary(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := UntrustedIdentityHeadersFrom(r.Context())
		if !reflect.DeepEqual(h, want) {
			t.Fatalf("snapshot = %v, want %v", h, want)
		}
		for name := range want {
			if r.Header.Get(name) != "" {
				t.Fatalf("unsigned header remains: %s", name)
			}
		}
		if _, exists := r.Header["x-tenant-id"]; exists {
			t.Fatal("non-canonical identity header survived")
		}
		h["X-Tenant-Id"][0] = "mutated"
		h.Set("X-Caller", "mutated")
		if !reflect.DeepEqual(UntrustedIdentityHeadersFrom(r.Context()), want) {
			t.Fatal("snapshot accessor leaked mutable state")
		}
		if got := UntrustedIdentityHeadersFrom(contract.Firewall(r.Context())); got != nil {
			t.Fatalf("untrusted identity crossed contract firewall: %v", got)
		}
		if got := callctx.From(r.Context()); got != (callctx.Meta{}) {
			t.Fatalf("untrusted snapshot granted trusted metadata: %+v", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), req)
	if req.Header["x-tenant-id"][0] != "tenant-a" || req.Header.Get("Authorization") == "" {
		t.Fatal("boundary mutated incoming request")
	}
	if UntrustedIdentityHeadersFrom(context.Background()) != nil {
		t.Fatal("missing snapshot must be nil")
	}
}
