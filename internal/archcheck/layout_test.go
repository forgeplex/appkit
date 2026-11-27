package archcheck_test

import (
	"testing"

	"github.com/forgeplex/appkit/internal/archcheck"
)

func TestCheckLayout(t *testing.T) {
	cfg := &archcheck.Config{
		Version: 1,
		Kind:    archcheck.KindDomain,
		Domain:  "ledger",
		Module:  "github.com/forgeplex/ledger",
	}

	tests := []struct {
		name  string
		cfg   *archcheck.Config
		files map[string]string
		want  []wantV
	}{
		{
			name: "标准组件目录与按功能嵌套子包零违规",
			cfg:  cfg,
			files: map[string]string{
				"internal/ledger/service.go":         "package ledger\n",
				"internal/ledger/journal/entry.go":   "package journal\n",
				"internal/ledger/settlement/calc.go": "package settlement\n",
				"internal/postgres/store.go":         "package postgres\n",
				"internal/http/handler.go":           "package http\n",
				"internal/inbox/consumer.go":         "package inbox\n",
				"internal/module/module.go":          "package module\n",
			},
		},
		{
			name: "禁止在 internal 下建平级业务目录",
			cfg:  cfg,
			files: map[string]string{
				"internal/ledger/service.go": "package ledger\n",
				"internal/report/report.go":  "package report\n",
				"internal/util/helper.go":    "package util\n",
			},
			want: []wantV{
				{File: "internal/report", Msg: "禁止在 internal/ 下建平级目录 internal/report"},
				{File: "internal/util", Msg: "禁止在 internal/ 下建平级目录 internal/util"},
			},
		},
		{
			name: "禁止在业务包下按层拆子包",
			cfg:  cfg,
			files: map[string]string{
				"internal/ledger/model/user.go":       "package model\n",
				"internal/ledger/service/posting.go":  "package service\n",
				"internal/ledger/repo/account.go":     "package repo\n",
				"internal/ledger/dto/response.go":     "package dto\n",
				"internal/ledger/journal/models/x.go": "package models\n",
			},
			want: []wantV{
				{File: "internal/ledger/dto", Msg: "禁止按层拆子包"},
				{File: "internal/ledger/journal/models", Msg: "禁止按层拆子包"},
				{File: "internal/ledger/model", Msg: "禁止按层拆子包"},
				{File: "internal/ledger/repo", Msg: "禁止按层拆子包"},
				{File: "internal/ledger/service", Msg: "禁止按层拆子包"},
			},
		},
		{
			name: "system 仓库允许自定义 internal 但禁止 sqlc.yaml 与 db/queries",
			cfg:  &archcheck.Config{Version: 1, Kind: archcheck.KindSystem, Module: "github.com/forgeplex/psp"},
			files: map[string]string{
				"internal/anything/x.go": "package anything\n",
				"sqlc.yaml":              "version: \"2\"\n",
				"db/queries/q.sql":       "SELECT 1;\n",
			},
			want: []wantV{
				{File: "sqlc.yaml", Msg: "禁止包含 sqlc.yaml"},
				{File: "db/queries", Msg: "禁止包含 db/queries/"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRepo(t, tt.files)
			got, err := archcheck.CheckLayout(dir, tt.cfg)
			if err != nil {
				t.Fatalf("CheckLayout: %v", err)
			}
			assertViolations(t, got, tt.want)
		})
	}
}
