package directive_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"

	"github.com/forgeplex/appkit/lint/internal/directive"
)

func TestParseAndIgnored(t *testing.T) {
	src := `package p

func f() {
	//appkit:ignore foo -- 独立注释行合法豁免
	x := 1
	y := 2 //appkit:ignore foo,bar -- 同行尾注释合法豁免
	//appkit:ignore foo
	z := 3
	_, _, _ = x, y, z
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "a.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	var diags []string
	pass := &analysis.Pass{
		Fset:  fset,
		Files: []*ast.File{f},
		Report: func(d analysis.Diagnostic) {
			diags = append(diags, d.Message)
		},
	}

	idx := directive.Parse(pass, pass.Files, "foo")
	if len(diags) != 1 || !strings.Contains(diags[0], "缺少豁免理由") {
		t.Fatalf("diags = %v, want 1 missing-reason diagnostic", diags)
	}

	// 找到 x/y/z 赋值语句的行号并核对豁免状态。
	got := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok {
			got[id.Name] = idx.Ignored(fset, as.Pos())
		}
		return true
	})
	if !got["x"] || !got["y"] || got["z"] {
		t.Fatalf("ignored = %v, want x=true y=true z=false", got)
	}
}
