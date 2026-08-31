package ctxstruct_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/forgeplex/appkit/lint/ctxstruct"
)

func TestAnalyzer(t *testing.T) {
	tests := []struct {
		name string
		pkgs []string
	}{
		{name: "字段与内嵌正反例", pkgs: []string{"a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analysistest.Run(t, analysistest.TestData(), ctxstruct.Analyzer, tt.pkgs...)
		})
	}
}

// TestSkipTests 默认只查生产代码：skiptests 包的违规只在 _test.go 里且无
// want 注释——若默认行为被改坏（开始查测试文件），会冒出未期望的诊断。
func TestSkipTests(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), ctxstruct.Analyzer, "skiptests")
}
