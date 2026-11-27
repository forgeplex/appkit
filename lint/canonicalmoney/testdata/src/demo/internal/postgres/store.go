package postgres

import "github.com/shopspring/decimal"

func readDB(raw string) {
	// 非 HTTP 层默认不拦。
	_, _ = decimal.NewFromString(raw)
}
