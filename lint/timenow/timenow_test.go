package timenow_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/forgeplex/appkit/lint/timenow"
)

func TestDefaultDisabled(t *testing.T) {
	// 默认 -scope 为空时不启用，避免误报普通包。
	analysistest.Run(t, analysistest.TestData(), timenow.Analyzer, "unscoped")
}

func TestScoped(t *testing.T) {
	if err := timenow.Analyzer.Flags.Set("scope", `^scoped/billing$`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = timenow.Analyzer.Flags.Set("scope", "") })
	analysistest.Run(t, analysistest.TestData(), timenow.Analyzer, "scoped/billing", "scoped/other")
}
