package canonicalmoney_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/forgeplex/appkit/lint/canonicalmoney"
)

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), canonicalmoney.Analyzer, "demo/internal/http", "demo/internal/postgres")
}
