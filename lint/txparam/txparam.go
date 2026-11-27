// Package txparam 禁止在函数参数或 struct 字段中显式传递或持有 pgx.Tx。
//
// 在 appkit 事务模型中，事务由 tx.Do 回调管理、句柄藏在 context.Context 中，
// 存储层通过 sqlc.New(pgtx.From(ctx, s.pool)) 自动感知当前事务（DESIGN §5.5）。
// 显式传递 pgx.Tx 会破坏统一抽象并绕过租户/分区作用域守卫。
//
// 默认只查非生成生产代码（_test.go 与生成文件不查），-tests=true 连测试一起查。
package txparam

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"

	"github.com/forgeplex/appkit/lint/internal/directive"
	"github.com/forgeplex/appkit/lint/internal/prodfiles"
	"github.com/forgeplex/appkit/lint/internal/scopeflag"
)

const pgxPkg = "github.com/jackc/pgx/v5"

// Analyzer 是 txparam 检查器。
var Analyzer = &analysis.Analyzer{
	Name: "txparam",
	Doc:  "禁止在函数参数或 struct 字段中传递或持有 pgx.Tx；事务句柄须经 tx.Do 放入 ctx 并由 pgtx.From(ctx, pool) 获取",
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

	checkField := func(label string, f *ast.Field) {
		if !containsPgxTx(pass.TypesInfo.TypeOf(f.Type)) {
			return
		}
		if ignores.Ignored(pass.Fset, f.Pos()) {
			return
		}
		pass.Reportf(f.Type.Pos(),
			"%s禁止出现 pgx.Tx：事务句柄必须经 tx.Do 藏在 ctx 中，通过 pgtx.From(ctx, s.pool) 自动感知", label)
	}

	visit := func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncType:
			if node.Params != nil {
				for _, f := range node.Params.List {
					checkField("函数参数", f)
				}
			}
		case *ast.StructType:
			if node.Fields != nil {
				for _, f := range node.Fields.List {
					checkField("struct 字段", f)
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

func containsPgxTx(t types.Type) bool {
	if t == nil {
		return false
	}
	t = types.Unalias(t)
	switch u := t.(type) {
	case *types.Named:
		obj := u.Obj()
		return obj.Pkg() != nil && obj.Pkg().Path() == pgxPkg && obj.Name() == "Tx"
	case *types.Pointer:
		return containsPgxTx(u.Elem())
	case *types.Slice:
		return containsPgxTx(u.Elem())
	case *types.Array:
		return containsPgxTx(u.Elem())
	case *types.Map:
		return containsPgxTx(u.Key()) || containsPgxTx(u.Elem())
	default:
		return false
	}
}
