package jsonany_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/forgeplex/appkit/lint/jsonany"
)

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), jsonany.Analyzer, "a", "skiptests")
}
