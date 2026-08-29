// Package moneyfloat 检查 float32/float64 在业务代码中的出现。
//
// 报告位置：struct 字段类型、变量/常量声明类型、函数参数与返回值、显式类型转换。
// 金额与业务数值必须用 money.Money 或 decimal 表示（见 appkit docs/DESIGN.md §7）。
//
// 本检查器故意不区分"金额"与"合法的数学计算"——time 换算等场景里的 float
// 同样会被报告。用 -scope 正则把检查圈定在业务包（如 internal/ledger），
// 而不是给检查器加豁免启发式。
package moneyfloat

import (
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"regexp"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// Analyzer 是 moneyfloat 检查器。
var Analyzer = &analysis.Analyzer{
	Name:     "moneyfloat",
	Doc:      "禁止 float32/float64 出现在 struct 字段、变量/常量声明、函数参数与返回值、显式类型转换中；金额与业务数值改用 money.Money 或 decimal",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

// scope 是 -scope flag：正则匹配包 import path 才检查；空 = 检查全部包。
var scope = &regexFlag{}

func init() {
	Analyzer.Flags.Var(scope, "scope", "正则表达式，仅检查 import path 匹配的包；为空则检查全部包")
}

// regexFlag 在 flag 解析期即校验正则，非法值直接报错而不是运行期崩溃。
type regexFlag struct {
	re *regexp.Regexp
}

var _ flag.Value = (*regexFlag)(nil)

func (f *regexFlag) String() string {
	if f == nil || f.re == nil {
		return ""
	}
	return f.re.String()
}

func (f *regexFlag) Set(s string) error {
	if s == "" {
		f.re = nil
		return nil
	}
	re, err := regexp.Compile(s)
	if err != nil {
		return fmt.Errorf("-scope 正则无效: %w", err)
	}
	f.re = re
	return nil
}

// floatObjs 是 universe 作用域里的 float32/float64 类型对象。
// 按对象而非按名字比较，用户自定义的同名类型不会误报。
var floatObjs = map[types.Object]bool{
	types.Universe.Lookup("float32"): true,
	types.Universe.Lookup("float64"): true,
}

func run(pass *analysis.Pass) (any, error) {
	if scope.re != nil && !scope.re.MatchString(pass.Pkg.Path()) {
		return nil, nil
	}
	ins := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	// 同一 ident 可能被多个语法上下文覆盖（如 struct 字段是函数类型），按位置去重。
	reported := map[token.Pos]bool{}
	report := func(label string, expr ast.Expr) {
		for _, id := range floatIdents(pass, expr) {
			if reported[id.Pos()] {
				continue
			}
			reported[id.Pos()] = true
			pass.Reportf(id.Pos(), "%s使用 %s：金额与业务数值禁用浮点，改用 money.Money 或 decimal.Decimal", label, id.Name)
		}
	}

	filter := []ast.Node{
		(*ast.StructType)(nil),
		(*ast.ValueSpec)(nil),
		(*ast.FuncType)(nil),
		(*ast.CallExpr)(nil),
	}
	ins.Preorder(filter, func(n ast.Node) {
		switch n := n.(type) {
		case *ast.StructType:
			for _, f := range n.Fields.List {
				report("struct 字段", f.Type)
			}
		case *ast.ValueSpec:
			if n.Type != nil {
				report("变量/常量声明", n.Type)
			}
		case *ast.FuncType:
			if n.Params != nil {
				for _, f := range n.Params.List {
					report("函数参数或返回值", f.Type)
				}
			}
			if n.Results != nil {
				for _, f := range n.Results.List {
					report("函数参数或返回值", f.Type)
				}
			}
		case *ast.CallExpr:
			// 显式类型转换：Fun 位置是类型而非函数。
			if tv, ok := pass.TypesInfo.Types[n.Fun]; ok && tv.IsType() {
				report("类型转换", n.Fun)
			}
		}
	})
	return nil, nil
}

// floatIdents 收集类型表达式里解析为内建 float32/float64 的标识符，
// 覆盖 []float64、map[string]float64、*float64、func(float64) 等复合形态。
func floatIdents(pass *analysis.Pass, expr ast.Expr) []*ast.Ident {
	var out []*ast.Ident
	ast.Inspect(expr, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		if floatObjs[pass.TypesInfo.Uses[id]] {
			out = append(out, id)
		}
		return true
	})
	return out
}
