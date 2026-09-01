// Package doctor 诊断本机/CI 是否满足 appkit 多私有仓库开发的前置条件。
//
// 针对的就是最常见的翻车现场：GOPRIVATE 没设 → go 去 sum.golang.org 校验私有
// 模块得到 404；git 走匿名 https → "could not read Username"。每项检查都给出
// 可直接复制的修复命令。
package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

type Status int

const (
	OK Status = iota
	Warn
	Fail
)

// Check 是一项诊断结果。Fix 在 Status != OK 时给出可执行的修复指引。
type Check struct {
	Name   string
	Status Status
	Detail string
	Fix    string
}

// Options 的注入点仅供测试；零值字段使用真实实现。
type Options struct {
	Dir    string // 待诊断的仓库目录；空 = 只做全局环境检查
	Prefix string // 私有 module 前缀，默认 github.com/forgeplex

	GoEnv     func(dir string, keys ...string) (map[string]string, error)
	GitConfig func() (string, error) // git config --get-regexp ^url\.
	LookPath  func(file string) (string, error)
	ReadFile  func(name string) ([]byte, error)
	Home      string
}

func (o Options) withDefaults() Options {
	if o.Prefix == "" {
		o.Prefix = "github.com/forgeplex"
	}
	if o.GoEnv == nil {
		o.GoEnv = realGoEnv
	}
	if o.GitConfig == nil {
		o.GitConfig = realGitConfig
	}
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	if o.ReadFile == nil {
		o.ReadFile = os.ReadFile
	}
	if o.Home == "" {
		o.Home, _ = os.UserHomeDir()
	}
	return o
}

// Run 执行全部检查。任何一项 Fail 表示"接下来的构建/拉取必然出问题"。
func Run(o Options) []Check {
	o = o.withDefaults()
	var cs []Check

	env, err := o.GoEnv(o.Dir, "GOVERSION", "GOPRIVATE", "GOPROXY", "GOWORK")
	if err != nil {
		return append(cs, Check{
			Name: "go 工具链", Status: Fail,
			Detail: fmt.Sprintf("go env 执行失败: %v", err),
			Fix:    "确认 go 已安装且在 PATH 中",
		})
	}

	cs = append(cs, checkGoVersion(env["GOVERSION"]))
	cs = append(cs, checkGoPrivate(env["GOPRIVATE"], o.Prefix))
	cs = append(cs, checkGitAuth(o))
	if o.Dir != "" {
		if c, ok := checkWorkspace(o, env["GOWORK"]); ok {
			cs = append(cs, c)
		}
	}
	cs = append(cs, checkDocker(o))
	return cs
}

