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

func TestValuesZeroAndDetachedMaps(t *testing.T) {
	var zero refs.Values
	if zero.Len() != 0 || zero.Map() == nil {
		t.Fatal("zero value must be an empty collection with a non-nil copied map")
	}
	if id, ok := zero.Get("merchant_id"); id != "" || ok {
		t.Fatal("zero value unexpectedly contains a reference")
	}
	encoded, err := json.Marshal(zero)
	if err != nil || string(encoded) != "{}" {
		t.Fatalf("zero JSON = %s, %v", encoded, err)
	}
	for _, input := range []map[string]string{nil, {}} {
		values, err := refs.NewValues(input)
		if err != nil || !reflect.DeepEqual(values, zero) {
			t.Fatalf("empty constructor = %#v, %v", values, err)
		}
	}
	for _, input := range []string{`{}`, " \t{ \n}\r"} {
		decoded, err := refs.DecodeJSON([]byte(input))
		if err != nil || !reflect.DeepEqual(decoded, zero) {
			t.Fatalf("empty JSON must restore the canonical zero value: %#v, %v", decoded, err)
		}
		value := mustTestValues(t, map[string]string{"merchant_id": "M1"})
		if err := json.Unmarshal([]byte(input), &value); err != nil || !reflect.DeepEqual(value, zero) {
			t.Fatalf("empty unmarshal must restore the canonical zero value: %#v, %v", value, err)
		}
	}

	input := map[string]string{"merchant_id": "M1"}
	values := mustTestValues(t, input)
	input["merchant_id"] = "M2"
	input["channel_id"] = "C1"
	copy := values.Map()
	copy["merchant_id"] = "M3"
	delete(copy, "merchant_id")
	copy["other"] = "O1"
	if got, ok := values.Get("merchant_id"); !ok || got != "M1" || values.Len() != 1 {
		t.Fatalf("input or returned map mutated Values: %#v", values.Map())
	}
	zero.Map()["merchant_id"] = "M4"
	if zero.Len() != 0 {
		t.Fatal("mutating zero's copied map must not change zero")
	}
}

func TestNewValuesIntrinsicValidation(t *testing.T) {
	tests := []struct {
		name string
		key  string
		id   string
	}{
		{"empty key", "", "M1"},
		{"uppercase key", "Merchant", "M1"},
		{"numeric first key", "1merchant", "M1"},
		{"underscore first key", "_merchant", "M1"},
		{"dash key", "merchant-id", "M1"},
		{"space key", "merchant id", "M1"},
		{"unicode key", "商户", "M1"},
		{"invalid UTF8 key", "a\xff", "M1"},
		{"leading dot", ".merchant", "M1"},
		{"trailing dot", "merchant.", "M1"},
		{"empty segment", "merchant..id", "M1"},
		{"numeric segment", "merchant.1id", "M1"},
		{"underscore segment", "merchant._id", "M1"},
		{"uppercase segment", "merchant.Id", "M1"},
		{"oversized key", strings.Repeat("a", refs.MaxKeyBytes+1), "M1"},
		{"empty ID", "merchant_id", ""},
		{"oversized ID", "merchant_id", strings.Repeat("a", refs.MaxIDBytes+1)},
		{"multibyte oversized ID", "merchant_id", strings.Repeat("商", 86)},
		{"space ID", "merchant_id", "M 1"},
		{"leading space ID", "merchant_id", " M1"},
		{"trailing space ID", "merchant_id", "M1 "},
		{"tab ID", "merchant_id", "M\t1"},
		{"newline ID", "merchant_id", "M\n1"},
		{"null ID", "merchant_id", "M\x001"},
		{"delete ID", "merchant_id", "M\x7f1"},
		{"C1 control ID", "merchant_id", "M\u009f1"},
		{"nonbreaking space ID", "merchant_id", "M\u00a01"},
		{"line separator ID", "merchant_id", "M\u20281"},
		{"ideographic space ID", "merchant_id", "M\u30001"},
		{"invalid UTF8 ID", "merchant_id", "M\xff1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := refs.NewValues(map[string]string{tt.key: tt.id})
			assertValuesError(t, err)
			if got.Len() != 0 {
				t.Fatal("failed constructor returned partial references")
			}
		})
	}

	valid := map[string]string{
		"merchant_id":                         "M1",
		"merchant.account_id":                 "urn:merchant/account:1",
		"namespace_a.b2.c_":                   "渠道一",
		"replacement":                         "\ufffd",
		"composed":                            "é",
		"decomposed":                          "e\u0301",
		"literal_backslash":                   `\ud800`,
		"max_id":                              strings.Repeat("a", refs.MaxIDBytes),
		strings.Repeat("a", refs.MaxKeyBytes): "x",
	}
	got := mustTestValues(t, valid)
	if !reflect.DeepEqual(got.Map(), valid) {
		t.Fatal("constructor normalized or changed accepted values")
	}

	entries := make(map[string]string, refs.MaxEntries+1)
	for i := range refs.MaxEntries {
		entries[fmt.Sprintf("ref_%02d", i)] = "ID"
	}
	if got := mustTestValues(t, entries); got.Len() != refs.MaxEntries {
		t.Fatal("maximum entry count not accepted")
	}
	entries["overflow"] = "ID"
	_, err := refs.NewValues(entries)
	assertValuesError(t, err)
}

