package scaffold

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Dev 维护本地多仓联调工作区（appkit dev）：
//   - 在 dir 下 go work init（已有 go.work 则跳过，幂等）；
//   - 扫描 root 下含 go.mod 的一级子目录，连同 dir 自身逐个 go work use。
//
// go.work 不提交（骨架 .gitignore 已忽略）；发版联动走 require + Renovate。
func Dev(dir, root string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("dev: 解析目录 %s: %w", dir, err)
	}
	// 消解符号链接（macOS /tmp、/var 等）：go 工具以真实路径计算相对
	// use 路径，混用两种写法会把 "." 写成一长串 ../..。
	absDir = resolvePath(absDir)
	workFile := filepath.Join(absDir, "go.work")
	if _, err := os.Stat(workFile); err == nil {
		fmt.Fprintf(out, "%s 已存在，跳过 init\n", workFile)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("dev: 检查 %s: %w", workFile, err)
	} else {
		if err := runGo(absDir, workFile, "work", "init"); err != nil {
			return fmt.Errorf("dev: %w", err)
		}
		fmt.Fprintf(out, "已创建 %s\n", workFile)
	}

	uses, err := collectModuleDirs(absDir, root)
	if err != nil {
		return fmt.Errorf("dev: %w", err)
	}
	if len(uses) == 0 {
		fmt.Fprintln(out, "未发现可纳入的 module 目录")
		return nil
	}
	if err := runGo(absDir, workFile, append([]string{"work", "use"}, uses...)...); err != nil {
		return fmt.Errorf("dev: %w", err)
	}
	fmt.Fprintf(out, "go.work 已纳入 %d 个 module 目录：%s\n", len(uses), strings.Join(uses, "、"))
	return nil
}

// collectModuleDirs 收集 go work use 的目标：dir 自身（有 go.mod 时，记作 "."）
// 与 root 下含 go.mod 的一级子目录（跳过与 dir 重复的自身）。
func collectModuleDirs(absDir, root string) ([]string, error) {
	var uses []string
	if fileExists(filepath.Join(absDir, "go.mod")) {
		uses = append(uses, ".")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("解析 root %s: %w", root, err)
	}
	absRoot = resolvePath(absRoot)
	entries, err := os.ReadDir(absRoot)
	if err != nil {
		return nil, fmt.Errorf("扫描 %s: %w", absRoot, err)
	}
	for _, e := range entries {
		sub := filepath.Join(absRoot, e.Name())
		if !e.IsDir() && !isSymlinkDir(sub) {
			continue
		}
		if resolvePath(sub) == absDir {
			continue // 自身已以 "." 纳入，避免同目录双写
		}
		if !fileExists(filepath.Join(sub, "go.mod")) {
			continue
		}
		uses = append(uses, sub)
	}
	return uses, nil
}

// resolvePath 消解符号链接后返回规范路径（macOS 的 /tmp → /private/tmp 等）。
func resolvePath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// isSymlinkDir 判断 path 是否为指向目录的符号链接（本地把兄弟仓库
// symlink 进 root 的场景）。
func isSymlinkDir(path string) bool {
	info, err := os.Stat(path) // Stat 跟随符号链接
	return err == nil && info.IsDir()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// runGo 在 dir 下执行 go 子命令；显式钉住 GOWORK 与 PWD——GOWORK 防外层
// 环境把工作区指到别处，PWD 防 go 工具从继承的 shell 环境读到符号链接
// 形式的旧 cwd（macOS /tmp）而把 "." 写成一长串 ../..。
func runGo(dir, workFile string, args ...string) error {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK="+workFile, "PWD="+dir)
	outBytes, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go %s（目录 %s）: %w\n%s", strings.Join(args, " "), dir, err, outBytes)
	}
	return nil
}
