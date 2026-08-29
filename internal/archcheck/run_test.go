package archcheck_test

import (
	"strings"
	"testing"

	"github.com/forgeplex/appkit/internal/archcheck"
)

func TestRun(t *testing.T) {
	t.Run("缺 .appkit.yml 报错提示先创建", func(t *testing.T) {
		dir := writeRepo(t, map[string]string{})
		_, err := archcheck.Run(dir)
		if err == nil || !strings.Contains(err.Error(), "未找到 .appkit.yml") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("domain 仓库四类检查按序汇总", func(t *testing.T) {
		dir := writeRepo(t, map[string]string{
			".appkit.yml": domainYML,
			"go.mod": `module github.com/forgeplex/ledger

go 1.26

require (
	github.com/forgeplex/appkit v0.1.0
	github.com/forgeplex/auth v0.2.0
)
`,
			"internal/ledger/service.go": `package ledger

import "github.com/jackc/pgx/v5"
`,
			"db/queries/q.sql":            "SELECT * FROM auth.users;\n",
			"db/migrations/0001_init.sql": "CREATE SCHEMA ledger;\n",
			"db/migrations/0002_a.sql":    "-- a\n",
			"db/migrations/0002_b.sql":    "-- b\n",
		})
		got, err := archcheck.Run(dir)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		assertViolations(t, got, []wantV{
			{File: "go.mod", Line: 7, Msg: "github.com/forgeplex/auth"},
			{File: "internal/ledger/service.go", Line: 3, Msg: "pgx"},
			{File: "db/queries/q.sql", Line: 1, Msg: "跨 schema 引用 auth"},
			{File: "db/migrations/0002_b.sql", Msg: "重复"},
		})
	})

	t.Run("system 仓库跳过 domain 相关检查", func(t *testing.T) {
		dir := writeRepo(t, map[string]string{
			".appkit.yml": "version: 1\nkind: system\nmodule: github.com/forgeplex/psp\n",
			"go.mod": `module github.com/forgeplex/psp

go 1.26

require (
	github.com/forgeplex/appkit v0.1.0
	github.com/forgeplex/auth v0.2.0
	github.com/forgeplex/ledger v0.5.0
)
`,
			"cmd/psp/main.go": `package main

import (
	"github.com/forgeplex/auth"
	"github.com/forgeplex/ledger"
	"github.com/jackc/pgx/v5/pgxpool"
)
`,
			"db/queries/report.sql":       "SELECT * FROM ledger.outbox JOIN auth.users u ON true;\n",
			"db/migrations/0001_init.sql": "-- init\n",
			"db/migrations/0001_dup.sql":  "-- dup\n",
		})
		got, err := archcheck.Run(dir)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		// requires/跨域 import/SQL schema 均跳过，仅剩迁移序号重复。
		assertViolations(t, got, []wantV{
			{File: "db/migrations/0001_init.sql", Msg: "重复"},
		})
	})

	t.Run("干净的 domain 仓库零违规", func(t *testing.T) {
		dir := writeRepo(t, map[string]string{
			".appkit.yml": domainYML,
			"go.mod": `module github.com/forgeplex/ledger

go 1.26

require (
	github.com/forgeplex/appkit v0.1.0
	github.com/forgeplex/psp-contracts/go v0.3.0
)
`,
			"internal/ledger/service.go": `package ledger

import (
	"context"

	"github.com/forgeplex/appkit/tx"
	"github.com/forgeplex/psp-contracts/go/ledgerv1"
)
`,
			"internal/postgres/store.go": `package postgres

import "github.com/jackc/pgx/v5"
`,
			"db/queries/q.sql":            "SELECT o.id FROM ledger.outbox o;\n",
			"db/migrations/0001_init.sql": "CREATE SCHEMA IF NOT EXISTS ledger;\n",
		})
		got, err := archcheck.Run(dir)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		assertViolations(t, got, nil)
	})
}
