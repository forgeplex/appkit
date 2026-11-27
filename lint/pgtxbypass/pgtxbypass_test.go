package pgtxbypass_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/forgeplex/appkit/lint/pgtxbypass"
)

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), pgtxbypass.Analyzer, "a", "skiptests")
}
