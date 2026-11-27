// Package canonicalmoney 禁止在 HTTP 入站边界直接使用 decimal.NewFromString /
// decimal.RequireFromString 解析金额字符串。
//
// decimal.NewFromString 接受 "+80"、"080"、"8e1"、"-0" 等非规范形态，会破坏
// 幂等指纹与请求签名的一致性；HTTP 入站（internal/http）必须统一走
// money.ParseCanonical 或 money.ParseCanonicalDecimal。
//
// 默认仅对路径匹配 `/internal/http($|/)` 的包生效（可通过 `-canonicalmoney.scope` 覆盖），
// 且默认只查非生成生产代码（_test.go 与生成文件不查）。
package canonicalmoney

import (
	"go/ast"
	"go/types"
	"regexp"

	"golang.org/x/tools/go/analysis"

	"github.com/forgeplex/appkit/lint/internal/directive"
	"github.com/forgeplex/appkit/lint/internal/prodfiles"
	"github.com/forgeplex/appkit/lint/internal/scopeflag"
)

const decimalPkg = "github.com/shopspring/decimal"

var defaultHTTPScope = regexp.MustCompile(`(^|/)internal/http($|/)`)

// Analyzer 是 canonicalmoney 检查器。
var Analyzer = &analysis.Analyzer{
	Name: "canonicalmoney",
	Doc:  "禁止在 HTTP 入站层（internal/http）直接调用 decimal.NewFromString/RequireFromString；请改用 money.ParseCanonical 或 money.ParseCanonicalDecimal",
	Run:  run,
}

var (
	scope = &scopeflag.RegexFlag{}
	tests bool
)

func init() {
	Analyzer.Flags.Var(scope, "scope", "正则表达式，仅检查 import path 匹配的包；为空时默认匹配 internal/http")
	Analyzer.Flags.BoolVar(&tests, "tests", false, "是否检查 _test.go（默认只查生产代码）")
}

func run(pass *analysis.Pass) (any, error) {
	if scope.String() == "" {
		if !defaultHTTPScope.MatchString(pass.Pkg.Path()) {
			return nil, nil
		}
	} else if !scope.Match(pass.Pkg.Path(), false) {
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
		if !ok || (sel.Sel.Name != "NewFromString" && sel.Sel.Name != "RequireFromString") {
			return true
		}
		fn, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
		if !ok || fn.Pkg() == nil || fn.Pkg().Path() != decimalPkg {
			return true
		}
		if ignores.Ignored(pass.Fset, call.Pos()) {
			return true
		}
		pass.Reportf(sel.Sel.Pos(),
			"禁止在 HTTP 边界直接调用 decimal.%s：请改用 money.ParseCanonical 或 money.ParseCanonicalDecimal 以拒绝非规范金额串", sel.Sel.Name)
		return true
	}
	for _, f := range files {
		ast.Inspect(f, visit)
	}
	return nil, nil
}
