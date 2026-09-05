package main

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/refs"
)

func TestPSPOrderReferencesAndJSONRoundTrip(t *testing.T) {
	schema, err := pspSchema()
	if err != nil {
		t.Fatal(err)
	}
	order, err := preparePSPOrder(context.Background(), schema, fakeAuthorizedScope{tenantID: demoTenant}, []byte(pspJSON))
	if err != nil {
		t.Fatal(err)
	}
	if order.Refs.Len() != 4 || order.TenantID != demoTenant {
		t.Fatalf("unexpected order: %+v", order)
	}
	wire, err := json.Marshal(order)
	if err != nil {
		t.Fatal(err)
	}
	var got Order
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(got.Refs); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, got) {
		t.Fatalf("roundtrip changed order: got %+v, want %+v", got, order)
	}
}

func TestSameOrderTypeDoesNotRequireMerchant(t *testing.T) {
	schema, err := storeSchema()
	if err != nil {
		t.Fatal(err)
	}
	values, err := schema.DecodeJSON([]byte(`{"store_id":"store-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	order := Order{ID: "order-store-1", TenantID: demoTenant, Refs: values}
	if order.Refs.Len() != 1 {
		t.Fatal("store order should need only its store reference")
	}
	if _, ok := order.Refs.Get("merchant_id"); ok {
		t.Fatal("store profile unexpectedly needs a merchant")
	}
	if _, err := schema.DecodeJSON([]byte(pspJSON)); err == nil {
		t.Fatal("one profile must not silently accept another profile's references")
	}
}

func TestSchemaValidationDoesNotGrantOwnership(t *testing.T) {
	schema, err := pspSchema()
	if err != nil {
		t.Fatal(err)
	}
	// Syntactically valid UUIDs do not prove account -> merchant membership.
	wrongMerchant := strings.Replace(pspJSON, merchantID, otherID, 1)
	if _, err := schema.DecodeJSON([]byte(wrongMerchant)); err != nil {
		t.Fatalf("shape is valid: %v", err)
	}
	if _, err := preparePSPOrder(context.Background(), schema, fakeAuthorizedScope{tenantID: demoTenant}, []byte(wrongMerchant)); !apperr.Is(err, apperr.CodePermissionDenied) {
		t.Fatalf("ownership mismatch must fail: %v", err)
	}
	if _, err := preparePSPOrder(context.Background(), schema, fakeAuthorizedScope{tenantID: "another-tenant"}, []byte(pspJSON)); !apperr.Is(err, apperr.CodePermissionDenied) {
		t.Fatalf("cross-tenant scope must fail: %v", err)
	}
	if _, err := preparePSPOrder(context.Background(), schema, fakeAuthorizedScope{}, []byte(pspJSON)); !apperr.Is(err, apperr.CodePermissionDenied) {
		t.Fatalf("missing authorized scope must fail: %v", err)
	}
	wrongChannel := strings.Replace(pspJSON, channelID, otherID, 1)
	if _, err := preparePSPOrder(context.Background(), schema, fakeAuthorizedScope{tenantID: demoTenant}, []byte(wrongChannel)); !apperr.Is(err, apperr.CodePermissionDenied) {
		t.Fatalf("channel/group mismatch must fail: %v", err)
	}
	// Related optional references need a business rule as well as schema rules.
	partialRoute := strings.TrimSuffix(unroutedJSON, "}") + `,"channel_id":"` + channelID + `"}`
	if _, err := schema.DecodeJSON([]byte(partialRoute)); err != nil {
		t.Fatalf("individually optional keys have a valid shape: %v", err)
	}
	if _, err := preparePSPOrder(context.Background(), schema, fakeAuthorizedScope{tenantID: demoTenant}, []byte(partialRoute)); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("incomplete route pair must fail the business rule: %v", err)
	}
}

func TestFullValuesAndPartialFiltersHaveDifferentRules(t *testing.T) {
	schema, err := pspSchema()
	if err != nil {
		t.Fatal(err)
	}
	filter, err := refs.DecodeJSON([]byte(`{"merchant_id":"` + merchantID + `","channel_id":"` + channelID + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.ValidateFilter(filter); err != nil {
		t.Fatalf("filters do not need all required resource keys: %v", err)
	}
	if err := schema.Validate(filter); err == nil {
		t.Fatal("an order still requires its merchant account")
	}
	if err := schema.ValidateFilter(refs.Values{}); err != nil {
		t.Fatalf("empty filter is structurally allowed: %v", err)
	}
	for _, raw := range []string{
		`{"unknown_id":"` + otherID + `"}`,
		`{"merchant_id":"not-a-uuid"}`,
	} {
		values, err := refs.DecodeJSON([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.ValidateFilter(values); err == nil {
			t.Fatalf("invalid filter accepted: %s", raw)
		}
	}
}

func TestRoutingReferencesAreAssignedOnce(t *testing.T) {
	schema, err := pspSchema()
	if err != nil {
		t.Fatal(err)
	}
	before, err := schema.DecodeJSON([]byte(unroutedJSON))
	if err != nil {
		t.Fatal(err)
	}
	after, err := schema.DecodeJSON([]byte(pspJSON))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.ValidateUpdate(before, after); err != nil {
		t.Fatalf("first optional reference assignment should work: %v", err)
	}
	if err := schema.ValidateUpdate(after, after); err != nil {
		t.Fatalf("unchanged snapshot should work: %v", err)
	}
	changed := after.Map()
	changed["channel_id"] = otherID
	values, err := refs.NewValues(changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.ValidateUpdate(after, values); err == nil {
		t.Fatal("changing an assigned immutable reference must fail")
	}
	if err := schema.ValidateUpdate(after, before); err == nil {
		t.Fatal("removing an assigned immutable reference must fail")
	}
	if err := schema.ValidateUpdate(refs.Values{}, after); err == nil {
		t.Fatal("an invalid previous full snapshot must not bypass validation")
	}
}

func TestStrictInputRejectsAmbiguousReferences(t *testing.T) {
	schema, err := pspSchema()
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`null`,
		`{"merchant_id":null}`,
		`{"merchant_id":{"id":"nested"}}`,
		`{"merchant_id":["many"]}`,
		`{"merchant_id":"` + merchantID + `","merchant_id":"` + otherID + `"}`,
		pspJSON + ` {}`,
	} {
		if _, err := schema.DecodeJSON([]byte(raw)); err == nil {
			t.Fatalf("ambiguous input accepted: %s", raw)
		}
	}
}

func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(&out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"merchant_account_id":"` + accountID + `"`,
		`"store_id":"store-1"`,
		"OK:", "REJECTED:", "DEMO ONLY:",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing output %q in %s", want, out.String())
		}
	}
}
