package appkit_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestRootAndTxZeroThirdPartyDeps 守住 AGENTS.md 不可破坏承诺 #2：
// 根包（appkit）只依赖标准库与本仓无驱动子包（apperr/callctx/health），
// tx 包只依赖标准库——API 面不得引入 pgx / gin / otel 等第三方驱动。
func TestRootAndTxZeroThirdPartyDeps(t *testing.T) {
	cases := []struct {
		name         string
		dir          string
		allowedLocal map[string]bool
	}{
		{
			name: "root package",
			dir:  ".",
			allowedLocal: map[string]bool{
				"github.com/forgeplex/appkit/apperr":             true,
				"github.com/forgeplex/appkit/callctx":            true,
				"github.com/forgeplex/appkit/health":             true,
				"github.com/forgeplex/appkit/internal/hoststate": true,
				"github.com/forgeplex/appkit/internal/metrics":   true,
			},
		},
		{
			name:         "tx package",
			dir:          "tx",
			allowedLocal: map[string]bool{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries, err := os.ReadDir(tc.dir)
			if err != nil {
				t.Fatal(err)
			}
			fset := token.NewFileSet()
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
					continue
				}
				full := filepath.Join(tc.dir, e.Name())
				f, err := parser.ParseFile(fset, full, nil, parser.ImportsOnly)
				if err != nil {
					t.Fatalf("parse %s: %v", full, err)
				}
				for _, spec := range f.Imports {
					imp, err := strconv.Unquote(spec.Path.Value)
					if err != nil {
						continue
					}
					// 标准库路径的第一段不含点号（"."）。
					firstSeg, _, _ := strings.Cut(imp, "/")
					if !strings.Contains(firstSeg, ".") {
						continue
					}
					if tc.allowedLocal[imp] {
						continue
					}
					t.Errorf("%s 禁止依赖第三方或重型内部包 %q（AGENTS.md 铁律 #2）", full, imp)
				}
			}
		})
	}
}
