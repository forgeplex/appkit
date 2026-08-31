// Package prodfiles 是各检查器共享的「生产代码文件」过滤：-tests 开关
// 关闭（默认）时只检查非 _test.go 文件。
//
// 为什么默认跳过测试文件：这些检查器约束的是线上形态（JSON 面、ctx 流向、
// 金额类型），测试夹具低一档——float64 近似断言半合法，fake 持 ctx 也只是
// 风味问题；而且 domain-ci.yml 经 @main 被全部存量域仓库共享，规则升级
// 不该让它们的测试文件一夜变红。要连测试一起查就显式 -xxx.tests=true。
package prodfiles

import (
	"go/ast"
	"go/token"
	"strings"
)

// Files 返回待检查文件：skipTests 为 true 时剔除 _test.go。
// 返回新切片，不动传入切片的底层数组（那是 pass.Files 的）。
func Files(fset *token.FileSet, files []*ast.File, skipTests bool) []*ast.File {
	if !skipTests {
		return files
	}
	out := make([]*ast.File, 0, len(files))
	for _, f := range files {
		if strings.HasSuffix(fset.Position(f.Package).Filename, "_test.go") {
			continue
		}
		out = append(out, f)
	}
	return out
}
