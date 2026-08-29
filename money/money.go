// Package money 提供不可变的金额值类型：decimal 数额 + ISO 4217 币种。
//
// 全系统金额禁用 float（appkit-lint 强制），金额只以 Money 或 decimal.Decimal
// 流转；JSON 里 amount 一律是字符串。数额保留构造时的标度（scale）：
// Parse("10.50") 的字符串形态始终是 "10.50" 而非 "10.5"。
// 数据库 NUMERIC 映射见子包 pgxmoney。
package money

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/shopspring/decimal"

	"github.com/forgeplex/appkit/apperr"
)

// 本包错误码。money 属框架层，不进合约仓库的业务错误码注册表。
const (
	CodeInvalidCurrency  = "MONEY_INVALID_CURRENCY"
	CodeInvalidAmount    = "MONEY_INVALID_AMOUNT"
	CodeCurrencyMismatch = "MONEY_CURRENCY_MISMATCH"
)

// 金额域约束（对齐 NUMERIC 与业务金额范围）：
//   - 输入字符串最长 64 字节：解析前的粗筛，挡住 "1e100000000" 这类
//     短输入撑爆渲染、以及超长数字串本身的解析开销（DoS）；
//   - 标度（指数）限于 [-18, 30]，系数最多 38 位十进制数字。
const (
	maxAmountLen = 64
	minExponent  = -18
	maxExponent  = 30
	maxDigits    = 38
)

var (
	errInvalidCurrency = apperr.New(CodeInvalidCurrency, http.StatusUnprocessableEntity,
		"currency 必须是 3 位大写字母的 ISO 4217 代码")
	errInvalidAmount = apperr.New(CodeInvalidAmount, http.StatusUnprocessableEntity,
		"amount 必须是十进制数字字符串")
	errCurrencyMismatch = apperr.New(CodeCurrencyMismatch, http.StatusUnprocessableEntity,
		"币种不匹配")
)

// Money 是不可变值：所有运算返回新值，可安全按值传递与共享。
// 零值 Money{} 的币种为空串，不可用；请经 New/Parse/Zero 构造。
type Money struct {
	amount   decimal.Decimal
	currency string
}

// New 构造 Money。currency 只校验格式（3 位大写字母），不校验是否在
// ISO 4217 现行表内——币种白名单属业务合约层。
func New(amount decimal.Decimal, currency string) (Money, error) {
	if !validCurrency(currency) {
		return Money{}, errInvalidCurrency.WithDetail("currency", currency)
	}
	if err := validAmount(amount); err != nil {
		return Money{}, err
	}
	return Money{amount: amount, currency: currency}, nil
}

// Parse 从十进制字符串构造 Money，保留字符串携带的标度。
func Parse(amount string, currency string) (Money, error) {
	if len(amount) > maxAmountLen {
		// 错误里只带长度不带原文，避免超长输入再经错误链外泄/放大。
		return Money{}, errInvalidAmount.
			WithMessage("amount 字符串过长（最多 %d 字节）", maxAmountLen).
			WithDetail("length", len(amount))
	}
	d, err := decimal.NewFromString(amount)
	if err != nil {
		return Money{}, errInvalidAmount.WithDetail("amount", amount).WithCause(err)
	}
	return New(d, currency)
}

// validAmount 校验数额在金额域内。New 与 Parse 共用，保证任何已构造的
// Money 渲染成本有界（String/MarshalJSON 不会被巨指数撑爆）。
func validAmount(d decimal.Decimal) error {
	if exp := d.Exponent(); exp < minExponent || exp > maxExponent {
		return errInvalidAmount.
			WithMessage("amount 指数超出 [%d, %d]", minExponent, maxExponent).
			WithDetail("exponent", exp)
	}
	coef := d.Coefficient()
	if digits := len(coef.Abs(coef).String()); digits > maxDigits {
		return errInvalidAmount.
			WithMessage("amount 有效数字超过 %d 位", maxDigits).
			WithDetail("digits", digits)
	}
	return nil
}

