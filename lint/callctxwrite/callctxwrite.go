// Package callctxwrite 禁止在 HTTP transport 层（internal/http）直接调用 callctx.With
// 覆写请求上下文中的身份元数据（TenantID / Partition / Caller）。
//
// 认证请求的身份元数据只能由组合根挂接的 authn 中间件从已验签令牌重建；
// 若管理面用例确需代目标租户执行操作，应在完成权限码校验后的业务层中显式处理。
//
// 默认仅对路径匹配 `/internal/http($|/)` 的包生效（可通过 `-callctxwrite.scope` 覆盖），
// 且默认只查非生成生产代码（_test.go 与生成文件不查）。
package callctxwrite

import (
	"go/ast"
	"go/types"
	"regexp"

	"golang.org/x/tools/go/analysis"

	"github.com/forgeplex/appkit/lint/internal/directive"
	"github.com/forgeplex/appkit/lint/internal/prodfiles"
	"github.com/forgeplex/appkit/lint/internal/scopeflag"
)

const callctxPkg = "github.com/forgeplex/appkit/callctx"

var defaultHTTPScope = regexp.MustCompile(`(^|/)internal/http($|/)`)

// Analyzer 是 callctxwrite 检查器。
var Analyzer = &analysis.Analyzer{
	Name: "callctxwrite",
	Doc:  "禁止在 HTTP 层（internal/http）直接调用 callctx.With 覆写身份元数据；身份只能由 authn 中间件从已验证凭证重建",
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
		if !ok || sel.Sel.Name != "With" {
			return true
		}
		fn, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
		if !ok || fn.Pkg() == nil || fn.Pkg().Path() != callctxPkg {
			return true
		}
		if ignores.Ignored(pass.Fset, call.Pos()) {
			return true
		}
		pass.Reportf(sel.Sel.Pos(),
			"禁止在 HTTP 层直接调用 callctx.With 覆写身份上下文：身份应由 authn 中间件重建，受权代租户操作请下沉至业务层")
		return true
	}
	for _, f := range files {
		ast.Inspect(f, visit)
	}
	return nil, nil
}
