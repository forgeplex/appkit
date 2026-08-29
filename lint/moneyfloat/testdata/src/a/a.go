// 默认（全包）模式下的正反例。
package a

import "time"

// Money 与 Decimal 模拟 money.Money / decimal.Decimal：命名类型合规，不报。
type Money struct {
	Amount   int64
	Currency string
}

type Decimal struct {
	coef uint64
	exp  int32
}

// struct 字段类型。
type Payment struct {
	Amount    float64   // want `struct 字段使用 float64：金额与业务数值禁用浮点，改用 money.Money 或 decimal.Decimal`
	FeeRate   float32   // want `struct 字段使用 float32：金额与业务数值禁用浮点，改用 money.Money 或 decimal.Decimal`
	History   []float64 // want `struct 字段使用 float64：金额与业务数值禁用浮点`
	Total     Money     // 合规
	Precise   Decimal   // 合规
	Reference string
}

// 变量/常量声明类型。
var globalRatio float64 // want `变量/常量声明使用 float64：金额与业务数值禁用浮点`

const maxSkew float32 = 0.01 // want `变量/常量声明使用 float32：金额与业务数值禁用浮点`

var amounts map[string]float64 // want `变量/常量声明使用 float64：金额与业务数值禁用浮点`

var reference string // 合规

// 函数参数与返回值（同一行两处，各报一次）。
func Convert(amount float64, currency string) float64 { // want `函数参数或返回值使用 float64：金额与业务数值禁用浮点` `函数参数或返回值使用 float64：金额与业务数值禁用浮点`
	return amount
}

func Transfer(from Money, to Money) Money { return from } // 合规

// 显式类型转换。time 计算里的 float 同样会报——用 -scope 圈定业务包。
func Seconds(d time.Duration) float64 { // want `函数参数或返回值使用 float64：金额与业务数值禁用浮点`
	return float64(d) / float64(time.Second) // want `类型转换使用 float64：金额与业务数值禁用浮点` `类型转换使用 float64：金额与业务数值禁用浮点`
}

func local() {
	var v float64 // want `变量/常量声明使用 float64：金额与业务数值禁用浮点`
	_ = v
	f := float32(1) // want `类型转换使用 float32：金额与业务数值禁用浮点`
	_ = f
	n := int64(42) // 合规：非浮点转换
	_ = n
}

// struct 字段是函数类型：按位置去重，两个 ident 各报一次、不重复。
type hooks struct {
	Score func(x float64) float64 // want `struct 字段使用 float64：金额与业务数值禁用浮点` `struct 字段使用 float64：金额与业务数值禁用浮点`
}
