// Package nogoroutine 禁止生产代码直接使用裸 `go` 语句启动协程。
//
// 在 appkit 域服务中，长驻或异步后台任务应通过 reg.Worker(name, run) 托管：
// 框架统一负责关停等待、超时控制与异常退出上报；裸 `go func()` 容易泄漏、
// 吞掉错误或在进程退出时被硬截断（见 AGENTS.md「惯用法」与 DESIGN §7）。
//
// 默认只查非生成生产代码（_test.go 与生成文件不查），-tests=true 连测试一起查。
// 确需并发 fan-out 等有界场景时，用 //appkit:ignore nogoroutine -- <理由> 显式豁免。
package nogoroutine

import (
	"go/ast"

	"golang.org/x/tools/go/analysis"

	"github.com/forgeplex/appkit/lint/internal/directive"
	"github.com/forgeplex/appkit/lint/internal/prodfiles"
	"github.com/forgeplex/appkit/lint/internal/scopeflag"
)

// Analyzer 是 nogoroutine 检查器。
var Analyzer = &analysis.Analyzer{
	Name: "nogoroutine",
	Doc:  "禁止生产代码直接使用裸 go 语句；后台任务请改用 reg.Worker 托管生命周期",
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
		gs, ok := n.(*ast.GoStmt)
		if !ok {
			return true
		}
		if ignores.Ignored(pass.Fset, gs.Pos()) {
			return true
		}
		pass.Reportf(gs.Go,
			"禁止直接使用裸 go 语句：后台任务请改用 reg.Worker 托管（关停等待/异常上报）；若为有界并发请加 //appkit:ignore nogoroutine -- <理由>")
		return true
	}
	for _, f := range files {
		ast.Inspect(f, visit)
	}
	return nil, nil
}
