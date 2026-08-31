package money_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/money"
)

func mustParse(t *testing.T, amount, currency string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, currency)
	if err != nil {
		t.Fatalf("Parse(%q, %q): %v", amount, currency, err)
	}
	return m
}

func TestNew(t *testing.T) {
	tests := []struct {
		name     string
		currency string
		wantCode string // 空串表示期望成功
	}{
		{"合法大写三位", "USD", ""},
		{"合法人民币", "CNY", ""},
		{"资产代码四位", "USDT", ""},
		{"资产代码六位（上限）", "ABCDEF", ""},
		{"小写", "usd", money.CodeInvalidCurrency},
		{"混合大小写", "Usd", money.CodeInvalidCurrency},
		{"两位", "US", money.CodeInvalidCurrency},
		{"七位（超上限）", "ABCDEFG", money.CodeInvalidCurrency},
		{"空串", "", money.CodeInvalidCurrency},
		{"含数字", "US1", money.CodeInvalidCurrency},
		{"含非 ASCII", "ÜSD", money.CodeInvalidCurrency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := money.New(decimal.New(1050, -2), tt.currency)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				if m.Currency() != tt.currency {
					t.Fatalf("Currency() = %q, want %q", m.Currency(), tt.currency)
				}
				return
			}
			if !apperr.Is(err, tt.wantCode) {
				t.Fatalf("err = %v, want code %s", err, tt.wantCode)
			}
		})
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name     string
		amount   string
		currency string
		wantCode string
		wantStr  string
	}{
		{"两位小数", "10.50", "USD", "", "10.50 USD"},
		{"整数", "10", "USD", "", "10 USD"},
		{"一位小数", "10.5", "USD", "", "10.5 USD"},
		{"负数", "-3.14", "EUR", "", "-3.14 EUR"},
		{"零保留标度", "0.00", "JPY", "", "0.00 JPY"},
		{"科学计数", "1e2", "USD", "", "100 USD"},
		{"非数字", "abc", "USD", money.CodeInvalidAmount, ""},
		{"空数额", "", "USD", money.CodeInvalidAmount, ""},
		{"币种非法", "10.50", "usd", money.CodeInvalidCurrency, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := money.Parse(tt.amount, tt.currency)
			if tt.wantCode != "" {
				if !apperr.Is(err, tt.wantCode) {
					t.Fatalf("err = %v, want code %s", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := m.String(); got != tt.wantStr {
				t.Fatalf("String() = %q, want %q", got, tt.wantStr)
			}
		})
	}
}

func TestAddSub(t *testing.T) {
	tests := []struct {
		name     string
		a, b     money.Money
		op       string // "add" | "sub"
		want     string // 期望 String()
		wantCode string
	}{
		{"加法", mustParse(t, "10.50", "USD"), mustParse(t, "0.50", "USD"), "add", "11.00 USD", ""},
		{"加负数", mustParse(t, "10.50", "USD"), mustParse(t, "-20.00", "USD"), "add", "-9.50 USD", ""},
		{"减法", mustParse(t, "10.50", "USD"), mustParse(t, "0.40", "USD"), "sub", "10.10 USD", ""},
		{"减到负", mustParse(t, "1.00", "USD"), mustParse(t, "2.50", "USD"), "sub", "-1.50 USD", ""},
		{"加法币种不匹配", mustParse(t, "1", "USD"), mustParse(t, "1", "EUR"), "add", "", money.CodeCurrencyMismatch},
		{"减法币种不匹配", mustParse(t, "1", "USD"), mustParse(t, "1", "EUR"), "sub", "", money.CodeCurrencyMismatch},
		{"与零值 Money 运算", mustParse(t, "1", "USD"), money.Money{}, "add", "", money.CodeCurrencyMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				got money.Money
				err error
			)
			switch tt.op {
			case "add":
				got, err = tt.a.Add(tt.b)
			case "sub":
				got, err = tt.a.Sub(tt.b)
			}
			if tt.wantCode != "" {
				if !apperr.Is(err, tt.wantCode) {
					t.Fatalf("err = %v, want code %s", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: %v", tt.op, err)
			}
			if got.String() != tt.want {
				t.Fatalf("String() = %q, want %q", got.String(), tt.want)
			}
		})
	}
}

// TestAmountLimits 覆盖金额域上限：超长输入、巨指数（"1e100000000" 这类
// 20 字节输入可致 100MB 渲染输出的 DoS 向量）、超位数系数，以及恰好在
// 边界上的合法值。
func TestAmountLimits(t *testing.T) {
	tests := []struct {
		name     string
		amount   string
		wantCode string // 空串表示期望成功
	}{
		{"巨正指数科学计数", "1e100000000", money.CodeInvalidAmount},
		{"巨负指数科学计数", "1e-100000000", money.CodeInvalidAmount},
		{"指数超int32仍拒绝", "1e10000000000", money.CodeInvalidAmount},
		{"超长数字串65字节", strings.Repeat("9", 65), money.CodeInvalidAmount},
		{"超长数字串1KB", strings.Repeat("1", 1024), money.CodeInvalidAmount},
		{"39位有效数字", strings.Repeat("9", 39), money.CodeInvalidAmount},
		{"长度合规但39位有效数字（含小数点）", strings.Repeat("9", 20) + "." + strings.Repeat("9", 19), money.CodeInvalidAmount},
		{"指数31越界", "1e31", money.CodeInvalidAmount},
		{"指数-19越界", "1e-19", money.CodeInvalidAmount},
		{"边界：指数恰为30", "1e30", ""},
		{"边界：指数恰为-18", "1e-18", ""},
		{"边界：18位小数", "0.123456789012345678", ""},
		{"边界：恰38位有效数字", strings.Repeat("9", 38), ""},
		{"边界：恰64字节（前导零）", strings.Repeat("0", 63) + "1", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := money.Parse(tt.amount, "USD")
			if tt.wantCode != "" {
				if !apperr.Is(err, tt.wantCode) {
					t.Fatalf("err = %v, want code %s", err, tt.wantCode)
				}
				// 错误文本自身不得携带放大后的内容。
				if len(err.Error()) > 256 {
					t.Fatalf("错误文本过长（%d 字节），疑似携带原始输入", len(err.Error()))
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.amount, err)
			}
			// 合法值渲染成本必须有界。
			if got := m.String(); len(got) > 128 {
				t.Fatalf("String() 长度 = %d，超出金额域应有上界", len(got))
			}
		})
	}
}

