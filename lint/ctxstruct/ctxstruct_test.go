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
