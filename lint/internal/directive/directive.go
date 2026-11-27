// Package directive 解析 appkit-lint 统一的行级带理由豁免注释：
//
//	//appkit:ignore <analyzer> -- <理由>
//
// 支持同行尾注释或紧邻上一行注释，也允许逗号分隔多个检查器名
// （例如 //appkit:ignore nogoroutine,authheader -- <理由>）。
// 缺少理由时会在注释行报告诊断，防止静默绕过约束（与 nolintlint 对齐）。
package directive

import (
	"go/ast"
	"go/token"
	"strings"

	"golang.org/x/tools/go/analysis"
)

const prefix = "appkit:ignore"

// Index 记录某检查器在各个文件中被合法豁免的行号集合。
type Index struct {
	lines map[string]map[int]bool
}

// Parse 扫描 files 中针对 analyzerName 的 appkit:ignore 注释。
// 若存在命中 analyzerName 但缺少理由的注释，通过 pass.Reportf 在注释处报错。
func Parse(pass *analysis.Pass, files []*ast.File, analyzerName string) *Index {
	idx := &Index{lines: make(map[string]map[int]bool)}
	for _, f := range files {
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
				if !strings.HasPrefix(text, prefix) {
					continue
				}
				rest := strings.TrimPrefix(text, prefix)
				if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
					continue
				}
				rest = strings.TrimSpace(rest)
				namesPart, reason, hasSep := strings.Cut(rest, "--")
				if !containsAnalyzer(namesPart, analyzerName) {
					continue
				}
				if !hasSep || strings.TrimSpace(reason) == "" {
					pass.Reportf(c.Pos(), "appkit:ignore %s 缺少豁免理由，格式：//appkit:ignore %s -- <理由>", analyzerName, analyzerName)
					continue
				}
				pos := pass.Fset.Position(c.Pos())
				if idx.lines[pos.Filename] == nil {
					idx.lines[pos.Filename] = make(map[int]bool)
				}
				// 同行尾注释覆盖当前行，独立注释行覆盖下一行。
				idx.lines[pos.Filename][pos.Line] = true
				idx.lines[pos.Filename][pos.Line+1] = true
			}
		}
	}
	return idx
}

// Ignored 返回 pos 所在行是否被合法豁免。
func (idx *Index) Ignored(fset *token.FileSet, pos token.Pos) bool {
	if idx == nil || !pos.IsValid() {
		return false
	}
	p := fset.Position(pos)
	return idx.lines[p.Filename][p.Line]
}

func containsAnalyzer(namesPart, target string) bool {
	for raw := range strings.SplitSeq(namesPart, ",") {
		if strings.TrimSpace(raw) == target {
			return true
		}
	}
	return false
}
