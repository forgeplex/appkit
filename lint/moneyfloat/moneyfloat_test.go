package moneyfloat_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/forgeplex/appkit/lint/moneyfloat"
)

func TestAnalyzer(t *testing.T) {
	tests := []struct {
		name  string
		scope string
		pkgs  []string
	}{
		{name: "默认检查全部包", scope: "", pkgs: []string{"a"}},
		// scoped/other 内有浮点但无 want 注释：scope 不匹配则必须零报告。
		{name: "scope 圈定业务包", scope: `^scoped/pay$`, pkgs: []string{"scoped/pay", "scoped/other"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := moneyfloat.Analyzer.Flags.Set("scope", tt.scope); err != nil {
				t.Fatalf("设置 -scope=%q 失败: %v", tt.scope, err)
			}
			t.Cleanup(func() {
				if err := moneyfloat.Analyzer.Flags.Set("scope", ""); err != nil {
					t.Fatalf("复位 -scope 失败: %v", err)
				}
			})
			analysistest.Run(t, analysistest.TestData(), moneyfloat.Analyzer, tt.pkgs...)
		})
	}
}

func TestScopeFlag(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "空值合法", value: "", wantErr: false},
		{name: "合法正则", value: `^github\.com/forgeplex/`, wantErr: false},
		{name: "非法正则报错", value: `([`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := moneyfloat.Analyzer.Flags.Set("scope", tt.value)
			t.Cleanup(func() { _ = moneyfloat.Analyzer.Flags.Set("scope", "") })
			if (err != nil) != tt.wantErr {
				t.Fatalf("Set(%q) 错误 = %v，期望出错 = %v", tt.value, err, tt.wantErr)
			}
		})
	}
}
