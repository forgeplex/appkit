package archcheck_test

import (
	"testing"

	"github.com/forgeplex/appkit/internal/archcheck"
)

var ledgerCfg = &archcheck.Config{
	Version:   1,
	Kind:      archcheck.KindDomain,
	Domain:    "ledger",
	Module:    "github.com/forgeplex/ledger",
	Contracts: "github.com/forgeplex/psp-contracts/go",
}

func TestCheckImports(t *testing.T) {
	tests := []struct {
		name  string
		cfg   *archcheck.Config
		files map[string]string
		want  []wantV
	}{
		{
			name: "业务包零 infra：pgx/gin/net/http 全违规",
			cfg:  ledgerCfg,
			files: map[string]string{
				"internal/ledger/service.go": `package ledger

import (
	"context"
	"net/http"
	"net/http/httputil"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)
`,
			},
			want: []wantV{
				{File: "internal/ledger/service.go", Line: 5, Msg: "禁止 import net/http"},
				{File: "internal/ledger/service.go", Line: 6, Msg: "禁止 import net/http/httputil"},
				{File: "internal/ledger/service.go", Line: 8, Msg: "禁止 import github.com/gin-gonic/gin"},
				{File: "internal/ledger/service.go", Line: 9, Msg: "禁止 import github.com/jackc/pgx/v5"},
				{File: "internal/ledger/service.go", Line: 10, Msg: "禁止 import github.com/jackc/pgx/v5/pgxpool"},
			},
		},
		{
			name: "业务包不得反向依赖本仓库 transport 与 postgres",
			cfg:  ledgerCfg,
			files: map[string]string{
				"internal/ledger/wire.go": `package ledger

import (
	"github.com/forgeplex/ledger/internal/http"
	"github.com/forgeplex/ledger/internal/inbox"
	"github.com/forgeplex/ledger/internal/postgres"
)
`,
			},
			want: []wantV{
				{File: "internal/ledger/wire.go", Line: 4, Msg: "internal/http"},
				{File: "internal/ledger/wire.go", Line: 5, Msg: "internal/inbox"},
				{File: "internal/ledger/wire.go", Line: 6, Msg: "internal/postgres"},
			},
		},
		{
			name: "业务包允许 stdlib、appkit 无驱动包与 contracts",
			cfg:  ledgerCfg,
			files: map[string]string{
				"internal/ledger/ok.go": `package ledger

import (
	"context"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/tx"
	"github.com/forgeplex/psp-contracts/go/ledgerv1"
)
`,
			},
			want: nil,
		},
		{
			name: "pgx 只许 internal/postgres：cmd 与 internal/module 放行",
			cfg:  ledgerCfg,
			files: map[string]string{
				"cmd/ledgerd/main.go": `package main

import "github.com/jackc/pgx/v5/pgxpool"
`,
				"internal/module/module.go": `package module

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/ledger/internal/postgres/sqlc"
)
`,
				"internal/postgres/store.go": `package postgres

import (
	"github.com/jackc/pgx/v5"

	"github.com/forgeplex/ledger/internal/postgres/sqlc"
)
`,
				"internal/report/report.go": `package report

import "github.com/jackc/pgx/v5"
`,
			},
			want: []wantV{
				{File: "internal/report/report.go", Line: 3, Msg: "pgx 只允许出现在 internal/postgres"},
			},
		},
		{
			name: "sqlc 生成物只许 internal/postgres 使用",
			cfg:  ledgerCfg,
			files: map[string]string{
				"internal/report/report.go": `package report

import "github.com/forgeplex/ledger/internal/postgres/sqlc"
`,
			},
			want: []wantV{
				{File: "internal/report/report.go", Line: 3, Msg: "sqlc 生成物只允许被 internal/postgres 使用"},
			},
		},
		{
			name: "transport 不得 import internal/postgres",
			cfg:  ledgerCfg,
			files: map[string]string{
				"internal/http/handler.go": `package http

import "github.com/forgeplex/ledger/internal/postgres"
`,
				"internal/inbox/consumer.go": `package inbox

import "github.com/forgeplex/ledger/internal/postgres"
`,
			},
			want: []wantV{
				{File: "internal/http/handler.go", Line: 3, Msg: "transport 必须走业务包接口"},
				{File: "internal/inbox/consumer.go", Line: 3, Msg: "transport 必须走业务包接口"},
			},
		},
		{
			name: "全仓禁 import 其他 forgeplex 域 module（含 cmd）",
			cfg:  ledgerCfg,
			files: map[string]string{
				"cmd/ledgerd/main.go": `package main

import (
	"github.com/forgeplex/appkit"
	"github.com/forgeplex/auth"
	"github.com/forgeplex/ledger/internal/module"
	"github.com/forgeplex/psp-contracts/go/authv1"
)
`,
				"internal/ledger/svc.go": `package ledger

import "github.com/forgeplex/merchant/internal/merchant"
`,
			},
			want: []wantV{
				{File: "cmd/ledgerd/main.go", Line: 5, Msg: "禁止 import 其他 forgeplex 域 module"},
				{File: "internal/ledger/svc.go", Line: 3, Msg: "禁止 import 其他 forgeplex 域 module"},
			},
		},
		{
			name: "allowRequires 放行的 module 可 import",
			cfg: &archcheck.Config{
				Version: 1, Kind: archcheck.KindDomain, Domain: "ledger",
				Module:        "github.com/forgeplex/ledger",
				AllowRequires: []string{"github.com/forgeplex/shared"},
			},
			files: map[string]string{
				"internal/report/report.go": `package report

import "github.com/forgeplex/shared/csv"
`,
			},
			want: nil,
		},
		{
			name: "_test.go 与 vendor 跳过",
			cfg:  ledgerCfg,
			files: map[string]string{
				"internal/ledger/service_test.go": `package ledger

import "github.com/jackc/pgx/v5"
`,
				"vendor/github.com/x/y/y.go": `package y

import "github.com/jackc/pgx/v5"
`,
			},
			want: nil,
		},
		{
			name: "解析失败记为违规不中断",
			cfg:  ledgerCfg,
			files: map[string]string{
				"internal/report/broken.go": "package report\n\nimport (\n",
				"internal/report/pgx.go": `package report

import "github.com/jackc/pgx/v5"
`,
			},
			want: []wantV{
				{File: "internal/report/broken.go", Msg: "解析失败"},
				{File: "internal/report/pgx.go", Line: 3, Msg: "pgx 只允许出现在 internal/postgres"},
			},
		},
		{
			name: "system 仓库跳过跨域与业务包检查、保留结构性规则",
			cfg: &archcheck.Config{
				Version: 1, Kind: archcheck.KindSystem,
				Module: "github.com/forgeplex/psp",
			},
			files: map[string]string{
				"cmd/psp/main.go": `package main

import (
	"github.com/forgeplex/auth"
	"github.com/forgeplex/ledger"
	"github.com/jackc/pgx/v5/pgxpool"
)
`,
				"internal/wiring/wiring.go": `package wiring

import "github.com/jackc/pgx/v5"
`,
			},
			want: []wantV{
				{File: "internal/wiring/wiring.go", Line: 3, Msg: "pgx 只允许出现在 internal/postgres"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRepo(t, tt.files)
			got, err := archcheck.CheckImports(dir, tt.cfg)
			if err != nil {
				t.Fatalf("CheckImports: %v", err)
			}
			assertViolations(t, got, tt.want)
		})
	}
}
