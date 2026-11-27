package callctxwrite_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/forgeplex/appkit/lint/callctxwrite"
)

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), callctxwrite.Analyzer, "demo/internal/http", "demo/internal/ledger")
}
