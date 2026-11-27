// Package pgtxbypass 禁止绕过 pgtx.From(ctx, pool) 直接在 *pgxpool.Pool 上执行 SQL。
//
// 在 appkit 架构中，事务句柄（tx.Do）、分区域域的 SET LOCAL search_path 与租户域的
// SET LOCAL app.tenant_id 都绑定在事务上下文中；业务数据访问必须走
// sqlc.New(pgtx.From(ctx, s.pool))（见 AGENTS.md 与 DESIGN §5.5/§8）。
// 直接在 *pgxpool.Pool 上调用 Query / QueryRow / Exec / SendBatch / CopyFrom
// 会静默绕过当前事务、分区路由与 RLS 策略。
// Ping / Begin / BeginTx / Acquire / Close / Stat / Config 等连接池管理方法不受限制。
//
// 默认只查非生成生产代码（_test.go 与生成文件不查），-tests=true 连测试一起查。
// 确需直连连接池（如建库工具/基础设施初始化）时，用 //appkit:ignore pgtxbypass -- <理由> 豁免。
package pgtxbypass

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"

	"github.com/forgeplex/appkit/lint/internal/directive"
	"github.com/forgeplex/appkit/lint/internal/prodfiles"
	"github.com/forgeplex/appkit/lint/internal/scopeflag"
)

const pgxpoolPkg = "github.com/jackc/pgx/v5/pgxpool"

var queryMethods = map[string]bool{
	"Exec":      true,
	"Query":     true,
	"QueryRow":  true,
	"SendBatch": true,
	"CopyFrom":  true,
}

// Analyzer 是 pgtxbypass 检查器。
var Analyzer = &analysis.Analyzer{
	Name: "pgtxbypass",
	Doc:  "禁止直接调用 (*pgxpool.Pool).Query/QueryRow/Exec/SendBatch/CopyFrom；请经 pgtx.From(ctx, pool) 执行以保留事务、分区路由与租户 RLS 上下文",
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
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !queryMethods[sel.Sel.Name] {
			return true
		}
		if !isPgxPoolMethod(pass, sel) {
			return true
		}
		if ignores.Ignored(pass.Fset, call.Pos()) {
			return true
		}
		pass.Reportf(sel.Sel.Pos(),
			"禁止绕过 pgtx.From(ctx, pool) 直接调用 (*pgxpool.Pool).%s：直接查连接池会逃出事务、分区 search_path 与租户 RLS 上下文", sel.Sel.Name)
		return true
	}
	for _, f := range files {
		ast.Inspect(f, visit)
	}
	return nil, nil
}

func isPgxPoolMethod(pass *analysis.Pass, sel *ast.SelectorExpr) bool {
	fn, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
	if !ok {
		return false
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return false
	}
	recv := types.Unalias(sig.Recv().Type())
	if ptr, ok := recv.(*types.Pointer); ok {
		recv = types.Unalias(ptr.Elem())
	}
	named, ok := recv.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj.Pkg() != nil && obj.Pkg().Path() == pgxpoolPkg && obj.Name() == "Pool"
}
