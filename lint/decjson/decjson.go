// Package decjson 禁止 shopspring/decimal 的类型直接出现在 JSON 序列化面。
//
// 金额的 JSON 形态必须是字符串字段（DTO 层显式转换，DESIGN §2），裸
// decimal 上 JSON 面有两个静默的洞：
//
//   - 出站：decimal.Decimal 的 MarshalJSON 受进程级全局开关
//     MarshalJSONWithoutQuotes 控制——任何一个直接或间接依赖把它翻成
//     true，全进程的金额就静默变成 JSON number，不报错、不 panic；
//   - 入站：decimal.Decimal 的 UnmarshalJSON 接受 JSON number
//     （{"a":0.1} 无错），"永不用 JSON number" 在入站方向形同虚设；
//     number 先经 float64 语义（0.1/0.2 类值在 JS 侧早已失真）。
//
// 规则：带 json tag 的 struct 字段（tag 名非 "-"），其类型表达式里出现
// decimal 包的任何类型（decimal.Decimal、decimal.NullDecimal、指针、
// 切片、map 值等复合形态）即报告。NullDecimal 同样报告——它的
// MarshalJSON 输出的是 JSON number。money.Money 不受影响：它自带
// MarshalJSON/UnmarshalJSON，amount 恒为字符串。
//
// 不带 json tag 的 decimal 字段不查：encoding/json 对导出字段照样序列化，
// 但检查器无法区分"会被 marshal 的类型"与"普通领域类型"，宁缺毋滥；
// 这部分由 DTO 层显式转换的约定承担。
//
// 默认只查生产代码（_test.go 不查），-tests=true 连测试一起查。
package decjson

import (
	"go/ast"
	"go/types"
	"reflect"
	"strings"

	"golang.org/x/tools/go/analysis"

	"github.com/forgeplex/appkit/lint/internal/prodfiles"
)

// decimalPkg 是被禁包。按 import path 而非类型名匹配，用户自定义的
// 同名类型不会误报。
const decimalPkg = "github.com/shopspring/decimal"

// Analyzer 是 decjson 检查器。
var Analyzer = &analysis.Analyzer{
	Name: "decjson",
	Doc:  "禁止 decimal 包类型出现在带 json tag 的 struct 字段上：金额的 JSON 形态必须是字符串（DTO 层显式转换），裸 decimal 出站受 MarshalJSONWithoutQuotes 全局开关影响、入站会接受 JSON number",
	Run:  run,
}

// tests 是 -tests flag：是否连 _test.go 一起检查（默认只查生产代码）。
var tests bool

func init() {
	Analyzer.Flags.BoolVar(&tests, "tests", false, "是否检查 _test.go（默认只查生产代码）")
}

func run(pass *analysis.Pass) (any, error) {
	visit := func(n ast.Node) bool {
		st, ok := n.(*ast.StructType)
		if !ok {
			return true
		}
		for _, f := range st.Fields.List {
			if !jsonTagged(f) {
				continue
			}
			for _, id := range decimalIdents(pass, f.Type) {
				pass.Reportf(id.Pos(),
					"json tag 字段的类型用了 %s：金额上 JSON 面必须用字符串字段（DTO 层显式转换）。"+
						"裸 decimal 出站会被 MarshalJSONWithoutQuotes 全局开关翻成 JSON number，"+
						"入站会接受 JSON number", id.Name)
			}
		}
		return true
	}
	for _, f := range prodfiles.Files(pass.Fset, pass.Files, !tests) {
		ast.Inspect(f, visit)
	}
	return nil, nil
}

// jsonTagged 判定字段是否带有效的 json tag。json:"-" 表示明确不参与
// 序列化，不报告；tag 名为空（仅选项，如 json:",omitempty"）仍算参与。
func jsonTagged(f *ast.Field) bool {
	if f.Tag == nil {
		return false
	}
	tag := reflect.StructTag(strings.Trim(f.Tag.Value, "`"))
	name, ok := tag.Lookup("json")
	if !ok {
		return false
	}
	return strings.Split(name, ",")[0] != "-"
}

// decimalIdents 收集类型表达式里解析到 decimal 包的标识符，
// 覆盖 *decimal.Decimal、[]decimal.Decimal、map[string]decimal.Decimal
// 等复合形态。
func decimalIdents(pass *analysis.Pass, expr ast.Expr) []*ast.Ident {
	var out []*ast.Ident
	ast.Inspect(expr, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		obj := pass.TypesInfo.Uses[id]
		tn, ok := obj.(*types.TypeName)
		if !ok || tn.Pkg() == nil {
			return true
		}
		if tn.Pkg().Path() == decimalPkg {
			out = append(out, id)
		}
		return true
	})
	return out
}
