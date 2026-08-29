package archcheck_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forgeplex/appkit/internal/archcheck"
)

// writeRepo 在 t.TempDir 里按相对路径写出 fixture 仓库。
func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	return dir
}

// wantV 是期望的违规：File 精确匹配，Line 为 0 表示不校验行号，Msg 为子串匹配。
type wantV struct {
	File string
	Line int
	Msg  string
}

func assertViolations(t *testing.T, got []archcheck.Violation, want []wantV) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("违规数 = %d，期望 %d\n实际: %v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.File != w.File {
			t.Errorf("[%d] File = %q，期望 %q", i, g.File, w.File)
		}
		if w.Line != 0 && g.Line != w.Line {
			t.Errorf("[%d] Line = %d，期望 %d（%s）", i, g.Line, w.Line, g.Msg)
		}
		if !strings.Contains(g.Msg, w.Msg) {
			t.Errorf("[%d] Msg = %q，期望包含 %q", i, g.Msg, w.Msg)
		}
	}
}

const domainYML = `version: 1
domain: ledger
module: github.com/forgeplex/ledger
contracts: github.com/forgeplex/psp-contracts/go
allowRequires: []
`

const ledgerGoMod = "module github.com/forgeplex/ledger\n\ngo 1.26\n"

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		wantErr string // 空串表示成功
		check   func(t *testing.T, cfg *archcheck.Config)
	}{
		{
			name:    "缺失文件报错并提示先创建",
			files:   map[string]string{},
			wantErr: "未找到 .appkit.yml",
		},
		{
			name:  "domain 仓库正常解析",
			files: map[string]string{".appkit.yml": domainYML},
			check: func(t *testing.T, cfg *archcheck.Config) {
				if cfg.Kind != archcheck.KindDomain || cfg.Domain != "ledger" ||
					cfg.Module != "github.com/forgeplex/ledger" ||
					cfg.Contracts != "github.com/forgeplex/psp-contracts/go" {
					t.Errorf("cfg = %+v", cfg)
				}
			},
		},
		{
			name:    "version 非 1 报错",
			files:   map[string]string{".appkit.yml": "version: 2\ndomain: ledger\nmodule: m\n"},
			wantErr: "version",
		},
		{
			name:    "domain 大写非法",
			files:   map[string]string{".appkit.yml": "version: 1\ndomain: Ledger\nmodule: m\n"},
			wantErr: "domain",
		},
		{
			name:    "domain 数字开头非法",
			files:   map[string]string{".appkit.yml": "version: 1\ndomain: 1edger\nmodule: m\n"},
			wantErr: "domain",
		},
		{
			name:    "domain 含连字符非法",
			files:   map[string]string{".appkit.yml": "version: 1\ndomain: led-ger\nmodule: m\n"},
			wantErr: "domain",
		},
		{
			name:    "kind 非法值报错",
			files:   map[string]string{".appkit.yml": "version: 1\nkind: platform\ndomain: ledger\nmodule: m\n"},
			wantErr: "kind",
		},
		{
			name: "module 缺省回退 go.mod",
			files: map[string]string{
				".appkit.yml": "version: 1\ndomain: ledger\n",
				"go.mod":      ledgerGoMod,
			},
			check: func(t *testing.T, cfg *archcheck.Config) {
				if cfg.Module != "github.com/forgeplex/ledger" {
					t.Errorf("Module = %q", cfg.Module)
				}
			},
		},
		{
			name:    "domain 仓库缺 module 且无 go.mod 报错",
			files:   map[string]string{".appkit.yml": "version: 1\ndomain: ledger\n"},
			wantErr: "module",
		},
		{
			name:  "system 仓库可无 domain",
			files: map[string]string{".appkit.yml": "version: 1\nkind: system\nmodule: github.com/forgeplex/psp\n"},
			check: func(t *testing.T, cfg *archcheck.Config) {
				if cfg.Kind != archcheck.KindSystem {
					t.Errorf("Kind = %q", cfg.Kind)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRepo(t, tt.files)
			cfg, err := archcheck.LoadConfig(dir)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v，期望包含 %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}
