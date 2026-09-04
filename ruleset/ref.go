package ruleset

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

const appkitModule = "github.com/forgeplex/appkit"

var commitSHARE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ResolveWorkflowRef 把 appkit 版本解析为其源码 commit。生成的 reusable
// workflow 只写完整 SHA；版本仅作为审计注释，绝不作为可执行引用。
func ResolveWorkflowRef(version string) (string, error) {
	if version == "" || version == "(devel)" {
		out, err := exec.Command("git", "rev-parse", "HEAD").Output()
		if err != nil {
			return "", fmt.Errorf("解析 devel workflow commit（请在 appkit git worktree 中运行）: %w", err)
		}
		return validateWorkflowRef(strings.TrimSpace(string(out)))
	}

	cmd := exec.Command("go", "mod", "download", "-json", appkitModule+"@"+version)
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
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

func validateWorkflowRef(ref string) (string, error) {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if !commitSHARE.MatchString(ref) {
		return "", fmt.Errorf("workflow ref %q 不合法：须为完整 40 位 commit SHA", ref)
	}
	return ref, nil
}
