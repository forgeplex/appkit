// Package ctxstruct 检查 struct 字段是否持有 context.Context。
//
// Go 官方明确的反模式：ctx 只应作为函数首参在调用链上传递，
// 存进 struct 会把请求生命周期和对象生命周期搅在一起
// （appkit 的 ctx 防火墙、事务传播都依赖 ctx 只走参数这一前提）。
//
// 默认只查生产代码（_test.go 不查），-tests=true 连测试一起查。
package ctxstruct

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"

	"github.com/forgeplex/appkit/lint/internal/prodfiles"
)

// Analyzer 是 ctxstruct 检查器。
var Analyzer = &analysis.Analyzer{
	Name: "ctxstruct",
	Doc:  "禁止 struct 字段类型为 context.Context；ctx 只能作为函数参数传递",
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
			if !isContext(pass.TypesInfo.TypeOf(f.Type)) {
				continue
			}
			if len(f.Names) == 0 {
				// 内嵌字段。
				pass.Reportf(f.Type.Pos(), "struct 内嵌 context.Context：ctx 只能作为函数参数传递，不要存进 struct")
				continue
			}
			for _, name := range f.Names {
				pass.Reportf(name.Pos(), "struct 字段 %s 的类型是 context.Context：ctx 只能作为函数参数传递，不要存进 struct", name.Name)
			}
		}
		return true
	}
	for _, f := range prodfiles.Files(pass.Fset, pass.Files, !tests) {
		ast.Inspect(f, visit)
	}
	return nil, nil
}

// isContext 判断类型是否为 context.Context（穿透别名）。
func isContext(t types.Type) bool {
	if t == nil {
		return false
	}
	named, ok := types.Unalias(t).(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj.Pkg() != nil && obj.Pkg().Path() == "context" && obj.Name() == "Context"
}
