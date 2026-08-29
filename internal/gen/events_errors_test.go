package gen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// genYAML 把 yaml 写进临时目录并跑生成器，返回错误。
func genYAML(t *testing.T, run func(in, out string) error, content string) error {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.yaml")
	if err := os.WriteFile(in, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return run(in, filepath.Join(dir, "out.gen.go"))
}

func wantErrContains(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望报错（含 %q），实际成功", sub)
	}
	if !strings.Contains(err.Error(), sub) {
		t.Fatalf("期望错误含 %q，得到: %v", sub, err)
	}
}

func TestEventsInvalid(t *testing.T) {
	valid := func(events string) string {
		return "version: 1\npackage: ledgerv1\nevents:\n" + events
	}
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"版本不支持", "version: 2\npackage: p\nevents: [{name: E, topic: a.b}]", "version 2"},
		{"包名非法", "version: 1\npackage: LedgerV1\nevents: [{name: E, topic: a.b}]", "package"},
		{"events 为空", "version: 1\npackage: p\nevents: []", "events 为空"},
		{"yaml 语法错误", "version: [", "解析 yaml"},
		{"事件名非导出", valid("  - {name: entryPosted, topic: a.b}"), "事件名"},
		{"事件名重复", valid("  - {name: E, topic: a.b}\n  - {name: E, topic: a.c}"), "重复"},
		{"topic 非法", valid("  - {name: E, topic: 'Ledger Posted'}"), "topic"},
		{"topic 重复", valid("  - {name: E, topic: a.b}\n  - {name: F, topic: a.b}"), "重复"},
		{"字段名非法", valid("  - name: E\n    topic: a.b\n    fields: [{name: EntryID, type: string}]"), "字段名"},
		{"字段类型不支持", valid("  - name: E\n    topic: a.b\n    fields: [{name: amount, type: float}]"), "float"},
		{"required 非 string", valid("  - name: E\n    topic: a.b\n    fields: [{name: amount, type: decimal, required: true}]"), "required"},
		{"字段撞 Event 方法", valid("  - name: E\n    topic: a.b\n    fields: [{name: event, type: string}]"), "Event() 方法冲突"},
		{"字段生成同名", valid("  - name: E\n    topic: a.b\n    fields: [{name: entry_id, type: string}, {name: entry__id, type: string}]"), "同名 Go 字段"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantErrContains(t, genYAML(t, Events, tc.yaml), tc.wantErr)
		})
	}
}

func TestEventsArgAndFileErrors(t *testing.T) {
	if err := Events("", ""); err == nil || !strings.Contains(err.Error(), "-in") {
		t.Errorf("缺 flag 应报错，得到 %v", err)
	}
	if err := Events(filepath.Join(t.TempDir(), "no.yaml"), "out.go"); err == nil {
		t.Error("输入不存在应报错")
	}
}

func TestErrorsInvalid(t *testing.T) {
	valid := func(codes string) string {
		return "version: 1\npackage: ledgerv1\ncodes:\n" + codes
	}
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"版本不支持", "version: 0\npackage: p\ncodes: [{code: A, status: 400, message: m}]", "version 0"},
		{"包名非法", "version: 1\npackage: 1p\ncodes: [{code: A, status: 400, message: m}]", "package"},
		{"codes 为空", "version: 1\npackage: p\ncodes: []", "codes 为空"},
		{"错误码非法", valid("  - {code: ledger_broke, status: 400, message: m}"), "错误码"},
		{"错误码重复", valid("  - {code: A_B, status: 400, message: m}\n  - {code: A_B, status: 404, message: n}"), "重复"},
		{"同名标识符", valid("  - {code: A_B, status: 400, message: m}\n  - {code: A__B, status: 404, message: n}"), "同名标识符"},
		{"status 越界", valid("  - {code: A_B, status: 42, message: m}"), "status 42"},
		{"缺 message", valid("  - {code: A_B, status: 400}"), "message"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantErrContains(t, genYAML(t, Errors, tc.yaml), tc.wantErr)
		})
	}
}

// TestErrorsPositions 报错必须带 文件:行号。
func TestErrorsPositions(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "codes.yaml")
	content := "version: 1\npackage: p\ncodes:\n  - {code: OK_CODE, status: 400, message: m}\n  - {code: bad, status: 400, message: m}\n"
	if err := os.WriteFile(in, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Errors(in, filepath.Join(dir, "out.gen.go"))
	wantErrContains(t, err, in+":5")
}

// TestEventsCamel 快速覆盖名字转换（含缩写）。
func TestEventsCamel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"entry_id", "EntryID"},
		{"amount", "Amount"},
		{"LEDGER_INSUFFICIENT_FUNDS", "LedgerInsufficientFunds"},
		{"http_url", "HTTPURL"},
		{"uuid", "UUID"},
	}
	for _, tc := range cases {
		if got := camel(tc.in); got != tc.want {
			t.Errorf("camel(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}
