package a

import "encoding/json"

type dto struct {
	Amount string `json:"amount"`
}

func f(raw []byte) {
	var v any
	_ = json.Unmarshal(raw, &v) // want `禁止 json\.Unmarshal 直接解码到动态类型`

	var m map[string]any
	_ = json.Unmarshal(raw, &m) // want `禁止 json\.Unmarshal 直接解码到动态类型`

	var s []interface{}
	_ = json.Unmarshal(raw, &s) // want `禁止 json\.Unmarshal 直接解码到动态类型`

	// 解码到具名 struct 或 map[string]string 合法。
	var d dto
	_ = json.Unmarshal(raw, &d)
	var ms map[string]string
	_ = json.Unmarshal(raw, &ms)

	//appkit:ignore jsonany -- 非金额透传元数据
	_ = json.Unmarshal(raw, &m)
}
