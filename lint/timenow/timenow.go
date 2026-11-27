// Package timenow 在显式圈定的核心业务包中禁止直接调用 time.Now()。
//
// 账务、计费、过期判定等需要确定性单测的核心业务包应注入时钟（now func() time.Time），
// 而不是散落调用全局 time.Now()。
//
// 注意：为避免对日志/基础设施/普通工具包产生噪音，本检查器默认关闭（-scope 为空时不检查），
// 仅在显式指定 `-timenow.scope=<正则>` 时对匹配的包生效（如
// `-timenow.scope='^github\.com/forgeplex/ledger/internal/ledger'`）。
//
// 默认只查非生成生产代码（_test.go 与生成文件不查），-tests=true 连测试一起查。
package timenow

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"

	"github.com/forgeplex/appkit/lint/internal/directive"
	"github.com/forgeplex/appkit/lint/internal/prodfiles"
	"github.com/forgeplex/appkit/lint/internal/scopeflag"
)

// Analyzer 是 timenow 检查器。
var Analyzer = &analysis.Analyzer{
	Name: "timenow",
	Doc:  "在 -timenow.scope 圈定的核心业务包中禁止直接调用 time.Now()，强制注入时钟以保证确定性单测（-scope 为空时默认不启用）",
	Run:  run,
}

var (
	scope = &scopeflag.RegexFlag{}
	tests bool
)

func init() {
	Analyzer.Flags.Var(scope, "scope", "正则表达式，仅检查 import path 匹配的包；为空时本检查器不启用（opt-in）")
	Analyzer.Flags.BoolVar(&tests, "tests", false, "是否检查 _test.go（默认只查生产代码）")
}

func run(pass *analysis.Pass) (any, error) {
	// opt-in：未显式指定 -timenow.scope 时不报告，避免全仓误报。
	if !scope.Match(pass.Pkg.Path(), false) {
		return nil, nil
	}
	files := prodfiles.NonGeneratedFiles(pass.Fset, pass.Files, !tests)
	ignores := directive.Parse(pass, files, pass.Analyzer.Name)

	visit := func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Now" {
			return true
		}
		fn, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
		if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "time" {
			return true
		}
		if ignores.Ignored(pass.Fset, call.Pos()) {
			return true
		}
		pass.Reportf(sel.Sel.Pos(),
			"禁止在受控业务包中直接调用 time.Now()：请通过注入时钟（now func() time.Time）保证确定性测试")
		return true
	}
	for _, f := range files {
		ast.Inspect(f, visit)
	}
	return nil, nil
}
