// Package money 提供不可变的金额值类型：decimal 数额 + 币种代码。
//
// 全系统金额禁用 float（appkit-lint 强制），金额只以 Money 或 decimal.Decimal
// 流转；JSON 里 amount 一律是字符串。数额保留构造时的标度（scale）：
// Parse("10.50") 的字符串形态始终是 "10.50" 而非 "10.5"。
// 入站解析用 ParseCanonical：只收规范形态（拒 "+80"/"080"/"8e1"/负零），
// 同值只有一种字节形态，幂等指纹与签名才可比。
//
// Money 是领域层值对象，不是持久化类型：单个 NUMERIC 列还原不了币种，
// 落库时金额（decimal.Decimal——sqlc 经脚手架全局 override 映射 NUMERIC，
// pgx 原生编解码，无需注册 codec）与币种分列存储，读出后再由领域层组装。
// 币种白名单与各资产标度（USD=2、USDT=6…）属业务知识，放资产注册表之类
// 的表，不进本包。
package money

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

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
		"currency 必须是 3-6 位大写字母（ISO 4217 或资产代码，如 USDT）")
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

// New 构造 Money。currency 只校验格式（3-6 位大写字母），不校验代码
// 是否真实存在——ISO 4217 恰好 3 位，资产代码更长（USDT/USDC/WBTC），
// 币种白名单属业务合约层（资产注册表）。
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

// canonicalForm 是入站金额唯一可接受的形态：可选负号 + (0|[1-9]\d*) +
// 可选小数部分。"+80"、"080"、".5"、"5."、"8e1" 与 "80"/"0.5" 同值不同串，
// 过不了这道闸——它们会把幂等指纹、请求签名、对账的字符串比对打歪
// （idem 的指纹吃原始字节，"80" 与 "80.00" 本就是两个不同的 hash，
// 再放任 "+80"/"8e1" 进来，同值的形态就更多了）。
var canonicalForm = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`)

// ParseCanonical 按 Parse 的全部约束解析，且只接受规范形态的金额字符串
// （并拒绝 "-0"）。入站 DTO 用它：同值只有一种字节形态，是幂等指纹与
// 签名可比的前提。出站渲染不受此限——按资产标度定宽是 DTO 层的事。
func ParseCanonical(amount string, currency string) (Money, error) {
	if len(amount) > maxAmountLen {
		return Money{}, errInvalidAmount.
			WithMessage("amount 字符串过长（最多 %d 字节）", maxAmountLen).
			WithDetail("length", len(amount))
	}
	if !canonicalForm.MatchString(amount) {
		return Money{}, errInvalidAmount.
			WithMessage("amount 必须是规范十进制字符串（如 \"80\"、\"80.00\"、\"-3.5\"）：拒绝正号、前导零、裸小数点、科学计数法")
	}
	m, err := Parse(amount, currency)
	if err != nil {
		return Money{}, err
	}
	if m.IsZero() && strings.HasPrefix(amount, "-") {
		return Money{}, errInvalidAmount.
			WithMessage("amount 不接受负零（\"0\"/\"0.00\" 已是零的规范形态）")
	}
	return m, nil
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

// validCurrency 接受 3-6 位大写 ASCII 字母：ISO 4217 是 3 位，多资产
// 场景下还要容得下 USDT/USDC/WBTC 这类资产代码。只查格式。
func validCurrency(c string) bool {
	if len(c) < 3 || len(c) > 6 {
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
