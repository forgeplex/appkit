package refs_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/refs"
)

const merchantUUID = "00000000-0000-4000-8000-000000000001"

func schemaSpec() refs.Spec {
	return refs.Spec{Domain: "order", Resource: "payment", Version: 1, Definitions: []refs.Definition{
		{Key: "merchant_id", Target: "merchant.merchant", Format: refs.FormatUUID, Required: true, Immutable: true},
		{Key: "channel_id", Target: "channel.channel", Format: refs.FormatOpaque, Immutable: true},
		{Key: "label_owner", Target: "labels.owner", Format: refs.FormatOpaque},
	}}
}

func mustSchema(t *testing.T, spec refs.Spec) *refs.Schema {
	t.Helper()
	s, err := refs.NewSchema(spec)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustValues(t *testing.T, entries map[string]string) refs.Values {
	t.Helper()
	v, err := refs.NewValues(entries)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func wantCode(t *testing.T, err error, code, key string) {
	t.Helper()
	if code == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if !apperr.Is(err, code) {
		t.Fatalf("error = %v, want %s", err, code)
	}
	e := apperr.From(err)
	status := 422
	if code == refs.CodeInvalidSchema {
		status = 500
	} else if code == refs.CodeImmutableReference {
		status = 409
	}
	if e.Status() != status {
		t.Fatalf("status = %d, want %d", e.Status(), status)
	}
	if key != "" && e.Details()["key"] != key {
		t.Fatalf("details = %v, want key %q", e.Details(), key)
	}
}

func TestNewSchemaValidation(t *testing.T) {
	tests := []struct {
		name string
		edit func(*refs.Spec)
	}{
		{"empty domain", func(s *refs.Spec) { s.Domain = "" }},
		{"uppercase domain", func(s *refs.Spec) { s.Domain = "Order" }},
		{"namespaced resource", func(s *refs.Spec) { s.Resource = "payment.order" }},
		{"hyphen resource", func(s *refs.Spec) { s.Resource = "payment-order" }},
		{"nonascii resource", func(s *refs.Spec) { s.Resource = "订单" }},
		{"long resource", func(s *refs.Spec) { s.Resource = strings.Repeat("a", refs.MaxKeyBytes+1) }},
		{"zero version", func(s *refs.Spec) { s.Version = 0 }},
		{"duplicate key", func(s *refs.Spec) { s.Definitions = append(s.Definitions, s.Definitions[0]) }},
		{"bad key", func(s *refs.Spec) { s.Definitions[0].Key = "Merchant ID" }},
		{"empty key", func(s *refs.Spec) { s.Definitions[0].Key = "" }},
		{"empty target", func(s *refs.Spec) { s.Definitions[0].Target = "" }},
		{"unqualified target", func(s *refs.Spec) { s.Definitions[0].Target = "merchant" }},
		{"bad target segment", func(s *refs.Spec) { s.Definitions[0].Target = "merchant..account" }},
		{"uppercase target", func(s *refs.Spec) { s.Definitions[0].Target = "merchant.Account" }},
		{"long target", func(s *refs.Spec) { s.Definitions[0].Target = strings.Repeat("a.", 65) + "b" }},
		{"long target segment", func(s *refs.Spec) { s.Definitions[0].Target = "a." + strings.Repeat("b", refs.MaxKeyBytes+1) }},
		{"missing format", func(s *refs.Spec) { s.Definitions[0].Format = "" }},
		{"unknown format", func(s *refs.Spec) { s.Definitions[0].Format = "uuidv4" }},
		{"too many definitions", func(s *refs.Spec) {
			s.Definitions = nil
			for i := range refs.MaxEntries + 1 {
				s.Definitions = append(s.Definitions, refs.Definition{Key: fmt.Sprintf("ref_%d", i), Target: "target.resource", Format: refs.FormatOpaque})
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := schemaSpec()
			tt.edit(&spec)
			s, err := refs.NewSchema(spec)
			if s != nil {
				t.Fatal("invalid configuration returned a schema")
			}
			wantCode(t, err, refs.CodeInvalidSchema, "")
		})
	}
	t.Run("bounds accepted", func(t *testing.T) {
		spec := schemaSpec()
		spec.Domain = strings.Repeat("d", refs.MaxKeyBytes)
		spec.Resource = strings.Repeat("r", refs.MaxKeyBytes)
		spec.Definitions = nil
		for i := range refs.MaxEntries {
			spec.Definitions = append(spec.Definitions, refs.Definition{Key: fmt.Sprintf("refs.key_%d", i), Target: strings.Repeat("a", 64) + "." + strings.Repeat("b", 63), Format: refs.FormatOpaque})
		}
		mustSchema(t, spec)
	})
}

func TestSchemaDetachedVersionedDescriptor(t *testing.T) {
	spec := schemaSpec()
	s := mustSchema(t, spec)
	if spec.Definitions[0].Key != "merchant_id" {
		t.Fatal("constructor mutated input order")
	}
	spec.Definitions[0].Required = false
	spec.Definitions[0].Key = "changed"
	copySpec := s.Spec()
	if copySpec.Domain != "order" || copySpec.Resource != "payment" || copySpec.Version != 1 || copySpec.Definitions[0].Key != "channel_id" {
		t.Fatalf("unexpected descriptor: %+v", copySpec)
	}
	copySpec.Definitions[0].Immutable = false
	copySpec.Definitions[2].Required = false
	if !s.Spec().Definitions[0].Immutable {
		t.Fatal("descriptor aliases schema")
	}
	wantCode(t, s.Validate(refs.Values{}), refs.CodeRequiredReference, "merchant_id")
	wire, err := json.Marshal(s.Spec())
	if err != nil {
		t.Fatal(err)
	}
	var decoded refs.Spec
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Spec(), mustSchema(t, decoded).Spec()) {
		t.Fatal("descriptor roundtrip changed the contract")
	}
}

func TestSchemaValidateAndFilter(t *testing.T) {
	s := mustSchema(t, schemaSpec())
	tests := []struct {
		name    string
		entries map[string]string
		full    string
		filter  string
		key     string
	}{
		{"empty", nil, refs.CodeRequiredReference, "", "merchant_id"},
		{"valid", map[string]string{"merchant_id": merchantUUID}, "", "", ""},
		{"partial", map[string]string{"channel_id": "ch_1"}, refs.CodeRequiredReference, "", "merchant_id"},
		{"unknown", map[string]string{"secret_role": "sensitive-id"}, refs.CodeUnknownReference, refs.CodeUnknownReference, "secret_role"},
		{"wrong format", map[string]string{"merchant_id": "private-id"}, refs.CodeInvalidID, refs.CodeInvalidID, "merchant_id"},
		{"nil uuid", map[string]string{"merchant_id": "00000000-0000-0000-0000-000000000000"}, refs.CodeInvalidID, refs.CodeInvalidID, "merchant_id"},
		{"uppercase uuid", map[string]string{"merchant_id": "aaaaaaaa-AAAA-4000-8000-aaaaaaaaaaaa"}, refs.CodeInvalidID, refs.CodeInvalidID, "merchant_id"},
		{"compact uuid", map[string]string{"merchant_id": "00000000000040008000000000000001"}, refs.CodeInvalidID, refs.CodeInvalidID, "merchant_id"},
		{"uuid hyphen", map[string]string{"merchant_id": "00000000_0000-4000-8000-000000000001"}, refs.CodeInvalidID, refs.CodeInvalidID, "merchant_id"},
		{"uuid nonhex", map[string]string{"merchant_id": "00000000-0000-4000-8000-00000000000g"}, refs.CodeInvalidID, refs.CodeInvalidID, "merchant_id"},
		{"no version restrictions", map[string]string{"merchant_id": "ffffffff-ffff-ffff-ffff-ffffffffffff"}, "", "", ""},
		{"unicode opaque", map[string]string{"merchant_id": merchantUUID, "channel_id": "渠道:一"}, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := mustValues(t, tt.entries)
			wantCode(t, s.Validate(values), tt.full, tt.key)
			wantCode(t, s.ValidateFilter(values), tt.filter, tt.key)
		})
	}
}

func TestSchemaEmptyAndUnconfigured(t *testing.T) {
	empty := mustSchema(t, refs.Spec{Domain: "order", Resource: "basic", Version: 1})
	wantCode(t, empty.Validate(refs.Values{}), "", "")
	wantCode(t, empty.Validate(mustValues(t, map[string]string{"merchant_id": merchantUUID})), refs.CodeUnknownReference, "merchant_id")
	for _, s := range []*refs.Schema{nil, new(refs.Schema)} {
		if !reflect.DeepEqual(s.Spec(), refs.Spec{}) {
			t.Fatal("unconfigured descriptor should be zero")
		}
		wantCode(t, s.Validate(refs.Values{}), refs.CodeInvalidSchema, "")
		wantCode(t, s.ValidateFilter(refs.Values{}), refs.CodeInvalidSchema, "")
		wantCode(t, s.ValidateUpdate(refs.Values{}, refs.Values{}), refs.CodeInvalidSchema, "")
		v, err := s.DecodeJSON([]byte(`{}`))
		wantCode(t, err, refs.CodeInvalidSchema, "")
		if v.Len() != 0 {
			t.Fatal("failed decode returned values")
		}
	}
}

func TestSchemaDecodeJSON(t *testing.T) {
	s := mustSchema(t, schemaSpec())
	for _, tc := range []struct{ data, code string }{
		{`{"merchant_id":"` + merchantUUID + `"}`, ""},
		{`{}`, refs.CodeRequiredReference},
		{`null`, refs.CodeInvalidValues},
		{`{"merchant_id":"bad"}`, refs.CodeInvalidID},
		{`{"merchant_id":"` + merchantUUID + `","merchant_id":"` + merchantUUID + `"}`, refs.CodeInvalidValues},
		{`{"unknown":"id"}`, refs.CodeUnknownReference},
	} {
		v, err := s.DecodeJSON([]byte(tc.data))
		wantCode(t, err, tc.code, "")
		if err != nil && v.Len() != 0 {
			t.Fatal("failed decode returned partially validated values")
		}
	}
}

func TestSchemaValidateUpdate(t *testing.T) {
	s := mustSchema(t, schemaSpec())
	base := map[string]string{"merchant_id": merchantUUID}
	withChannel := map[string]string{"merchant_id": merchantUUID, "channel_id": "ch_1"}
	for _, tt := range []struct {
		name          string
		before, after map[string]string
		code, key     string
	}{
		{"unchanged", withChannel, withChannel, "", ""},
		{"late bind", base, withChannel, "", ""},
		{"remove immutable", withChannel, base, refs.CodeImmutableReference, "channel_id"},
		{"change immutable", withChannel, map[string]string{"merchant_id": merchantUUID, "channel_id": "ch_2"}, refs.CodeImmutableReference, "channel_id"},
		{"change required immutable", base, map[string]string{"merchant_id": "00000000-0000-4000-8000-000000000002"}, refs.CodeImmutableReference, "merchant_id"},
		{"mutable change", map[string]string{"merchant_id": merchantUUID, "label_owner": "a"}, map[string]string{"merchant_id": merchantUUID, "label_owner": "b"}, "", ""},
		{"mutable remove", map[string]string{"merchant_id": merchantUUID, "label_owner": "a"}, base, "", ""},
		{"invalid before", nil, base, refs.CodeRequiredReference, "merchant_id"},
		{"invalid after", base, nil, refs.CodeRequiredReference, "merchant_id"},
		{"unknown before", map[string]string{"merchant_id": merchantUUID, "old_ref": "id"}, base, refs.CodeUnknownReference, "old_ref"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wantCode(t, s.ValidateUpdate(mustValues(t, tt.before), mustValues(t, tt.after)), tt.code, tt.key)
		})
	}
}

