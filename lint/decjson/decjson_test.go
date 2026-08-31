package decjson_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/forgeplex/appkit/lint/decjson"
)

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), decjson.Analyzer, "a")
}

// TestSkipTests 默认只查生产代码：skiptests 包的违规只在 _test.go 里且无
// want 注释——若默认行为被改坏（开始查测试文件），会冒出未期望的诊断。
func TestSkipTests(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), decjson.Analyzer, "skiptests")
}

// TestIncludeTests -tests=true 时测试文件也要查（withtests 的违规带 want）。
func TestIncludeTests(t *testing.T) {
	if err := decjson.Analyzer.Flags.Set("tests", "true"); err != nil {
		t.Fatalf("设置 -tests=true 失败: %v", err)
	}
	t.Cleanup(func() {
		if err := decjson.Analyzer.Flags.Set("tests", "false"); err != nil {
			t.Fatalf("复位 -tests 失败: %v", err)
		}
	})
	analysistest.Run(t, analysistest.TestData(), decjson.Analyzer, "withtests")
}
