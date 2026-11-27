package txparam_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/forgeplex/appkit/lint/txparam"
)

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), txparam.Analyzer, "a")
}
