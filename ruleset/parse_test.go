package ruleset_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/forgeplex/appkit/ruleset"
)

func TestParseAppConfigFileParity(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{"valid", validAppkitYML, ""},
		{"explicit domain", "kind: domain\n" + validAppkitYML, ""},
		{"optional contracts", "version: 1\ndomain: auth\nmodule: example.com/auth\n", ""},
		{"partitioned", validAppkitYML + "partitioned: true\n", ""},
		{"allowRequires", strings.Replace(validAppkitYML, "allowRequires: []", "allowRequires: [example.com/provider]", 1), ""},
		{"empty", "", "EOF"},
		{"malformed yaml", "version: [\n", "解析"},
		{"scalar yaml", "value\n", "解析"},
		{"duplicate field", validAppkitYML + "version: 1\n", "解析"},
		{"unknown field", validAppkitYML + "unknown: true\n", "field unknown not found"},
		{"wrong field type", strings.Replace(validAppkitYML, "version: 1", "version: invalid", 1), "解析"},
		{"missing version", strings.TrimPrefix(validAppkitYML, "version: 1\n"), "version 须为 1，当前为 0"},
		{"wrong version", strings.Replace(validAppkitYML, "version: 1", "version: 2", 1), "version 须为 1，当前为 2"},
		{"system kind", "kind: system\n" + validAppkitYML, "仅适用于域仓库"},
		{"unknown kind", "kind: infra\n" + validAppkitYML, "kind \"infra\" 非法"},
		{"invalid domain", strings.Replace(validAppkitYML, "domain: ledger", "domain: Ledger", 1), "不合法"},
		{"reserved domain", strings.Replace(validAppkitYML, "domain: ledger", "domain: postgres", 1), "保留名"},
		{"missing module", "version: 1\ndomain: auth\n", "module 不能为空"},
		// LoadAppConfig 历来只解析首个文档；纯解析入口不得悄悄收紧输入语义。
		{"following document unchanged", validAppkitYML + "---\nunknown: true\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeAppkitYML(t, dir, tt.content)
			fileConfig, fileErr := ruleset.LoadAppConfig(dir)
			dataConfig, dataErr := ruleset.ParseAppConfig([]byte(tt.content))
			if !reflect.DeepEqual(fileConfig, dataConfig) {
				t.Fatalf("file config %+v differs from bytes %+v", fileConfig, dataConfig)
			}
			if tt.wantErr == "" {
				if fileErr != nil || dataErr != nil {
					t.Fatalf("want valid config; file error %v, bytes error %v", fileErr, dataErr)
				}
				return
			}
			if fileErr == nil || dataErr == nil {
				t.Fatalf("want %q; file error %v, bytes error %v", tt.wantErr, fileErr, dataErr)
			}
			if !strings.Contains(dataErr.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", dataErr, tt.wantErr)
			}
			// 诊断只有来源路径的差别；文件入口保留原有绝对/相对路径。
			wantDataError := strings.Replace(fileErr.Error(), filepath.Join(dir, ".appkit.yml"), ".appkit.yml", 1)
			if dataErr.Error() != wantDataError {
				t.Errorf("file and byte diagnostics differ: %q vs %q", wantDataError, dataErr)
			}
			if !reflect.DeepEqual(dataConfig, ruleset.AppConfig{}) {
				t.Errorf("invalid input returned partial configuration: %+v", dataConfig)
			}
		})
	}
}

func TestParseAppConfigHasNoFileDependency(t *testing.T) {
	data := []byte(validAppkitYML)
	before := string(data)
	cfg, err := ruleset.ParseAppConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Domain != "ledger" || cfg.Module != "github.com/forgeplex/ledger" {
		t.Fatalf("unexpected parsed config: %+v", cfg)
	}
	if string(data) != before {
		t.Fatal("ParseAppConfig mutated its input")
	}
}
