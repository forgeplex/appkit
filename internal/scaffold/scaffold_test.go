package scaffold

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// TestNameValidation 校验域名/系统名与 module path 的入口检查。
func TestNameValidation(t *testing.T) {
	cases := []struct {
		name    string
		in      Options
		wantErr string // 空表示应成功
	}{
		{"合法小写", Options{Name: "ledger"}, ""},
		{"合法带数字", Options{Name: "pay2"}, ""},
		{"大写拒绝", Options{Name: "Ledger"}, "不合法"},
		{"数字开头拒绝", Options{Name: "1pay"}, "不合法"},
		{"连字符拒绝", Options{Name: "led-ger"}, "不合法"},
		{"下划线拒绝", Options{Name: "led_ger"}, "不合法"},
		{"空名拒绝", Options{Name: ""}, "不合法"},
		{"保留字 postgres", Options{Name: "postgres"}, "保留字"},
		{"保留字 http", Options{Name: "http"}, "保留字"},
		{"保留字 internal", Options{Name: "internal"}, "保留字"},
		{"非法 module path", Options{Name: "ledger", Module: "github.com/森林/x"}, "module path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.Dir = filepath.Join(t.TempDir(), "out")
			err := Domain(tc.in, nil)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Domain(%q) 应成功: %v", tc.in.Name, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Domain(%q) 错误应含 %q，实际 %v", tc.in.Name, tc.wantErr, err)
			}
			// System 与 Domain 共用同一套校验。
			if err := System(tc.in, nil); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("System(%q) 错误应含 %q，实际 %v", tc.in.Name, tc.wantErr, err)
			}
		})
	}
}

// TestEnsureFreshDir 拒绝写入非空目录。
func TestEnsureFreshDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Domain(Options{Name: "ledger", Dir: dir}, nil)
	if err == nil || !strings.Contains(err.Error(), "非空") {
		t.Fatalf("非空目录应拒绝覆盖，实际 %v", err)
	}
}

// ---- 公共断言 ----

// listFiles 返回 dir 下全部文件的相对路径（正斜杠），排序后返回。
func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s: %v", dir, err)
	}
	sort.Strings(out)
	return out
}

// assertFileSet 断言生成文件集与期望完全一致。
func assertFileSet(t *testing.T, dir string, want []string) {
	t.Helper()
	got := listFiles(t, dir)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if len(got) != len(sorted) {
		t.Fatalf("文件集不一致：\n得到 %v\n期望 %v", got, sorted)
	}
	for i := range got {
		if got[i] != sorted[i] {
			t.Fatalf("文件集不一致：\n得到 %v\n期望 %v", got, sorted)
		}
	}
}

// assertRendered 断言全部生成文件不残留模板占位符。
func assertRendered(t *testing.T, dir string) {
	t.Helper()
	toolFiles, err := SchemaToolFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range listFiles(t, dir) {
		data := readFile(t, dir, rel)
		if source, ok := toolFiles[rel]; ok {
			// Copied Go source is not a text/template; nested literals contain
			// doubled braces. Exact byte parity is stronger than a brace scan.
			if data != string(source) {
				t.Errorf("%s 与框架工具源码不一致", rel)
			}
			continue
		}
		if strings.Contains(data, "{{") || strings.Contains(data, "}}") {
			t.Errorf("%s 残留模板占位符", rel)
		}
	}
}

// assertGoParses 断言全部 .go 文件经 go/parser 无语法错误。
func assertGoParses(t *testing.T, dir string) {
	t.Helper()
	fset := token.NewFileSet()
	for _, rel := range listFiles(t, dir) {
		if !strings.HasSuffix(rel, ".go") {
			continue
		}
		if _, err := parser.ParseFile(fset, filepath.Join(dir, rel), nil, parser.ParseComments); err != nil {
			t.Errorf("%s 语法错误: %v", rel, err)
		}
	}
}

func readFile(t *testing.T, dir, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("读取 %s: %v", rel, err)
	}
	return string(data)
}

func mustContain(t *testing.T, rel, content string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(content, w) {
			t.Errorf("%s 应包含 %q", rel, w)
		}
	}
}

// appkitRoot 返回本仓库根目录（测试运行目录是 internal/scaffold）。
func appkitRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// writeGoWork 在生成仓库里写 go.work（use 本地 appkit），返回消解过符号链接的
// 仓库目录与 go.work 路径——go 工具按真实路径匹配 go.work 条目，macOS 的临时
// 目录是符号链接，不消解会匹配不上。
func writeGoWork(t *testing.T, dir string) (string, string) {
	t.Helper()
	dir = resolvePath(dir)
	goVersion := strings.TrimPrefix(runtime.Version(), "go")
	if !strings.HasPrefix(goVersion, "1.") {
		goVersion = "1.26" // devel 工具链等非常规版本号
	}
	work := "go " + goVersion + "\n\nuse (\n\t.\n\t" + appkitRoot(t) + "\n)\n"
	workFile := filepath.Join(dir, "go.work")
	if err := os.WriteFile(workFile, []byte(work), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, workFile
}

// buildGenerated 在生成仓库里写 go.work（use 本地 appkit）后 go build ./...。
// 依赖全部在本机 module cache（GOPROXY=off 离线构建）；环境导致的
// 下载/校验失败降级为跳过——生成物本身的语法已由 parser 断言兜底。
func buildGenerated(t *testing.T, dir string) {
	t.Helper()
	dir, workFile := writeGoWork(t, dir)
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK="+workFile, "GOFLAGS=", "GOPROXY=off")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return
	}
	msg := string(out)
	for _, marker := range []string{
		"missing go.sum entry", "GOPROXY", "proxy", "dial tcp",
		"connection refused", "no such host", "lookup ",
	} {
		if strings.Contains(msg, marker) {
			t.Skipf("环境无法离线编译生成仓库（已降级为 parser 校验）：%v\n%s", err, msg)
		}
	}
	t.Fatalf("生成仓库编译失败: %v\n%s", err, msg)
}

// assertGofmt 断言生成的 .go 文件 gofmt 归一（生成期已过 format.Source）。
func assertGofmt(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("gofmt", "-l", ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gofmt -l: %v\n%s", err, out)
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		t.Errorf("以下文件不符合 gofmt：\n%s", s)
	}
}