func TestSchemaDeterministicErrorsAndNoIDDisclosure(t *testing.T) {
	s := mustSchema(t, schemaSpec())
	v := mustValues(t, map[string]string{"zzz": "private-secret-id", "aaa": "private-secret-id"})
	for range 100 {
		err := s.Validate(v)
		wantCode(t, err, refs.CodeUnknownReference, "aaa")
		if strings.Contains(fmt.Sprintf("%v %+v", err, apperr.From(err).Details()), "private-secret-id") {
			t.Fatal("error disclosed an ID")
		}
	}
	spec := schemaSpec()
	spec.Definitions[0].Required = true
	spec.Definitions[1].Required = true
	s = mustSchema(t, spec)
	for range 100 {
		wantCode(t, s.Validate(refs.Values{}), refs.CodeRequiredReference, "channel_id")
	}
}

func TestSchemaConcurrentSnapshots(t *testing.T) {
	s := mustSchema(t, schemaSpec())
	v := mustValues(t, map[string]string{"merchant_id": merchantUUID, "channel_id": "ch_1"})
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 100 {
				if err := s.Validate(v); err != nil {
					t.Error(err)
				}
				if err := s.ValidateUpdate(v, v); err != nil {
					t.Error(err)
				}
				s.Spec().Definitions[0].Key = "detached"
				v.Map()["merchant_id"] = "detached"
			}
		})
	}
	wg.Wait()
}
