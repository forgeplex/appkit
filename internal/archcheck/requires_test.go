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
		gomod string
		cfg   *archcheck.Config
		want  []wantV
	}{
		{
			name: "放行 appkit、appkit/lint、contracts 与第三方",
			gomod: `module github.com/forgeplex/ledger

go 1.26

require (
	github.com/forgeplex/appkit v0.1.0
	github.com/forgeplex/appkit/lint v0.1.0
	github.com/forgeplex/psp-contracts/go v0.3.0
	github.com/jackc/pgx/v5 v5.10.0
)
`,
			cfg:  cfg,
			want: nil,
		},
		{
			name: "require 其他域 module 违规且行号准确",
			gomod: `module github.com/forgeplex/ledger

go 1.26

require (
	github.com/forgeplex/appkit v0.1.0
	github.com/forgeplex/auth v0.2.0
	github.com/forgeplex/merchant v0.1.0
)
`,
			cfg: cfg,
			want: []wantV{
				{File: "go.mod", Line: 7, Msg: "github.com/forgeplex/auth"},
				{File: "go.mod", Line: 8, Msg: "github.com/forgeplex/merchant"},
			},
		},
		{
			name:  "单行 require 也能定位",
			gomod: "module github.com/forgeplex/ledger\n\ngo 1.26\n\nrequire github.com/forgeplex/gateway v0.1.0\n",
			cfg:   cfg,
			want:  []wantV{{File: "go.mod", Line: 5, Msg: "github.com/forgeplex/gateway"}},
		},
		{
			name: "allowRequires 放行指定 module",
			gomod: `module github.com/forgeplex/ledger

go 1.26

require github.com/forgeplex/shared v0.1.0
`,
			cfg: &archcheck.Config{
				Version: 1, Kind: archcheck.KindDomain, Domain: "ledger",
				Module:        "github.com/forgeplex/ledger",
				AllowRequires: []string{"github.com/forgeplex/shared"},
			},
			want: nil,
		},
		{
			name: "contracts 为空时不放行 contracts module",
			gomod: `module github.com/forgeplex/ledger

go 1.26

require github.com/forgeplex/psp-contracts/go v0.3.0
`,
			cfg: &archcheck.Config{
				Version: 1, Kind: archcheck.KindDomain, Domain: "ledger",
				Module: "github.com/forgeplex/ledger",
			},
			want: []wantV{{File: "go.mod", Msg: "github.com/forgeplex/psp-contracts/go"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRepo(t, map[string]string{"go.mod": tt.gomod})
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
