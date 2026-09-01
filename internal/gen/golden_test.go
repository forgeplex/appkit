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

// goldenFiles 是每组用例要逐字节比对的检入生成物（相对 internal/gen）。
var goldenFiles = map[string][]string{
	"events":   {"genfixture/events.gen.go"},
	"errors":   {"genfixture/codes.gen.go"},
	"contract": {"genfixture/service.gen.go", "genfixture/wrap.gen.go"},
}

func TestGolden(t *testing.T) {
	runs := map[string]func(dir string) error{
		"events":   func(dir string) error { return Events("testdata/events.yaml", filepath.Join(dir, "events.gen.go")) },
		"errors":   func(dir string) error { return Errors("testdata/codes.yaml", filepath.Join(dir, "codes.gen.go")) },
		"contract": func(dir string) error { return Contract("testdata/contract.yaml", dir) },
	}
	for name, run := range runs {
		t.Run(name, func(t *testing.T) {
			// 每组用例独立目录：contract 的 wrap 环节会扫目录里的 Go 源，
			// 不能让别的用例的产出混进去。
			dir := t.TempDir()
			if err := run(dir); err != nil {
				t.Fatalf("生成失败: %v", err)
			}
			for _, golden := range goldenFiles[name] {
				got, err := os.ReadFile(filepath.Join(dir, filepath.Base(golden)))
				if err != nil {
					t.Fatal(err)
				}
				if *update {
					if err := os.WriteFile(golden, got, 0o644); err != nil {
						t.Fatal(err)
					}
					continue
				}
				want, err := os.ReadFile(golden)
				if err != nil {
					t.Fatalf("读取 golden 失败（先用 -update 生成）: %v", err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("生成物与 %s 不一致（若为有意变更，用 -update 重写）\n--- got ---\n%s", golden, got)
				}
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
