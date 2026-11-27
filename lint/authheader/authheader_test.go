package authheader_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/forgeplex/appkit/lint/authheader"
)

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), authheader.Analyzer, "a", "skiptests")
}
