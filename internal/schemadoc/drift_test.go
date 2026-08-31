package schemadoc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiff(t *testing.T) {
	want := map[string]string{
		"db/SCHEMA.md":            "总览",
		"db/schema/accounts.sql":  "A",
		"db/schema/entries.sql":   "B",
		"db/schema/_appkit/o.sql": "C",
	}
	cases := []struct {
		name    string
		got     map[string]string
		wantErr []string // 错误里必须出现的片段，空 = 期望无错
	}{
		{name: "一致", got: map[string]string{
			"db/SCHEMA.md": "总览", "db/schema/accounts.sql": "A",
			"db/schema/entries.sql": "B", "db/schema/_appkit/o.sql": "C",
		}},
		{name: "缺文件", got: map[string]string{
			"db/SCHEMA.md": "总览", "db/schema/entries.sql": "B", "db/schema/_appkit/o.sql": "C",
		}, wantErr: []string{"缺失", "db/schema/accounts.sql"}},
		{name: "被手改", got: map[string]string{
			"db/SCHEMA.md": "总览", "db/schema/accounts.sql": "A 改了一个字节",
			"db/schema/entries.sql": "B", "db/schema/_appkit/o.sql": "C",
		}, wantErr: []string{"内容漂移", "db/schema/accounts.sql"}},
		{
			// 与 ruleset.Check 的关键差别：这里的文件集是动态的。迁移删掉一张表后，
			// 残留的 db/schema/x.sql 会一直躺在仓库里骗人，必须报出来。
			name: "陈旧残留", got: map[string]string{
				"db/SCHEMA.md": "总览", "db/schema/accounts.sql": "A",
				"db/schema/entries.sql": "B", "db/schema/_appkit/o.sql": "C",
				"db/schema/dropped.sql": "旧的",
			}, wantErr: []string{"多余", "db/schema/dropped.sql"},
		},
		{name: "三类一次报全", got: map[string]string{
			"db/SCHEMA.md": "总览", "db/schema/entries.sql": "改过",
			"db/schema/_appkit/o.sql": "C", "db/schema/dropped.sql": "旧的",
		}, wantErr: []string{"缺失", "db/schema/accounts.sql", "内容漂移", "db/schema/entries.sql", "多余", "db/schema/dropped.sql"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := diff(want, c.got)
			if len(c.wantErr) == 0 {
				if err != nil {
					t.Fatalf("期望无漂移，得到：%v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("期望报漂移，得到 nil")
			}
			for _, frag := range c.wantErr {
				if !strings.Contains(err.Error(), frag) {
					t.Errorf("错误里缺 %q：\n%v", frag, err)
				}
			}
		})
	}
}

func TestAdoptedGate(t *testing.T) {
	cases := []struct {
		name  string
		setup func(dir string)
		want  bool
	}{
		{name: "全新仓库", setup: func(string) {}, want: false},
		{name: "只有总览", setup: func(dir string) {
			mustWrite(t, filepath.Join(dir, "db", "SCHEMA.md"), "x")
		}, want: true},
		{
			// 只删一半不能让检查静音——否则「删掉就不检查了」会变成一条真实的逃生路。
			name: "只有目录", setup: func(dir string) {
				if err := os.MkdirAll(filepath.Join(dir, "db", "schema"), 0o755); err != nil {
					t.Fatal(err)
				}
			}, want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			c.setup(dir)
			got, err := Adopted(dir)
			if err != nil {
				t.Fatalf("Adopted: %v", err)
			}
			if got != c.want {
				t.Errorf("Adopted = %v，期望 %v", got, c.want)
			}
		})
	}
}

func TestReadOnDiskAndPrune(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "db", "SCHEMA.md"), "总览")
	mustWrite(t, filepath.Join(dir, "db", "schema", "accounts.sql"), "A")
	mustWrite(t, filepath.Join(dir, "db", "schema", "_appkit", "outbox.sql"), "O")
	mustWrite(t, filepath.Join(dir, "db", "migrations", "0001.sql"), "不该被碰")

	got, err := readOnDisk(dir)
	if err != nil {
		t.Fatalf("readOnDisk: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("读到 %d 个文件，期望 3：%v", len(got), keysOf(got))
	}
	// 路径一律斜杠：Windows 上生成的产出也必须与 Linux 逐字节一致。
	for p := range got {
		if strings.Contains(p, `\`) {
			t.Errorf("路径含反斜杠：%q", p)
		}
	}

	want := map[string]string{"db/SCHEMA.md": "总览", "db/schema/accounts.sql": "A"}
	if err := pruneStale(dir, want); err != nil {
		t.Fatalf("pruneStale: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "db", "schema", "_appkit", "outbox.sql")); !os.IsNotExist(err) {
		t.Error("陈旧产出没被清掉")
	}
	for _, keep := range []string{
		filepath.Join(dir, "db", "schema", "accounts.sql"),
		filepath.Join(dir, "db", "SCHEMA.md"),
		filepath.Join(dir, "db", "migrations", "0001.sql"),
	} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("不该删的文件被删了：%s", keep)
		}
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