func TestDecodeJSONRejectsAmbiguousOrInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{"empty input", ""},
		{"whitespace only", " \r\n\t"},
		{"root null", `null`},
		{"root array", `[]`},
		{"root string", `"M1"`},
		{"root number", `1`},
		{"root true", `true`},
		{"duplicate key", `{"merchant_id":"M1","merchant_id":"M2"}`},
		{"escaped duplicate key", `{"merchant_id":"M1","\u006derchant_id":"M2"}`},
		{"value null", `{"merchant_id":null}`},
		{"value false", `{"merchant_id":false}`},
		{"value true", `{"merchant_id":true}`},
		{"value integer", `{"merchant_id":1}`},
		{"value fraction", `{"merchant_id":1.2}`},
		{"value array", `{"merchant_id":["M1"]}`},
		{"value object", `{"merchant_id":{"id":"M1"}}`},
		{"empty value", `{"merchant_id":""}`},
		{"empty key", `{"":"M1"}`},
		{"invalid key", `{"Merchant":"M1"}`},
		{"escaped invalid key", `{"\u004derchant":"M1"}`},
		{"unquoted key", `{merchant_id:"M1"}`},
		{"missing colon", `{"merchant_id" "M1"}`},
		{"missing comma", `{"merchant_id":"M1" "channel_id":"C1"}`},
		{"extra comma", `{"merchant_id":"M1",,"channel_id":"C1"}`},
		{"trailing comma", `{"merchant_id":"M1",}`},
		{"unterminated object", `{"merchant_id":"M1"`},
		{"unterminated key", `{"merchant_id`},
		{"unterminated value", `{"merchant_id":"M1`},
		{"trailing object", `{} {}`},
		{"trailing null", `{} null`},
		{"trailing junk", `{}x`},
		{"comment", `{}// comment`},
		{"BOM", "\ufeff{}"},
		{"non-JSON whitespace", "\u00a0{}"},
		{"unescaped control", "{\"merchant_id\":\"M\x001\"}"},
		{"escaped whitespace ID", `{"merchant_id":"M\u00201"}`},
		{"escaped newline ID", `{"merchant_id":"M\n1"}`},
		{"escaped control ID", `{"merchant_id":"M\u007f1"}`},
		{"unknown escape", `{"merchant_id":"M\x31"}`},
		{"unfinished escape", `{"merchant_id":"M\`},
		{"short unicode escape", `{"merchant_id":"\u00"}`},
		{"unicode escape at EOF", `{"merchant_id":"\u00`},
		{"invalid unicode escape", `{"merchant_id":"\uZZZZ"}`},
		{"lone high surrogate", `{"merchant_id":"\ud800"}`},
		{"lone low surrogate", `{"merchant_id":"\udc00"}`},
		{"high surrogate at EOF", `{"merchant_id":"\ud800`},
		{"high surrogate short pair", `{"merchant_id":"\ud800\`},
		{"high surrogate before scalar", `{"merchant_id":"\ud800\u0041"}`},
		{"two high surrogates", `{"merchant_id":"\ud800\udbff"}`},
		{"two low surrogates", `{"merchant_id":"\udc00\udfff"}`},
		{"reversed surrogates", `{"merchant_id":"\udc00\ud800"}`},
		{"separated surrogates", `{"merchant_id":"\ud800x\udc00"}`},
		{"surrogate in key", `{"\ud800":"M1"}`},
		{"invalid UTF8 key", "{\"a\xff\":\"M1\"}"},
		{"invalid UTF8 ID", "{\"merchant_id\":\"M\xff1\"}"},
		{"UTF8 encoded surrogate", "{\"merchant_id\":\"\xed\xa0\x80\"}"},
		{"overlong UTF8", "{\"merchant_id\":\"\xc0\xaf\"}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := refs.DecodeJSON([]byte(tt.json))
			assertValuesError(t, err)
			if got.Len() != 0 {
				t.Fatal("failed decoder returned partial references")
			}
		})
	}
}

func TestDecodeJSONValidAndDeterministic(t *testing.T) {
	input := []byte(" \n\t" + `{"z":"Z1", "merchant\u002eaccount_id":"A1","a":"\uD83D\uDE80","literal":"\\ud800"}` + "\r\n")
	got, err := refs.DecodeJSON(input)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"z": "Z1", "merchant.account_id": "A1", "a": "🚀", "literal": `\ud800`}
	if !reflect.DeepEqual(got.Map(), want) {
		t.Fatalf("decoded = %#v, want %#v", got.Map(), want)
	}
	wantJSON := `{"a":"🚀","literal":"\\ud800","merchant.account_id":"A1","z":"Z1"}`
	for range 20 {
		encoded, err := got.MarshalJSON()
		if err != nil || string(encoded) != wantJSON {
			t.Fatalf("marshal = %s, %v; want %s", encoded, err, wantJSON)
		}
		reconstructed, err := refs.DecodeJSON(encoded)
		if err != nil || !reflect.DeepEqual(reconstructed.Map(), want) {
			t.Fatalf("roundtrip failed: %#v, %v", reconstructed.Map(), err)
		}
	}
	for _, input := range []string{`{}`, " \n{ \t}\r", `{"key":"\ud800\udc00"}`, `{"key":"\uDBFF\uDFFF"}`, `{"key":"\ufffd"}`, `{"key":"é"}`, `{"key":"e\u0301"}`, `{"key":"a\/b"}`, `{"key":"a\"b"}`} {
		if _, err := refs.DecodeJSON([]byte(input)); err != nil {
			t.Errorf("valid JSON rejected: %q: %v", input, err)
		}
	}
}

func TestDecodeJSONCapacityLimits(t *testing.T) {
	maxInput := append([]byte("{}"), []byte(strings.Repeat(" ", refs.MaxJSONBytes-2))...)
	if _, err := refs.DecodeJSON(maxInput); err != nil {
		t.Fatalf("maximum JSON bytes rejected: %v", err)
	}
	_, err := refs.DecodeJSON(append(maxInput, ' '))
	assertValuesError(t, err)

	entries := make(map[string]string, refs.MaxEntries+1)
	for i := range refs.MaxEntries {
		entries[fmt.Sprintf("ref_%02d", i)] = strings.Repeat("x", refs.MaxIDBytes)
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := refs.DecodeJSON(encoded); err != nil || got.Len() != refs.MaxEntries {
		t.Fatalf("maximum entries and IDs rejected: %v", err)
	}
	entries["overflow"] = "ID"
	encoded, err = json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	_, err = refs.DecodeJSON(encoded)
	assertValuesError(t, err)
	for _, input := range []string{
		`{"` + strings.Repeat("a", refs.MaxKeyBytes+1) + `":"ID"}`,
		`{"key":"` + strings.Repeat("x", refs.MaxIDBytes+1) + `"}`,
		`{"key":"` + strings.Repeat(`\u0061`, refs.MaxIDBytes+1) + `"}`,
	} {
		_, err := refs.DecodeJSON([]byte(input))
		assertValuesError(t, err)
	}

	// The largest accepted collection must remain decodable even when IDs need
	// the standard encoder's six-byte HTML escapes.
	worstEncoding := make(map[string]string, refs.MaxEntries)
	for i := range refs.MaxEntries {
		key := strings.Repeat("r", refs.MaxKeyBytes-2) + fmt.Sprintf("%02d", i)
		worstEncoding[key] = strings.Repeat("<", refs.MaxIDBytes)
	}
	values := mustTestValues(t, worstEncoding)
	encoded, err = values.MarshalJSON()
	if err != nil || len(encoded) > refs.MaxJSONBytes {
		t.Fatalf("accepted values exceed the JSON roundtrip limit: %d bytes, %v", len(encoded), err)
	}
	if decoded, err := refs.DecodeJSON(encoded); err != nil || !reflect.DeepEqual(decoded.Map(), worstEncoding) {
		t.Fatalf("largest escaped collection failed roundtrip: %v", err)
	}
}

func TestValuesUnmarshalIsAtomicAndReplaces(t *testing.T) {
	var nilReceiver *refs.Values
	assertValuesError(t, nilReceiver.UnmarshalJSON([]byte(`{}`)))
	values := mustTestValues(t, map[string]string{"merchant_id": "M1"})
	before := values.Map()
	for _, invalid := range []string{`null`, `{"channel_id":"C1","merchant_id":null}`, `{"channel_id":"C1","channel_id":"C2"}`, `{"channel_id":"\ud800"}`, `{"channel_id":"C1"} trailing`} {
		assertValuesError(t, values.UnmarshalJSON([]byte(invalid)))
		if !reflect.DeepEqual(values.Map(), before) {
			t.Fatal("failed direct unmarshal changed the receiver")
		}
		if err := json.Unmarshal([]byte(invalid), &values); err == nil {
			t.Fatal("standard unmarshal accepted invalid references")
		}
		if !reflect.DeepEqual(values.Map(), before) {
			t.Fatal("failed standard unmarshal changed the receiver")
		}
	}
	alias := values
	if err := json.Unmarshal([]byte(`{"channel_id":"C1"}`), &values); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(values.Map(), map[string]string{"channel_id": "C1"}) {
		t.Fatal("unmarshal merged instead of replacing references")
	}
	if !reflect.DeepEqual(alias.Map(), before) {
		t.Fatal("unmarshal mutated a previously copied Values")
	}
	if err := values.UnmarshalJSON([]byte(`{}`)); err != nil || values.Len() != 0 {
		t.Fatal("empty object must replace references with empty collection")
	}
}

func TestDecodeJSONDetachesInputBuffer(t *testing.T) {
	input := []byte(`{"merchant_id":"M1"}`)
	values, err := refs.DecodeJSON(input)
	if err != nil {
		t.Fatal(err)
	}
	for i := range input {
		input[i] = 'x'
	}
	if got, ok := values.Get("merchant_id"); !ok || got != "M1" {
		t.Fatal("decoder retained mutable input bytes")
	}
}

func TestValuesErrorsDoNotDiscloseIDs(t *testing.T) {
	secret := "sensitive-reference-value"
	_, newErr := refs.NewValues(map[string]string{"merchant_id": secret + " "})
	_, decodeErr := refs.DecodeJSON([]byte(`{"merchant_id":"` + secret + `","merchant_id":"second"}`))
	_, syntaxErr := refs.DecodeJSON([]byte(`{"merchant_id":"` + secret + `\q"}`))
	for _, err := range []error{newErr, decodeErr, syntaxErr} {
		assertValuesError(t, err)
		if strings.Contains(err.Error(), secret) {
			t.Fatal("error text disclosed reference ID")
		}
		appErr := apperr.From(err)
		if len(appErr.Details()) != 0 || appErr.Unwrap() != nil {
			t.Fatal("value error unexpectedly retained input details or a parser cause")
		}
	}
}

func TestValuesConcurrentReadAndMapCopies(t *testing.T) {
	values := mustTestValues(t, map[string]string{"merchant_id": "M1", "channel_id": "C1"})
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 50 {
				values.Map()["merchant_id"] = "changed"
				if got, ok := values.Get("merchant_id"); !ok || got != "M1" {
					t.Error("copied map changed shared references")
				}
				if _, err := values.MarshalJSON(); err != nil {
					t.Error(err)
				}
			}
		})
	}
	workers.Wait()
}

func mustTestValues(t *testing.T, input map[string]string) refs.Values {
	t.Helper()
	values, err := refs.NewValues(input)
	if err != nil {
		t.Fatal(err)
	}
	return values
}

func assertValuesError(t *testing.T, err error) {
	t.Helper()
	if err == nil || !apperr.Is(err, refs.CodeInvalidValues) {
		t.Fatalf("error = %v, want %s", err, refs.CodeInvalidValues)
	}
}
