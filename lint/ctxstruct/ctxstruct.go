// Package ctxstruct 检查 struct 字段是否持有 context.Context。
//
// Go 官方明确的反模式：ctx 只应作为函数首参在调用链上传递，
// 存进 struct 会把请求生命周期和对象生命周期搅在一起
// （appkit 的 ctx 防火墙、事务传播都依赖 ctx 只走参数这一前提）。
package ctxstruct

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// Analyzer 是 ctxstruct 检查器。
var Analyzer = &analysis.Analyzer{
	Name:     "ctxstruct",
	Doc:      "禁止 struct 字段类型为 context.Context；ctx 只能作为函数参数传递",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (any, error) {
	ins := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	ins.Preorder([]ast.Node{(*ast.StructType)(nil)}, func(n ast.Node) {
		st := n.(*ast.StructType)
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
	})
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
