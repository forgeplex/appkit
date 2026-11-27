package skiptests

import "encoding/json"

func f(raw []byte) {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
}
