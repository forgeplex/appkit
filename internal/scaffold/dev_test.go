package scaffold

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeModule 造一个最小 go module 目录。
func writeModule(t *testing.T, dir, modpath string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gomod := "module " + modpath + "\n\ngo 1.26\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDev(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "psp")
	writeModule(t, work, "example.com/psp")
	writeModule(t, filepath.Join(root, "ledger"), "example.com/ledger")
	writeModule(t, filepath.Join(root, "gateway"), "example.com/gateway")
	// 无 go.mod 的目录与普通文件都应被跳过。
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := Dev(work, root, &out); err != nil {
		t.Fatalf("Dev: %v\n%s", err, out.String())
	}

	data, err := os.ReadFile(filepath.Join(work, "go.work"))
	if err != nil {
		t.Fatalf("go.work 未生成: %v", err)
	}
	gowork := string(data)
	for _, want := range []string{"ledger", "gateway"} {
		if !strings.Contains(gowork, want) {
			t.Errorf("go.work 应纳入 %s：\n%s", want, gowork)
		}
	}
	if strings.Contains(gowork, "docs") {
		t.Errorf("go.work 不应纳入无 go.mod 的目录：\n%s", gowork)
	}
	// 自身以 "." 纳入，且不因也在 root 下而重复出现第二个条目。
	if !strings.Contains(gowork, "\t.\n") && !strings.Contains(gowork, "use .") {
		t.Errorf("go.work 应以 . 纳入自身：\n%s", gowork)
	}
	for _, line := range strings.Split(gowork, "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), "/psp") {
			t.Errorf("go.work 不应把自身再以路径形式纳入一次：\n%s", gowork)
		}
	}

	// 幂等：重复执行不报错，结果不变。
	before := gowork
	if err := Dev(work, root, nil); err != nil {
		t.Fatalf("Dev 第二次执行: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(work, "go.work"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != before {
		t.Errorf("Dev 应幂等：\n前:\n%s\n后:\n%s", before, after)
	}
}

// TestDevNewModuleAppears 断言后加入的模块在下次 dev 时被纳入。
func TestDevNewModuleAppears(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "psp")
	writeModule(t, work, "example.com/psp")
	if err := Dev(work, root, nil); err != nil {
		t.Fatalf("Dev: %v", err)
	}

	writeModule(t, filepath.Join(root, "auth"), "example.com/auth")
	if err := Dev(work, root, nil); err != nil {
		t.Fatalf("Dev（新增模块后）: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(work, "go.work"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "auth") {
		t.Errorf("新增模块应被纳入：\n%s", data)
	}
}
