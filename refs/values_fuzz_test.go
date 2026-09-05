package refs_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/forgeplex/appkit/refs"
)

func FuzzValuesDecodeRoundTrip(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{}`),
		[]byte(`{"merchant_id":"M1","channel_id":"C1"}`),
		[]byte(`{"merchant_id":"M1","\u006derchant_id":"M2"}`),
		[]byte(`{"key":"\ud800"}`),
		[]byte(`{"key":"\ud83d\ude80"}`),
		[]byte(`{"key":null}`),
		[]byte(`{"key":{"id":"M1"}}`),
		[]byte("{\"key\":\"\xff\"}"),
		[]byte(`null`),
		[]byte(`{} {}`),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		before := map[string]string{"existing_id": "unchanged"}
		receiver := mustTestValues(t, before)
		decoded, err := refs.DecodeJSON(input)
		unmarshalErr := receiver.UnmarshalJSON(input)
		if err != nil {
			assertValuesError(t, err)
			assertValuesError(t, unmarshalErr)
			if decoded.Len() != 0 || !reflect.DeepEqual(receiver.Map(), before) {
				t.Fatal("invalid JSON exposed partial values or changed the receiver")
			}
			return
		}
		if unmarshalErr != nil || !reflect.DeepEqual(receiver.Map(), decoded.Map()) {
			t.Fatal("decoder and unmarshal disagree")
		}
		if !json.Valid(input) {
			t.Fatal("decoder accepted syntactically invalid JSON")
		}
		constructed, err := refs.NewValues(decoded.Map())
		if err != nil {
			t.Fatal("decoder accepted values rejected by constructor")
		}
		encoded, err := json.Marshal(decoded)
		if err != nil || !json.Valid(encoded) {
			t.Fatal("valid values did not marshal as valid JSON")
		}
		other, err := constructed.MarshalJSON()
		if err != nil || !bytes.Equal(encoded, other) {
			t.Fatal("equivalent values did not marshal deterministically")
		}
		redecoded, err := refs.DecodeJSON(encoded)
		if err != nil || !reflect.DeepEqual(redecoded.Map(), decoded.Map()) {
			t.Fatal("JSON roundtrip changed values")
		}
	})
}
