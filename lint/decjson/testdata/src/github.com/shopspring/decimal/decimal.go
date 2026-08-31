// Package decimal 是 analysistest 用的桩：只提供类型名，不进真实依赖图。
package decimal

// Decimal 桩。
type Decimal struct{ i int }

// NullDecimal 桩。
type NullDecimal struct{ D Decimal }
