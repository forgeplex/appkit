package gen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// genContract 把 yaml 写进临时目录并跑 Contract 生成器，返回错误。
func genContract(t *testing.T, content string) error {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "contract.yaml")
	if err := os.WriteFile(in, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return Contract(in, filepath.Join(dir, "out"))
}

func TestContractInvalid(t *testing.T) {
	// valid 包装出一份只差 methods 段的合法 yaml。
	valid := func(methods string) string {
		return "version: 1\npackage: ledgerv1\nsystem: ledger\nmethods:\n" + methods
	}
	oneMethod := "  - {name: M, path: /m, doc: 方法。}"
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"版本不支持", "version: 2\npackage: p\nsystem: s\nmethods: [{name: M, path: /m, doc: d}]", "version 2"},
		{"包名非法", "version: 1\npackage: LedgerV1\nsystem: s\nmethods: [{name: M, path: /m, doc: d}]", "package"},
		{"system 非法", "version: 1\npackage: p\nsystem: Ledger\nmethods: [{name: M, path: /m, doc: d}]", "system"},
		{"methods 为空", "version: 1\npackage: p\nsystem: s\nmethods: []", "methods 为空"},
		{"方法名非导出", valid("  - {name: greet, path: /g, doc: d}"), "方法名"},
		{"方法名重复", valid("  - {name: M, path: /a, doc: d}\n  - {name: M, path: /b, doc: d}"), "重复"},
		{"path 非法", valid("  - {name: M, path: 'no slash', doc: d}"), "path"},
		{"path 重复", valid("  - {name: A, path: /m, doc: d}\n  - {name: B, path: /m, doc: d}"), "重复"},
		{"缺 doc", valid("  - {name: M, path: /m}"), "缺少 doc"},
		{"字段名非法", valid("  - name: M\n    path: /m\n    doc: d\n    request: [{name: EntryID, type: string}]"), "字段名"},
		{"字段类型不支持", valid("  - name: M\n    path: /m\n    doc: d\n    request: [{name: amount, type: float}]"), "float"},
		{"required 非 string", valid("  - name: M\n    path: /m\n    doc: d\n    request: [{name: n, type: int64, required: true}]"), "required"},
		{"字段生成同名", valid("  - name: M\n    path: /m\n    doc: d\n    request: [{name: entry_id, type: string}, {name: entry__id, type: string}]"), "同名 Go 字段"},
		{"引用不存在的命名 DTO", valid("  - name: M\n    path: /m\n    doc: d\n    response: [{name: entries, type: '[]Ghost'}]"), "Ghost"},
		{"request 可用命名 DTO", "version: 1\npackage: p\nsystem: s\ntypes: [{name: Entry, fields: [{name: id, type: string}]}]\nmethods:\n  - name: M\n    path: /m\n    doc: d\n    request: [{name: entries, type: '[]Entry'}]", ""},
		{"类型名非法", "version: 1\npackage: p\nsystem: s\ntypes: [{name: entry, fields: [{name: id, type: string}]}]\nmethods:\n" + oneMethod, "类型名"},
		{"类型名重复", "version: 1\npackage: p\nsystem: s\ntypes:\n  - {name: Entry, fields: [{name: id, type: string}]}\n  - {name: Entry, fields: [{name: id, type: string}]}\nmethods:\n" + oneMethod, "重复"},
		{"类型无字段", "version: 1\npackage: p\nsystem: s\ntypes: [{name: Entry, fields: []}]\nmethods:\n" + oneMethod, "没有字段"},
		{"类型撞生成 DTO 名", "version: 1\npackage: p\nsystem: s\ntypes: [{name: MRequest, fields: [{name: id, type: string}]}]\nmethods:\n  - name: M\n    path: /m\n    doc: d\n    request: [{name: x, type: string}]", "撞名"},
		{"类型嵌套超一层", "version: 1\npackage: p\nsystem: s\ntypes:\n  - {name: Entry, fields: [{name: id, type: string}]}\n  - {name: Bag, fields: [{name: entries, type: '[]Entry'}]}\nmethods:\n" + oneMethod, "封顶一层"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := genContract(t, tc.yaml)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("期望成功，得到: %v", err)
				}
				return
			}
			wantErrContains(t, err, tc.wantErr)
		})
	}
}

// TestContractPositions 报错必须带 文件:行号。
func TestContractPositions(t *testing.T) {
	err := genContract(t, "version: 1\npackage: p\nsystem: s\nmethods:\n  - {name: Good, path: /a, doc: d}\n  - {name: bad, path: /b, doc: d}\n")
	if err == nil || !strings.Contains(err.Error(), "contract.yaml:6") {
		t.Fatalf("期望错误定位到第 6 行，得到: %v", err)
	}
}

func TestContractArgAndFileErrors(t *testing.T) {
	if err := Contract("", ""); err == nil || !strings.Contains(err.Error(), "-in") {
		t.Errorf("缺 flag 应报错，得到 %v", err)
	}
	if err := Contract(filepath.Join(t.TempDir(), "no.yaml"), "out"); err == nil {
		t.Error("输入不存在应报错")
	}
}

// TestContractGeneratedShape 锁住四种方法形态在生成物里的签名形状
// （golden 锁全文，这里锁语义骨架，防止 golden 被 -update 盲改）。
func TestContractGeneratedShape(t *testing.T) {
	dir := t.TempDir()
	if err := Contract("testdata/contract.yaml", dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "service.gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Greet(ctx context.Context, req GreetRequest) (GreetReply, error)",
		"Stats(ctx context.Context) (StatsReply, error)",
		"Reset(ctx context.Context, req ResetRequest) error",
		"Ping(ctx context.Context) error",
		"Entries    []Entry `json:\"entries\"`",
		"Amount string    `json:\"amount\"`", // decimal → string
		"type Service interface",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("service.gen.go 缺少 %q", want)
		}
	}
}
