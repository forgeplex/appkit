package ruleset

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"time"
)

const appkitModule = "github.com/forgeplex/appkit"

var commitSHARE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ResolveWorkflowRef 把 appkit 版本解析为其源码 commit。生成的 reusable
// workflow 只写完整 SHA；版本仅作为审计注释，绝不作为可执行引用。
func ResolveWorkflowRef(version string) (string, error) {
	return ResolveWorkflowRefContext(context.Background(), version)
}

// ResolveWorkflowRefContext resolves before rendering or taking workspace locks.
// Downloads use an isolated scratch directory, never the caller's go.mod/go.sum.
func ResolveWorkflowRefContext(ctx context.Context, version string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if version == "" || version == "(devel)" {
		if info, ok := debug.ReadBuildInfo(); ok {
			if ref, found, err := appkitBuildWorkflowRef(info); found || err != nil {
				return ref, err
			}
		}
		return develWorkflowRef(ctx)
	}

	dir, err := os.MkdirTemp("", "appkit-workflow-ref-")
	if err != nil {
		return "", fmt.Errorf("准备 workflow 来源解析: %w", err)
	}
	defer os.RemoveAll(dir)
	cmd := exec.CommandContext(ctx, "go", "mod", "download", "-json", appkitModule+"@"+version)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=", "GO111MODULE=on")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("解析 appkit %s workflow commit: %w", version, err)
	}
	var result struct {
		Origin struct {
			Hash string `json:"Hash"`
		} `json:"Origin"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return "", fmt.Errorf("解析 go mod download 输出: %w", err)
	}
	if result.Origin.Hash == "" {
		return "", fmt.Errorf("appkit %s 缺少模块来源 commit，拒绝生成可移动 workflow 引用", version)
	}
	return validateWorkflowRef(result.Origin.Hash)
}

// NormalizeWorkflowRef validates and normalizes an explicit immutable reference
// without running commands or accessing the filesystem/network.
func NormalizeWorkflowRef(ref string) (string, error) { return validateWorkflowRef(ref) }

func appkitBuildWorkflowRef(info *debug.BuildInfo) (string, bool, error) {
	// A consumer binary's VCS revision belongs to the consumer, not its AppKit
	// dependency. Test binaries may also have no module provenance at all.
	if info == nil || info.Main.Path != appkitModule {
		return "", false, nil
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			ref, err := validateWorkflowRef(setting.Value)
			return ref, true, err
		}
	}
	return "", false, nil
}

var appkitModuleLineRE = regexp.MustCompile(`(?m)^module[\t ]+(?:"github\.com/forgeplex/appkit"|github\.com/forgeplex/appkit)[\t ]*(?://[^\n]*)?\r?$`)

func develWorkflowRef(ctx context.Context) (string, error) {
	git := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.WaitDelay = time.Second
		out, err := cmd.Output()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return out, err
	}
	fail := func(err error) (string, error) {
		return "", fmt.Errorf("解析 devel workflow commit（需 AppKit 构建来源或 AppKit git worktree；也可显式提供 -workflow-ref）: %w", err)
	}
	out, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return fail(err)
	}
	root := strings.TrimSpace(string(out))
	current, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return fail(err)
	}
	out, err = git("-C", root, "rev-parse", "HEAD")
	if err != nil {
		return fail(err)
	}
	ref, err := validateWorkflowRef(strings.TrimSpace(string(out)))
	if err != nil {
		return fail(err)
	}
	// Bind identity to that exact commit, even if another process moves HEAD
	// between commands. Never validate one revision and return another.
	committed, err := git("-C", root, "show", ref+":go.mod")
	if err != nil {
		return fail(err)
	}
	if !appkitModuleLineRE.Match(current) || !appkitModuleLineRE.Match(committed) {
		return fail(fmt.Errorf("当前仓库不是 %s，拒绝使用下游 HEAD", appkitModule))
	}
	return ref, nil
}

func validateWorkflowRef(ref string) (string, error) {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if !commitSHARE.MatchString(ref) {
		return "", fmt.Errorf("workflow ref %q 不合法：须为完整 40 位 commit SHA", ref)
	}
	return ref, nil
}
