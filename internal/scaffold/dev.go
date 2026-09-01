package scaffold

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"
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
	// GOPRIVATE 影响「已 require 发版版本但私有库走代理」那一条提示；
	// 读不到就当未设置——提示宁可多打一条，无害。
	warnAppkitMissing(absDir, uses, goEnvValue(absDir, "GOPRIVATE"), out)
	return nil
}

// appkitModulePath 是 appkit 的模块路径；require 与 GOPRIVATE 检查都以它为准。
const appkitModulePath = "github.com/forgeplex/appkit"

// warnAppkitMissing 在依赖解析会出问题的形态下提示。appkit 发版后，骨架
// go.mod 里写的是具体 tag，工作区不含 appkit 本体是合法形态（解析走远程）；
// 只剩两种情况需要说话：
//   - go.mod 未 require appkit（源码构建的 CLI 生成的骨架形态）：远程没有
//     版本可拉，appkit 本体必须进工作区；
//   - 已 require 但 GOPRIVATE 未覆盖 forgeplex 模块路径：私有仓库经模块
//     代理拉取会被拒。doctor 的 GOPRIVATE 检查管同一件事，这里在建工作区
//     的现场顺手提一次。
func warnAppkitMissing(absDir string, uses []string, goprivate string, out io.Writer) {
	for _, u := range uses {
		dir := u
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(absDir, dir)
		}
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(data), "module "+appkitModulePath+"\n") {
			return // 工作区已含 appkit 本体，解析在本地，无事可提
		}
	}
	v, ok := requiredAppkitVersion(absDir)
	if !ok {
		fmt.Fprintln(out, "提示：工作区未包含 appkit 本体，且 go.mod 未 require 发版的 appkit 版本——")
		fmt.Fprintln(out, "      依赖解析没有来源。把 appkit clone 到 -root 目录下重跑 appkit dev，或手动执行：")
		fmt.Fprintln(out, "      go work use <appkit checkout 路径>")
		return
	}
	if !goprivateCovers(goprivate, appkitModulePath) {
		fmt.Fprintf(out, "提示：go.mod 已 require %s %s，但 GOPRIVATE 未覆盖 forgeplex 模块路径——\n", appkitModulePath, v)
		fmt.Fprintln(out, "      私有仓库经模块代理拉取会被拒，先执行：go env -w GOPRIVATE=github.com/forgeplex/*")
	}
}

// requiredAppkitVersion 返回 go.mod 里 require 的 appkit 版本。
// v0.0.0-00010101… 占位版本（手工把 require 钉向工作区的写法）不算——它
// 本身就要靠工作区/replace 才能解析，按未 require 处理。
func requiredAppkitVersion(dir string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", false
	}
	mf, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return "", false
	}
	for _, r := range mf.Require {
		if r.Mod.Path == appkitModulePath && !strings.HasPrefix(r.Mod.Version, "v0.0.0-00010101") {
			return r.Mod.Version, true
		}
	}
	return "", false
}

// goprivateCovers 报告 GOPRIVATE 的模式列表是否覆盖 module。模式是逗号分隔的
// glob（如 github.com/forgeplex/*），按模块路径前缀或 path.Match 匹配；
// 提示用途，语义只需贴近 go 命令，不必逐字复刻。
func goprivateCovers(goprivate, module string) bool {
	for pat := range strings.SplitSeq(goprivate, ",") {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if strings.HasPrefix(module, pat) {
			return true
		}
		if ok, _ := path.Match(pat, module); ok {
			return true
		}
	}
	return false
}

// goEnvValue 在 dir 下读一个 go env 值；失败返回空串。
func goEnvValue(dir, name string) string {
	cmd := exec.Command("go", "env", name)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
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