// Zero 返回指定币种的零值。currency 由调用方保证合法（通常为常量）；
// 需要校验时用 New(decimal.Decimal{}, currency)。
func Zero(currency string) Money {
	return Money{currency: currency}
}

func validCurrency(c string) bool {
	if len(c) != 3 {
		return false
	}
	for i := 0; i < len(c); i++ {
		if c[i] < 'A' || c[i] > 'Z' {
			return false
		}
	}
	return true
}

// Amount 返回数额。decimal.Decimal 运算不改写操作数，可安全外传。
func (m Money) Amount() decimal.Decimal { return m.amount }

// Currency 返回币种代码。
func (m Money) Currency() string { return m.currency }

// Add 返回 m + o。币种不匹配返回 CodeCurrencyMismatch。
func (m Money) Add(o Money) (Money, error) {
	if err := m.sameCurrency(o); err != nil {
		return Money{}, err
	}
	return Money{amount: m.amount.Add(o.amount), currency: m.currency}, nil
}

// Sub 返回 m - o。币种不匹配返回 CodeCurrencyMismatch。
func (m Money) Sub(o Money) (Money, error) {
	if err := m.sameCurrency(o); err != nil {
		return Money{}, err
	}
	return Money{amount: m.amount.Sub(o.amount), currency: m.currency}, nil
}

// Mul 返回 m × f（费率、汇率等无币种系数）。结果标度为两操作数标度之和，
// 落库/出账前的舍入策略由业务层决定。
func (m Money) Mul(f decimal.Decimal) Money {
	return Money{amount: m.amount.Mul(f), currency: m.currency}
}

// Neg 返回 -m。
func (m Money) Neg() Money {
	return Money{amount: m.amount.Neg(), currency: m.currency}
}

// Cmp 比较数额：m < o 返回 -1，相等返回 0，m > o 返回 1。
// 比较忽略标度差异（"0.3" 与 "0.30" 相等）。币种不匹配返回 CodeCurrencyMismatch。
func (m Money) Cmp(o Money) (int, error) {
	if err := m.sameCurrency(o); err != nil {
		return 0, err
	}
	return m.amount.Cmp(o.amount), nil
}

func (m Money) IsZero() bool     { return m.amount.IsZero() }
func (m Money) IsNegative() bool { return m.amount.IsNegative() }
func (m Money) IsPositive() bool { return m.amount.IsPositive() }

// String 返回 "10.50 USD" 形态，数额按自身标度渲染。
func (m Money) String() string {
	return fmt.Sprintf("%s %s", formatAmount(m.amount), m.currency)
}

func (m Money) sameCurrency(o Money) error {
	if m.currency != o.currency {
		return errCurrencyMismatch.
			WithDetail("left", m.currency).
			WithDetail("right", o.currency)
	}
	return nil
}

// formatAmount 按数额自身的标度渲染，不丢尾零：
// decimal.String() 会把 "10.50" 缩成 "10.5"，这里用 StringFixed 保住标度。
func formatAmount(d decimal.Decimal) string {
	if exp := d.Exponent(); exp < 0 {
		return d.StringFixed(-exp)
	}
	return d.String()
}

// moneyJSON 是线上形态：amount 必须是 JSON 字符串。
// 字段用 string 而非数字类型，使数字形态的 amount 在解码期即被拒绝。
type moneyJSON struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// MarshalJSON 输出 {"amount":"10.50","currency":"USD"}，amount 恒为字符串。
func (m Money) MarshalJSON() ([]byte, error) {
	return json.Marshal(moneyJSON{Amount: formatAmount(m.amount), Currency: m.currency})
}

// UnmarshalJSON 解析并校验；amount 为 JSON 数字（含 float）时直接报错。
// 失败时 *m 保持原值不变。
func (m *Money) UnmarshalJSON(data []byte) error {
	var raw moneyJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return errInvalidAmount.
			WithMessage("money JSON 必须形如 {\"amount\":\"10.50\",\"currency\":\"USD\"}，amount 必须是字符串").
			WithCause(err)
	}
	parsed, err := Parse(raw.Amount, raw.Currency)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}
