package gen

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// -update 重写 genfixture 下检入的生成物（golden 与可编译夹具是同一份文件）。
var update = flag.Bool("update", false, "重写 genfixture 下的 golden 生成物")

func TestGolden(t *testing.T) {
	cases := []struct {
		name   string
		golden string
		run    func(out string) error
	}{
		{
			name:   "events",
			golden: "genfixture/events.gen.go",
			run:    func(out string) error { return Events("testdata/events.yaml", out) },
		},
		{
			name:   "errors",
			golden: "genfixture/codes.gen.go",
			run:    func(out string) error { return Errors("testdata/codes.yaml", out) },
		},
		{
			name:   "wrap",
			golden: "genfixture/service_wrap.gen.go",
			run:    func(out string) error { return Wrap("genfixture", "Service", "greet", out) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out.gen.go")
			if err := tc.run(out); err != nil {
				t.Fatalf("生成失败: %v", err)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if *update {
				if err := os.WriteFile(tc.golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(tc.golden)
			if err != nil {
				t.Fatalf("读取 golden 失败（先用 -update 生成）: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("生成物与 %s 不一致（若为有意变更，用 -update 重写）\n--- got ---\n%s", tc.golden, got)
			}
		})
	}
}

// TestGoldenStable 确认重复生成逐字节稳定（map 迭代序等不确定性没有泄漏进输出）。
func TestGoldenStable(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.gen.go")
	b := filepath.Join(dir, "b.gen.go")
	if err := Wrap("genfixture", "Service", "greet", a); err != nil {
		t.Fatal(err)
	}
	if err := Wrap("genfixture", "Service", "greet", b); err != nil {
		t.Fatal(err)
	}
	ab, _ := os.ReadFile(a)
	bb, _ := os.ReadFile(b)
	if !bytes.Equal(ab, bb) {
		t.Error("两次生成输出不一致")
	}
}
