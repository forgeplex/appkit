package pgxmoney

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"
)

func newMap() *pgtype.Map {
	m := pgtype.NewMap()
	registerMap(m)
	return m
}

// TestMapRoundTrip 不连库，直接经 type map 的编解码计划做文本/二进制双格式往返。
func TestMapRoundTrip(t *testing.T) {
	m := newMap()
	formats := []struct {
		name string
		code int16
	}{
		{"text", pgtype.TextFormatCode},
		{"binary", pgtype.BinaryFormatCode},
	}
	values := []string{
		"0",
		"1",
		"-1",
		"10.50",
		"0.1",
		"-3.14159",
		"123456789012345678.123456789",
		"-0.000000001",
		"1e6",
	}
	for _, f := range formats {
		for _, v := range values {
			t.Run(f.name+"/"+v, func(t *testing.T) {
				want := decimal.RequireFromString(v)
				buf, err := m.Encode(pgtype.NumericOID, f.code, want, nil)
				if err != nil {
					t.Fatalf("Encode: %v", err)
				}
				var got decimal.Decimal
				if err := m.Scan(pgtype.NumericOID, f.code, buf, &got); err != nil {
					t.Fatalf("Scan: %v", err)
				}
				if got.Cmp(want) != 0 {
					t.Fatalf("往返 = %s, want %s", got, want)
				}
			})
		}
	}
}

func TestMapScanErrors(t *testing.T) {
	m := newMap()
	tests := []struct {
		name string
		src  []byte
	}{
		{"NULL 进值类型", nil},
		{"NaN", []byte("NaN")},
		{"Infinity", []byte("Infinity")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got decimal.Decimal
			if err := m.Scan(pgtype.NumericOID, pgtype.TextFormatCode, tt.src, &got); err == nil {
				t.Fatalf("Scan(%q) = nil, want error", tt.src)
			}
		})
	}
}

// TestMapScanNull 验证可空列的惯用目标 **decimal.Decimal。
func TestMapScanNull(t *testing.T) {
	m := newMap()

	sentinel := decimal.RequireFromString("1")
	p := &sentinel
	if err := m.Scan(pgtype.NumericOID, pgtype.TextFormatCode, nil, &p); err != nil {
		t.Fatalf("Scan(NULL): %v", err)
	}
	if p != nil {
		t.Fatalf("p = %v, want nil", p)
	}

	if err := m.Scan(pgtype.NumericOID, pgtype.TextFormatCode, []byte("10.50"), &p); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if p == nil || p.Cmp(decimal.RequireFromString("10.50")) != 0 {
		t.Fatalf("p = %v, want 10.50", p)
	}
}

// TestMapTypeForValue 验证未知 OID 的参数能从 Go 类型推导为 numeric。
func TestMapTypeForValue(t *testing.T) {
	m := newMap()
	tests := []struct {
		name  string
		value any
		want  uint32
	}{
		{"值", decimal.Decimal{}, pgtype.NumericOID},
		{"指针", (*decimal.Decimal)(nil), pgtype.NumericOID},
		{"切片", []decimal.Decimal{}, pgtype.NumericArrayOID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ty, ok := m.TypeForValue(tt.value)
			if !ok || ty.OID != tt.want {
				t.Fatalf("TypeForValue = (%v, %v), want OID %d", ty, ok, tt.want)
			}
		})
	}
}

// TestDBRoundTrip 走真实 Postgres（默认扩展协议 = 二进制线格式）。
func TestDBRoundTrip(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过数据库测试")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	Register(conn)

	values := []string{"0", "10.50", "0.1", "-3.14159", "123456789012345678.123456789"}
	for _, v := range values {
		t.Run(v, func(t *testing.T) {
			want := decimal.RequireFromString(v)
			var got decimal.Decimal
			if err := conn.QueryRow(ctx, "select $1::numeric", want).Scan(&got); err != nil {
				t.Fatalf("QueryRow: %v", err)
			}
			if got.Cmp(want) != 0 {
				t.Fatalf("往返 = %s, want %s", got, want)
			}
		})
	}

	t.Run("字面量", func(t *testing.T) {
		var got decimal.Decimal
		if err := conn.QueryRow(ctx, "select 10.50::numeric").Scan(&got); err != nil {
			t.Fatalf("QueryRow: %v", err)
		}
		if got.Cmp(decimal.RequireFromString("10.50")) != 0 {
			t.Fatalf("got %s, want 10.50", got)
		}
	})

	t.Run("NULL", func(t *testing.T) {
		sentinel := decimal.RequireFromString("1")
		p := &sentinel
		if err := conn.QueryRow(ctx, "select null::numeric").Scan(&p); err != nil {
			t.Fatalf("QueryRow: %v", err)
		}
		if p != nil {
			t.Fatalf("p = %v, want nil", p)
		}
	})
}
