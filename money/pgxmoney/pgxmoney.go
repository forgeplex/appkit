// Package pgxmoney 把 Postgres NUMERIC ↔ shopspring/decimal 的编解码注册进
// pgx v5 的 type map，供 sqlc 生成代码与手写查询直接以 decimal.Decimal
// 读写 NUMERIC 列（全系统禁 float 金额的数据库侧落点）。
//
// 实现方式是桥接 pgtype.NumericCodec：包内 wrapper 类型实现
// pgtype.NumericValuer / NumericScanner，经 TryWrap*PlanFuncs 把
// decimal.Decimal 转到 wrapper，编解码本身仍走内建 codec（文本与二进制
// 两种线格式都覆盖），不引入额外依赖。
//
// Deprecated: 本包没有存在必要。shopspring/decimal 自带 driver.Valuer 与
// sql.Scanner，pgx v5 对未注册的类型自动回落到这两个接口——实测有无本包，
// decimal.Decimal 读写 NUMERIC 的结果完全一致（含数组、NULL、CopyFrom、
// Batch、简单协议、高精度）。新代码直接用 decimal.Decimal 即可，不要接入。
package pgxmoney

import (
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"
)

// Register 把 NUMERIC ↔ decimal.Decimal 编解码注册到 conn 的 type map。
// type map 归单个连接所有，因此要在建连钩子里对每条连接调用
// （pgxpool.Config.AfterConnect）。
//
// Deprecated: 无需注册，见包文档。
func Register(conn *pgx.Conn) {
	registerMap(conn.TypeMap())
}

// registerMap 拆出 map 级注册，便于不依赖数据库的单元测试。
func registerMap(m *pgtype.Map) {
	// 前插：wrapper 必须先于内建的 TryWrapStructPlan 等命中 decimal.Decimal
	//（它是含未导出字段的 struct，落到内建 wrap 只会得到无用计划）。
	m.TryWrapEncodePlanFuncs = append(
		[]pgtype.TryWrapEncodePlanFunc{tryWrapNumericEncodePlan},
		m.TryWrapEncodePlanFuncs...)
	m.TryWrapScanPlanFuncs = append(
		[]pgtype.TryWrapScanPlanFunc{tryWrapNumericScanPlan},
		m.TryWrapScanPlanFuncs...)

	// 让未知 OID 的查询参数（如 $1 直接传 decimal.Decimal）推导为 numeric。
	m.RegisterDefaultPgType(decimal.Decimal{}, "numeric")
	m.RegisterDefaultPgType((*decimal.Decimal)(nil), "numeric")
	m.RegisterDefaultPgType([]decimal.Decimal{}, "_numeric")
	m.RegisterDefaultPgType([]*decimal.Decimal{}, "_numeric")
}

// numericDecimal 桥接 pgtype.NumericCodec 与 decimal.Decimal。
type numericDecimal decimal.Decimal

// NumericValue 实现 pgtype.NumericValuer。
func (d numericDecimal) NumericValue() (pgtype.Numeric, error) {
	dd := decimal.Decimal(d)
	// Coefficient 返回副本，Numeric 不会反向共享 Decimal 的内部状态。
	return pgtype.Numeric{Int: dd.Coefficient(), Exp: dd.Exponent(), Valid: true}, nil
}

// ScanNumeric 实现 pgtype.NumericScanner。
func (d *numericDecimal) ScanNumeric(v pgtype.Numeric) error {
	if !v.Valid {
		return fmt.Errorf("pgxmoney: NULL 不能扫描进 decimal.Decimal（可空列请用 *decimal.Decimal 目标）")
	}
	if v.NaN {
		return fmt.Errorf("pgxmoney: decimal.Decimal 无法表示 NaN")
	}
	if v.InfinityModifier != pgtype.Finite {
		return fmt.Errorf("pgxmoney: decimal.Decimal 无法表示 %v", v.InfinityModifier)
	}
	coef := v.Int
	if coef == nil {
		coef = new(big.Int)
	}
	*d = numericDecimal(decimal.NewFromBigInt(coef, v.Exp))
	return nil
}

func tryWrapNumericEncodePlan(value any) (pgtype.WrappedEncodePlanNextSetter, any, bool) {
	if v, ok := value.(decimal.Decimal); ok {
		return &wrapEncodePlan{}, numericDecimal(v), true
	}
	return nil, nil, false
}

type wrapEncodePlan struct{ next pgtype.EncodePlan }

func (p *wrapEncodePlan) SetNext(next pgtype.EncodePlan) { p.next = next }

func (p *wrapEncodePlan) Encode(value any, buf []byte) ([]byte, error) {
	b, err := p.next.Encode(numericDecimal(value.(decimal.Decimal)), buf)
	if err != nil {
		return nil, fmt.Errorf("pgxmoney: 编码 NUMERIC: %w", err)
	}
	return b, nil
}

func tryWrapNumericScanPlan(target any) (pgtype.WrappedScanPlanNextSetter, any, bool) {
	if t, ok := target.(*decimal.Decimal); ok {
		return &wrapScanPlan{}, (*numericDecimal)(t), true
	}
	return nil, nil, false
}

type wrapScanPlan struct{ next pgtype.ScanPlan }

func (p *wrapScanPlan) SetNext(next pgtype.ScanPlan) { p.next = next }

func (p *wrapScanPlan) Scan(src []byte, dst any) error {
	if err := p.next.Scan(src, (*numericDecimal)(dst.(*decimal.Decimal))); err != nil {
		return fmt.Errorf("pgxmoney: 扫描 NUMERIC: %w", err)
	}
	return nil
}
