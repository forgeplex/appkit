// scope 匹配本包（^scoped/pay$）：正常报告。
package pay

type Charge struct {
	Amount float64 // want `struct 字段使用 float64：金额与业务数值禁用浮点，改用 money.Money 或 decimal.Decimal`
}