// HasFailure 报告是否存在 Fail 级检查（CLI 以此决定退出码）。
func HasFailure(cs []Check) bool {
	for _, c := range cs {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

func checkGoVersion(v string) Check {
	c := Check{Name: "Go 工具链", Detail: v}
	major, minor, ok := parseGoVersion(v)
	switch {
	case !ok:
		c.Status = Warn
		c.Detail = fmt.Sprintf("无法解析 go 版本 %q", v)
	case major > 1 || (major == 1 && minor >= 26):
		c.Status = OK
	default:
		c.Status = Fail
		c.Fix = "appkit 需要 Go 1.26+，请升级工具链"
	}
	return c
}

func parseGoVersion(v string) (major, minor int, ok bool) {
	v = strings.TrimPrefix(v, "go")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	return major, minor, err1 == nil && err2 == nil
}

func checkGoPrivate(goprivate, prefix string) Check {
	c := Check{Name: "GOPRIVATE", Detail: valueOr(goprivate, "(未设置)")}
	// 用与 go 工具链相同的匹配语义验证覆盖面。
	if module.MatchPrefixPatterns(goprivate, prefix+"/appkit") {
		c.Status = OK
		return c
	}
	c.Status = Fail
	c.Detail = fmt.Sprintf("%s 未覆盖 %s/*（go 会到公共代理与 sum.golang.org 找私有模块 → 404）", c.Detail, prefix)
	c.Fix = fmt.Sprintf("go env -w GOPRIVATE='%s/*'", prefix)
	return c
}

func checkGitAuth(o Options) Check {
	c := Check{Name: "git 私有仓库凭据"}
	httpsPrefix := "https://" + o.Prefix + "/"

	if out, err := o.GitConfig(); err == nil {
		for line := range strings.SplitSeq(out, "\n") {
			// 形如：url.git@github.com:forgeplex/.insteadof https://github.com/forgeplex/
			key, val, found := strings.Cut(strings.TrimSpace(line), " ")
			if !found || !strings.HasSuffix(strings.ToLower(key), ".insteadof") {
				continue
			}
			if strings.HasPrefix(httpsPrefix, val) || strings.HasPrefix(val, httpsPrefix) {
				c.Status = OK
				c.Detail = "insteadOf 已把 " + val + " 重写为 SSH"
				return c
			}
		}
	}
	host, _, _ := strings.Cut(o.Prefix, "/")
	if b, err := o.ReadFile(filepath.Join(o.Home, ".netrc")); err == nil &&
		strings.Contains(string(b), "machine "+host) {
		c.Status = OK
		c.Detail = "~/.netrc 已配置 " + host + " 凭据"
		return c
	}

	c.Status = Warn
	c.Detail = "未检测到 insteadOf 重写或 ~/.netrc 凭据；go 直连将走匿名 https" +
		"（报错特征：could not read Username for 'https://" + host + "'）。" +
		"若已配置 git 凭据助手可忽略本项"
	c.Fix = fmt.Sprintf("git config --global url.\"git@%s:%s/\".insteadOf \"%s\"",
		host, strings.TrimPrefix(o.Prefix, host+"/"), httpsPrefix)
	return c
}

// checkWorkspace 检查 appkit 依赖的解析来源：go.mod 未 require appkit（源码
// 构建的 CLI 生成的骨架形态）时，必须在 go.work 工作区内构建，否则 go 会去
// 远程解析一个不存在的版本。
func checkWorkspace(o Options, gowork string) (Check, bool) {
	data, err := o.ReadFile(filepath.Join(o.Dir, "go.mod"))
	if err != nil {
		return Check{}, false // 不在 Go module 里，跳过本项
	}
	mf, err := modfile.Parse("go.mod", data, nil)
	if err != nil || mf.Module == nil {
		return Check{Name: "工作区", Status: Warn, Detail: "go.mod 解析失败: " + fmt.Sprint(err)}, true
	}
	appkitPath := o.Prefix + "/appkit"
	if mf.Module.Mod.Path == appkitPath {
		return Check{}, false
	}
	for _, r := range mf.Require {
		if r.Mod.Path == appkitPath {
			return Check{Name: "工作区", Status: OK,
				Detail: "go.mod 已 require " + appkitPath + "@" + r.Mod.Version}, true
		}
	}
	if !dependsOnPrefix(mf, o.Prefix) && !importsPrefixLikely(o) {
		return Check{}, false
	}
	if gowork != "" && gowork != "off" {
		return Check{Name: "工作区", Status: OK, Detail: "go.work 生效: " + gowork}, true
	}
	return Check{Name: "工作区", Status: Fail,
		Detail: "go.mod 未 require " + appkitPath + " 且不在 go.work 工作区内——" +
			"构建时 go 会去远程解析 appkit，而远程没有可解析的版本（私有库另需 GOPRIVATE，见上方检查项）",
		Fix: "在仓库目录运行 appkit dev（父目录需有 appkit 的 checkout；或 appkit dev -root <多仓根目录>）",
	}, true
}

func dependsOnPrefix(mf *modfile.File, prefix string) bool {
	for _, r := range mf.Require {
		if strings.HasPrefix(r.Mod.Path, prefix+"/") {
			return true
		}
	}
	return false
}

// importsPrefixLikely 粗探仓库是否 import 了 appkit（.appkit.yml 存在即认定是
// appkit 系仓库）。避免对无关仓库误报。
func importsPrefixLikely(o Options) bool {
	_, err := o.ReadFile(filepath.Join(o.Dir, ".appkit.yml"))
	return err == nil
}

func checkDocker(o Options) Check {
	c := Check{Name: "docker（集成测试用）"}
	if _, err := o.LookPath("docker"); err != nil {
		c.Status = Warn
		c.Detail = "未找到 docker；数据层集成测试需要一个 Postgres（TEST_DATABASE_URL）"
		c.Fix = "安装 Docker，或改用任何可达的 Postgres 实例"
		return c
	}
	c.Status = OK
	c.Detail = "已安装"
	return c
}

func valueOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func realGoEnv(dir string, keys ...string) (map[string]string, error) {
	args := append([]string{"env", "-json"}, keys...)
	cmd := exec.Command("go", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go env: %w", err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, fmt.Errorf("解析 go env 输出: %w", err)
	}
	return m, nil
}

func realGitConfig() (string, error) {
	out, err := exec.Command("git", "config", "--get-regexp", `^url\.`).Output()
	return string(out), err
}
