// Package authheader 禁止业务代码直接从 HTTP 请求头读取受框架保护的身份与凭证头。
//
// 在 appkit 信任模型中，验签与身份重建只在 authn/严格安全边界完成：
// 用户身份应从 appkit.ActorFrom(ctx) 获取，租户与分区应从 callctx.From(ctx) 获取。
// 业务或 transport 代码直接读 Authorization / X-Step-Up / X-Tenant-Id /
// X-Partition / X-Caller 等头会绕过验签或引入 unsigned 头伪造面（DESIGN §7）。
//
// 默认只查非生成生产代码（_test.go 与生成文件不查），-tests=true 连测试一起查。
// 确需读取的场景（如网关/鉴权提供方本身）可用 //appkit:ignore authheader -- <理由> 豁免。
package authheader

import (
	"go/ast"
	"go/constant"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"

	"github.com/forgeplex/appkit/lint/internal/directive"
	"github.com/forgeplex/appkit/lint/internal/prodfiles"
	"github.com/forgeplex/appkit/lint/internal/scopeflag"
)

// Analyzer 是 authheader 检查器。
var Analyzer = &analysis.Analyzer{
	Name: "authheader",
	Doc:  "禁止直接从 http.Header 读取受保护的身份/凭证头（Authorization / X-Step-Up / X-Tenant-Id / X-Partition / X-Caller 等）；请改用 appkit.ActorFrom(ctx) 或 callctx.From(ctx)",
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

// protectedHeaders 是由框架身份边界与 authn 接管的请求头（全小写）。
var protectedHeaders = map[string]bool{
	"authorization": true,
	"x-step-up":     true,
	"x-tenant-id":   true,
	"x-partition":   true,
	"x-caller":      true,
	"x-actor-id":    true,
	"x-actor-type":  true,
	"x-actor-perms": true,
}

func run(pass *analysis.Pass) (any, error) {
	if !scope.Match(pass.Pkg.Path(), true) {
		return nil, nil
	}
	files := prodfiles.NonGeneratedFiles(pass.Fset, pass.Files, !tests)
	ignores := directive.Parse(pass, files, pass.Analyzer.Name)

	visit := func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Get" && sel.Sel.Name != "Values") || len(node.Args) != 1 {
				return true
			}
			if !isHTTPHeader(pass.TypesInfo.TypeOf(sel.X)) {
				return true
			}
			key, ok := constString(pass, node.Args[0])
			if !ok || !protectedHeaders[strings.ToLower(strings.TrimSpace(key))] {
				return true
			}
			if ignores.Ignored(pass.Fset, node.Pos()) {
				return true
			}
			pass.Reportf(node.Args[0].Pos(),
				"禁止直接读取身份/凭证请求头 %q：请改用 appkit.ActorFrom(ctx) 或 callctx.From(ctx)（避免绕过 authn 验签或信任 unsigned 头）", key)
		case *ast.IndexExpr:
			if !isHTTPHeader(pass.TypesInfo.TypeOf(node.X)) {
				return true
			}
			key, ok := constString(pass, node.Index)
			if !ok || !protectedHeaders[strings.ToLower(strings.TrimSpace(key))] {
				return true
			}
			if ignores.Ignored(pass.Fset, node.Pos()) {
				return true
			}
			pass.Reportf(node.Index.Pos(),
				"禁止直接读取身份/凭证请求头 %q：请改用 appkit.ActorFrom(ctx) 或 callctx.From(ctx)（避免绕过 authn 验签或信任 unsigned 头）", key)
		}
		return true
	}
	for _, f := range files {
		ast.Inspect(f, visit)
	}
	return nil, nil
}

func isHTTPHeader(t types.Type) bool {
	if t == nil {
		return false
	}
	t = types.Unalias(t)
	if ptr, ok := t.(*types.Pointer); ok {
		t = types.Unalias(ptr.Elem())
	}
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj.Pkg() != nil && obj.Pkg().Path() == "net/http" && obj.Name() == "Header"
}

func constString(pass *analysis.Pass, expr ast.Expr) (string, bool) {
	tv, ok := pass.TypesInfo.Types[expr]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}
