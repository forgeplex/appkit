package http

import "github.com/shopspring/decimal"

func handle(raw string) {
	_, _ = decimal.NewFromString(raw)  // want `禁止在 HTTP 边界直接调用 decimal\.NewFromString`
	_ = decimal.RequireFromString(raw) // want `禁止在 HTTP 边界直接调用 decimal\.RequireFromString`

	//appkit:ignore canonicalmoney -- 内部常量换算
	_ = decimal.RequireFromString("100")
}
