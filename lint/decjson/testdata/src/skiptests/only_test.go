// Package skiptests 验证默认不查 _test.go：下面的违规没有 want 注释，
// 若默认行为被改坏（开始查测试文件），analysistest 会报"未期望的诊断"。
package skiptests

import "github.com/shopspring/decimal"

type dtoTest struct {
	A decimal.Decimal `json:"a"`
}
