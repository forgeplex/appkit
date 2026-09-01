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

// TestWarnAppkitMissing 锁提示的三叉判定：appkit 发版后，工作区没有 appkit
// 本体是合法形态（go.mod 已 require + GOPRIVATE 覆盖 → 静默），只有
// 「未 require」与「已 require 但 GOPRIVATE 缺失」两种形态需要说话。
func TestWarnAppkitMissing(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "psp")
	writeModule(t, work, "example.com/psp")

	cases := []struct {
		name      string
		gomod     string
		goprivate string
		wantHint  string // 空串 = 应静默
	}{
		{
			name:     "未require——依赖解析没有来源",
			gomod:    "module example.com/psp\n\ngo 1.26\n",
			wantHint: "未 require 发版的 appkit 版本",
		},
		{
			name:      "已require且GOPRIVATE覆盖——静默",
			gomod:     "module example.com/psp\n\ngo 1.26\n\nrequire github.com/forgeplex/appkit v0.5.2\n",
			goprivate: "github.com/forgeplex/*",
		},
		{
			name:     "已require但GOPRIVATE未覆盖",
			gomod:    "module example.com/psp\n\ngo 1.26\n\nrequire (\n\tgithub.com/forgeplex/appkit v0.5.2\n)\n",
			wantHint: "GOPRIVATE",
		},
		{
			// 占位版本是手工钉向工作区的写法，本身就要靠工作区解析。
			name:     "占位版本不算已require",
			gomod:    "module example.com/psp\n\ngo 1.26\n\nrequire github.com/forgeplex/appkit v0.0.0-00010101000000-000000000000\n",
			wantHint: "未 require 发版的 appkit 版本",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte(tt.gomod), 0o644); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			warnAppkitMissing(work, []string{"."}, tt.goprivate, &out)
			got := out.String()
			if tt.wantHint == "" {
				if got != "" {
					t.Fatalf("应静默，实际输出:\n%s", got)
				}
				return
			}
			if !strings.Contains(got, tt.wantHint) {
				t.Fatalf("提示缺 %q:\n%s", tt.wantHint, got)
			}
		})
	}

	// 工作区已含 appkit 本体：无论 require 形态如何都静默。
	appkitDir := filepath.Join(root, "appkit")
	writeModule(t, appkitDir, "github.com/forgeplex/appkit")
	if err := os.WriteFile(filepath.Join(work, "go.mod"),
		[]byte("module example.com/psp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	warnAppkitMissing(work, []string{".", appkitDir}, "", &out)
	if out.Len() != 0 {
		t.Errorf("工作区含 appkit 本体时不应提示:\n%s", out.String())
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
