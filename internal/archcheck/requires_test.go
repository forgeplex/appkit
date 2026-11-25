package archcheck_test

import (
	"testing"

	"github.com/forgeplex/appkit/internal/archcheck"
)

func TestCheckRequires(t *testing.T) {
	cfg := &archcheck.Config{
		Version:   1,
		Kind:      archcheck.KindDomain,
		Domain:    "ledger",
		Module:    "github.com/forgeplex/ledger",
		Contracts: "github.com/forgeplex/psp-contracts/go",
	}
	tests := []struct {
		name  string
		files map[string]string
		cfg   *archcheck.Config
		want  []wantV
	}{
		{
			name: "放行 appkit、appkit/lint、contracts 与第三方",
			files: map[string]string{
				"go.mod": `module github.com/forgeplex/ledger

go 1.26

require (
	github.com/forgeplex/appkit v0.1.0
	github.com/forgeplex/appkit/lint v0.1.0
	github.com/forgeplex/psp-contracts/go v0.3.0
	github.com/jackc/pgx/v5 v5.10.0
)
`,
			},
			cfg:  cfg,
			want: nil,
		},
		{
			name: "放行自身及对端嵌套 */contract 子模块，拒绝非 contract 子路径",
			files: map[string]string{
				"go.mod": `module github.com/forgeplex/ledger

go 1.26

require (
	github.com/forgeplex/appkit v0.1.0
	github.com/forgeplex/ledger/contract v0.1.0
	github.com/forgeplex/subscription/contract v0.1.0
	github.com/forgeplex/subscription v0.1.0
	github.com/forgeplex/subscription/pkg v0.1.0
)
`,
			},
			cfg: cfg,
			want: []wantV{
				{File: "go.mod", Line: 9, Msg: "github.com/forgeplex/subscription"},
				{File: "go.mod", Line: 10, Msg: "github.com/forgeplex/subscription/pkg"},
			},
		},
		{
			name: "干净的嵌套 contract/go.mod 零违规",
			files: map[string]string{
				"go.mod": `module github.com/forgeplex/ledger

go 1.26

require (
	github.com/forgeplex/appkit v0.1.0
	github.com/forgeplex/ledger/contract v0.1.0
)
`,
				"contract/go.mod": `module github.com/forgeplex/ledger/contract

go 1.26

require (
	github.com/forgeplex/appkit v0.1.0
	github.com/forgeplex/subscription/contract v0.1.0
)
`,
			},
			cfg:  cfg,
			want: nil,
		},
		{
			name: "contract/go.mod 声明错误、反向 require 根模块、require 域模块或 pgx 均违规",
			files: map[string]string{
				"go.mod": ledgerGoMod,
				"contract/go.mod": `module github.com/forgeplex/wrong/contract

go 1.26

require (
	github.com/forgeplex/appkit v0.1.0
	github.com/forgeplex/ledger v0.1.0
	github.com/forgeplex/auth v0.2.0
	github.com/jackc/pgx/v5 v5.10.0
)
`,
			},
			cfg: cfg,
			want: []wantV{
				{File: "contract/go.mod", Line: 1, Msg: "必须为 github.com/forgeplex/ledger/contract"},
				{File: "contract/go.mod", Line: 7, Msg: "禁止反向 require 根域模块"},
				{File: "contract/go.mod", Line: 8, Msg: "禁止 require github.com/forgeplex/auth"},
				{File: "contract/go.mod", Line: 9, Msg: "禁止 require pgx"},
			},
		},
		{
			name: "require 其他域 module 违规且行号准确",
			files: map[string]string{
				"go.mod": `module github.com/forgeplex/ledger

go 1.26

require (
	github.com/forgeplex/appkit v0.1.0
	github.com/forgeplex/auth v0.2.0
	github.com/forgeplex/merchant v0.1.0
)
`,
			},
			cfg: cfg,
			want: []wantV{
				{File: "go.mod", Line: 7, Msg: "github.com/forgeplex/auth"},
				{File: "go.mod", Line: 8, Msg: "github.com/forgeplex/merchant"},
			},
		},
		{
			name:  "单行 require 也能定位",
			files: map[string]string{"go.mod": "module github.com/forgeplex/ledger\n\ngo 1.26\n\nrequire github.com/forgeplex/gateway v0.1.0\n"},
			cfg:   cfg,
			want:  []wantV{{File: "go.mod", Line: 5, Msg: "github.com/forgeplex/gateway"}},
		},
		{
			name: "allowRequires 放行指定 module",
			files: map[string]string{
				"go.mod": `module github.com/forgeplex/ledger

go 1.26

require github.com/forgeplex/shared v0.1.0
`,
			},
			cfg: &archcheck.Config{
				Version: 1, Kind: archcheck.KindDomain, Domain: "ledger",
				Module:        "github.com/forgeplex/ledger",
				AllowRequires: []string{"github.com/forgeplex/shared"},
			},
			want: nil,
		},
		{
			name: "contracts 为空时不放行 contracts module",
			files: map[string]string{
				"go.mod": `module github.com/forgeplex/ledger

go 1.26

require github.com/forgeplex/psp-contracts/go v0.3.0
`,
			},
			cfg: &archcheck.Config{
				Version: 1, Kind: archcheck.KindDomain, Domain: "ledger",
				Module: "github.com/forgeplex/ledger",
			},
			want: []wantV{{File: "go.mod", Msg: "github.com/forgeplex/psp-contracts/go"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRepo(t, tt.files)
			got, err := archcheck.CheckRequires(dir, tt.cfg)
			if err != nil {
				t.Fatalf("CheckRequires: %v", err)
			}
			assertViolations(t, got, tt.want)
		})
	}

	t.Run("go.mod 缺失报错指出文件", func(t *testing.T) {
		dir := writeRepo(t, map[string]string{})
		_, err := archcheck.CheckRequires(dir, cfg)
		if err == nil {
			t.Fatal("期望报错")
		}
	})
}
