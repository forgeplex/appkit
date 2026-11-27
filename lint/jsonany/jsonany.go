// Package jsonany 禁止 encoding/json.Unmarshal 直接解码到 any / map[string]any / []any。
//
// Go 标准库 json.Unmarshal 把 JSON number 解进空接口时一律落成 float64，
// 会静默绕过 moneyfloat / decjson 对金额与业务数值的精度防护；同时丢失
// 结构契约校验。请解码到具名 DTO struct（金额字段为 string + money.ParseCanonical），
// 或使用 json.NewDecoder 配合 UseNumber()。
//
// 默认只查非生成生产代码（_test.go 与生成文件不查），-tests=true 连测试一起查。
// 确需动态 JSON 结构时，用 //appkit:ignore jsonany -- <理由> 显式豁免。
package jsonany

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"

	"github.com/forgeplex/appkit/lint/internal/directive"
	"github.com/forgeplex/appkit/lint/internal/prodfiles"
	"github.com/forgeplex/appkit/lint/internal/scopeflag"
)

// Analyzer 是 jsonany 检查器。
var Analyzer = &analysis.Analyzer{
	Name: "jsonany",
	Doc:  "禁止 json.Unmarshal 直接解码到 any / map[string]any / []any（JSON number 会静默转成 float64）；请改用具名 DTO struct 或 json.Decoder.UseNumber",
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

func run(pass *analysis.Pass) (any, error) {
	if !scope.Match(pass.Pkg.Path(), true) {
		return nil, nil
	}
	files := prodfiles.NonGeneratedFiles(pass.Fset, pass.Files, !tests)
	ignores := directive.Parse(pass, files, pass.Analyzer.Name)

	visit := func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !isJSONUnmarshal(pass, sel) {
			return true
		}
		targetType := pass.TypesInfo.TypeOf(call.Args[1])
		if !isDynamicAnyTarget(targetType) {
			return true
		}
		if ignores.Ignored(pass.Fset, call.Pos()) {
			return true
		}
		pass.Reportf(call.Args[1].Pos(),
			"禁止 json.Unmarshal 直接解码到动态类型 %s（JSON number 会静默转成 float64）：请改用具名 DTO struct，或用 json.NewDecoder 配合 UseNumber()", targetType)
		return true
	}
	for _, f := range files {
		ast.Inspect(f, visit)
	}
	return nil, nil
}

func isJSONUnmarshal(pass *analysis.Pass, sel *ast.SelectorExpr) bool {
	if sel.Sel.Name != "Unmarshal" {
		return false
	}
	fn, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
	if !ok || fn.Pkg() == nil {
		return false
	}
	return fn.Pkg().Path() == "encoding/json"
}

// isDynamicAnyTarget 判断传给 json.Unmarshal 的目标指针是否指向 any /
// map[K]any / []any（穿透类型别名与单层指针）。
func isDynamicAnyTarget(t types.Type) bool {
	if t == nil {
		return false
	}
	t = types.Unalias(t)
	if ptr, ok := t.(*types.Pointer); ok {
		t = types.Unalias(ptr.Elem())
	}
	switch u := t.Underlying().(type) {
	case *types.Interface:
		return u.NumMethods() == 0
	case *types.Map:
		return isEmptyInterface(u.Elem())
	case *types.Slice:
		return isEmptyInterface(u.Elem())
	default:
		return false
	}
}

func isEmptyInterface(t types.Type) bool {
	iface, ok := types.Unalias(t).Underlying().(*types.Interface)
	return ok && iface.NumMethods() == 0
}
