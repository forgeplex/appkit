// Package withtests 配合 -tests=true 跑：测试文件里的违规也要抓到。
package withtests

import "github.com/shopspring/decimal"

type dtoTest struct {
	A decimal.Decimal `json:"a"` // want `json tag 字段的类型用了 Decimal`
}
