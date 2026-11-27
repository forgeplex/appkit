// Package errcmp 禁止通过 err.Error() 字符串比对做错误分支判定。
//
// appkit 的错误身份是错误码（*apperr.Error），跨层与跨进程判定必须用
// apperr.Is(err, code) 或标准库 errors.Is / errors.As；对 err.Error() 做
// == / != / strings.Contains / HasPrefix / HasSuffix / EqualFold 比对既脆弱
// 又会在文案或包装变化后静默失效（见 AGENTS.md「惯用法」）。
// 日志记录、序列化或拼接输出里的 err.Error() 不受影响。
//
// 默认只查非生成生产代码（_test.go 与生成文件不查），-tests=true 连测试一起查。
package errcmp

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"

	"github.com/forgeplex/appkit/lint/internal/directive"
	"github.com/forgeplex/appkit/lint/internal/prodfiles"
	"github.com/forgeplex/appkit/lint/internal/scopeflag"
)

// Analyzer 是 errcmp 检查器。
var Analyzer = &analysis.Analyzer{
	Name: "errcmp",
	Doc:  "禁止对 error.Error() 结果做字符串比较（== / != / strings.Contains 等）；错误判定请使用 apperr.Is 或 errors.Is / errors.As",
	Run:  run,
}

var (
	scope = &scopeflag.RegexFlag{}
	tests bool
)

func init() {
	Analyzer.Flags.Var(scope, "scope", "正则表达式，仅检查 import path 匹配的包；为空则检查全部包")
	Analyzer.Flags.BoolVar(&tests, "tests", false, "是否检查 _test.go（默认只查生产代码）")
}

var errorIface = types.Universe.Lookup("error").Type().Underlying().(*types.Interface)

var stringCmpFuncs = map[string]bool{
	"Contains":  true,
	"HasPrefix": true,
	"HasSuffix": true,
	"EqualFold": true,
	"Compare":   true,
}

func run(pass *analysis.Pass) (any, error) {
	if !scope.Match(pass.Pkg.Path(), true) {
		return nil, nil
	}
	files := prodfiles.NonGeneratedFiles(pass.Fset, pass.Files, !tests)
	ignores := directive.Parse(pass, files, pass.Analyzer.Name)

	visit := func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.BinaryExpr:
			if node.Op != token.EQL && node.Op != token.NEQ {
				return true
			}
			if !isErrErrorCall(pass, node.X) && !isErrErrorCall(pass, node.Y) {
				return true
			}
			if ignores.Ignored(pass.Fset, node.Pos()) {
				return true
			}
			pass.Reportf(node.Pos(),
				"禁止对 err.Error() 做字符串比对：请改用 apperr.Is(err, code) 或 errors.Is / errors.As")
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok || !isStringsCmpFunc(pass, sel) {
				return true
			}
			for _, arg := range node.Args {
				if isErrErrorCall(pass, arg) {
					if ignores.Ignored(pass.Fset, node.Pos()) {
						return true
					}
					pass.Reportf(node.Pos(),
						"禁止用 strings.%s 对 err.Error() 做字符串比对：请改用 apperr.Is(err, code) 或 errors.Is / errors.As", sel.Sel.Name)
					return true
				}
			}
		}
		return true
	}
	for _, f := range files {
		ast.Inspect(f, visit)
	}
	return nil, nil
}

func isStringsCmpFunc(pass *analysis.Pass, sel *ast.SelectorExpr) bool {
	if !stringCmpFuncs[sel.Sel.Name] {
		return false
	}
	fn, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
	if !ok || fn.Pkg() == nil {
		return false
	}
	return fn.Pkg().Path() == "strings"
}

func isErrErrorCall(pass *analysis.Pass, expr ast.Expr) bool {
	expr = ast.Unparen(expr)
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Error" {
		return false
	}
	t := pass.TypesInfo.TypeOf(sel.X)
	if t == nil {
		return false
	}
	return types.Implements(t, errorIface) || types.Implements(types.NewPointer(t), errorIface)
}
