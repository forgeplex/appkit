// Package a 是 decjson 的测试样本。
package a

import "github.com/shopspring/decimal"

// 命中：json tag + decimal 包类型，全部复合形态都要抓到。
type wireBad struct {
	A decimal.Decimal            `json:"a"`           // want `json tag 字段的类型用了 Decimal`
	B *decimal.Decimal           `json:"b"`           // want `json tag 字段的类型用了 Decimal`
	C []decimal.Decimal          `json:"c"`           // want `json tag 字段的类型用了 Decimal`
	D map[string]decimal.Decimal `json:"d"`           // want `json tag 字段的类型用了 Decimal`
	E decimal.NullDecimal        `json:"e"`           // want `json tag 字段的类型用了 NullDecimal`
	F decimal.Decimal            `json:"f,omitempty"` // want `json tag 字段的类型用了 Decimal`
}

// 合法：字符串字段 + 显式转换才是金额上 JSON 面的正解。
type wireOK struct {
	A string `json:"a"`
	B int64  `json:"b"`
}

// 合法：json:"-" 明确不参与序列化（内部/调试字段）。
type excluded struct {
	A decimal.Decimal `json:"-"`
}

// 合法：无 json tag——是否会被 marshal 不可知，宁缺毋滥（见包文档）。
type domain struct {
	A decimal.Decimal
	B *decimal.Decimal
}

// 合法：其他 tag（db 列映射）不是 JSON 面。
type stored struct {
	A decimal.Decimal `db:"a"`
}

// 局部同名类型不该误报：按 import path 而非名字匹配。
type Decimal struct{ x int }

type sameName struct {
	A Decimal `json:"a"`
}