// TestNewAmountLimits 验证 New 与 Parse 共用同一套数额校验。
func TestNewAmountLimits(t *testing.T) {
	tests := []struct {
		name     string
		d        decimal.Decimal
		wantCode string
	}{
		{"巨指数", decimal.New(1, 100000000), money.CodeInvalidAmount},
		{"巨负指数", decimal.New(1, -100000000), money.CodeInvalidAmount},
		{"39位系数", decimal.RequireFromString(strings.Repeat("9", 39)), money.CodeInvalidAmount},
		{"边界指数30", decimal.New(1, 30), ""},
		{"边界指数-18", decimal.New(1, -18), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := money.New(tt.d, "USD")
			if tt.wantCode != "" {
				if !apperr.Is(err, tt.wantCode) {
					t.Fatalf("err = %v, want code %s", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
		})
	}
}

// TestUnmarshalJSONAmountLimits 验证 JSON 入口同样拒绝超界 amount。
func TestUnmarshalJSONAmountLimits(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"巨指数", `{"amount":"1e100000000","currency":"USD"}`},
		{"超长数字串", `{"amount":"` + strings.Repeat("9", 65) + `","currency":"USD"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m money.Money
			err := json.Unmarshal([]byte(tt.in), &m)
			if !apperr.Is(err, money.CodeInvalidAmount) {
				t.Fatalf("err = %v, want code %s", err, money.CodeInvalidAmount)
			}
		})
	}
}

// TestPrecision 是禁 float 的动机测试：0.1 + 0.2 必须精确等于 0.3。
func TestPrecision(t *testing.T) {
	sum, err := mustParse(t, "0.1", "USD").Add(mustParse(t, "0.2", "USD"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := sum.String(); got != "0.3 USD" {
		t.Fatalf("String() = %q, want %q", got, "0.3 USD")
	}
	cmp, err := sum.Cmp(mustParse(t, "0.3", "USD"))
	if err != nil || cmp != 0 {
		t.Fatalf("Cmp = (%d, %v), want (0, nil)", cmp, err)
	}
}

func TestMulNeg(t *testing.T) {
	tests := []struct {
		name string
		got  money.Money
		want string
	}{
		{"乘整数", mustParse(t, "10.50", "USD").Mul(decimal.NewFromInt(3)), "31.50 USD"},
		{"乘费率标度相加", mustParse(t, "10.50", "USD").Mul(decimal.New(1, -1)), "1.050 USD"},
		{"乘零", mustParse(t, "10.50", "USD").Mul(decimal.Decimal{}), "0.00 USD"},
		{"取负", mustParse(t, "10.50", "USD").Neg(), "-10.50 USD"},
		{"负取负", mustParse(t, "-10.50", "USD").Neg(), "10.50 USD"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.got.String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCmp(t *testing.T) {
	tests := []struct {
		name     string
		a, b     money.Money
		want     int
		wantCode string
	}{
		{"小于", mustParse(t, "1.00", "USD"), mustParse(t, "2", "USD"), -1, ""},
		{"等于忽略标度", mustParse(t, "0.30", "USD"), mustParse(t, "0.3", "USD"), 0, ""},
		{"大于", mustParse(t, "2", "USD"), mustParse(t, "1.99", "USD"), 1, ""},
		{"币种不匹配", mustParse(t, "1", "USD"), mustParse(t, "1", "EUR"), 0, money.CodeCurrencyMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.a.Cmp(tt.b)
			if tt.wantCode != "" {
				if !apperr.Is(err, tt.wantCode) {
					t.Fatalf("err = %v, want code %s", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("Cmp: %v", err)
			}
			if got != tt.want {
				t.Fatalf("Cmp = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestPredicates(t *testing.T) {
	tests := []struct {
		name                     string
		m                        money.Money
		zero, negative, positive bool
	}{
		{"Zero 构造", money.Zero("USD"), true, false, false},
		{"零保留标度", mustParse(t, "0.00", "USD"), true, false, false},
		{"正数", mustParse(t, "0.01", "USD"), false, false, true},
		{"负数", mustParse(t, "-0.01", "USD"), false, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.m.IsZero(); got != tt.zero {
				t.Errorf("IsZero = %v, want %v", got, tt.zero)
			}
			if got := tt.m.IsNegative(); got != tt.negative {
				t.Errorf("IsNegative = %v, want %v", got, tt.negative)
			}
			if got := tt.m.IsPositive(); got != tt.positive {
				t.Errorf("IsPositive = %v, want %v", got, tt.positive)
			}
		})
	}
}

func TestZero(t *testing.T) {
	z := money.Zero("USD")
	if z.Currency() != "USD" || !z.IsZero() {
		t.Fatalf("Zero = %v", z)
	}
	sum, err := z.Add(mustParse(t, "10.50", "USD"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := sum.String(); got != "10.50 USD" {
		t.Fatalf("String() = %q, want %q", got, "10.50 USD")
	}
}

func TestMarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		m    money.Money
		want string
	}{
		{"两位小数", mustParse(t, "10.50", "USD"), `{"amount":"10.50","currency":"USD"}`},
		{"整数", mustParse(t, "10", "USD"), `{"amount":"10","currency":"USD"}`},
		{"负数", mustParse(t, "-0.01", "EUR"), `{"amount":"-0.01","currency":"EUR"}`},
		{"Zero", money.Zero("JPY"), `{"amount":"0","currency":"JPY"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.m)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(b) != tt.want {
				t.Fatalf("Marshal = %s, want %s", b, tt.want)
			}
		})
	}
}

func TestUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantCode string
	}{
		{"合法", `{"amount":"10.50","currency":"USD"}`, ""},
		{"amount 是 float", `{"amount":10.50,"currency":"USD"}`, money.CodeInvalidAmount},
		{"amount 是整数", `{"amount":10,"currency":"USD"}`, money.CodeInvalidAmount},
		{"amount 非数字串", `{"amount":"ten","currency":"USD"}`, money.CodeInvalidAmount},
		{"缺 amount", `{"currency":"USD"}`, money.CodeInvalidAmount},
		{"币种非法", `{"amount":"10.50","currency":"usd"}`, money.CodeInvalidCurrency},
		{"缺 currency", `{"amount":"10.50"}`, money.CodeInvalidCurrency},
		{"非对象", `"10.50 USD"`, money.CodeInvalidAmount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m money.Money
			err := json.Unmarshal([]byte(tt.in), &m)
			if tt.wantCode != "" {
				if !apperr.Is(err, tt.wantCode) {
					t.Fatalf("err = %v, want code %s", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got := m.String(); got != "10.50 USD" {
				t.Fatalf("String() = %q, want %q", got, "10.50 USD")
			}
		})
	}
}

// TestJSONRoundTrip 验证 编→解→编 完全稳定（含标度）。
func TestJSONRoundTrip(t *testing.T) {
	tests := []string{
		`{"amount":"10.50","currency":"USD"}`,
		`{"amount":"0.3","currency":"USD"}`,
		`{"amount":"-123456789012345678.123456789","currency":"CNY"}`,
		`{"amount":"0.00","currency":"JPY"}`,
	}
	for _, in := range tests {
		t.Run(in, func(t *testing.T) {
			var m money.Money
			if err := json.Unmarshal([]byte(in), &m); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			out, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(out) != in {
				t.Fatalf("往返 = %s, want %s", out, in)
			}
		})
	}
}

// TestImmutable 验证运算不改写接收者。
func TestImmutable(t *testing.T) {
	m := mustParse(t, "10.50", "USD")
	if _, err := m.Add(mustParse(t, "1", "USD")); err != nil {
		t.Fatal(err)
	}
	m.Neg()
	m.Mul(decimal.NewFromInt(100))
	if got := m.String(); got != "10.50 USD" {
		t.Fatalf("接收者被改写: %q", got)
	}
}
