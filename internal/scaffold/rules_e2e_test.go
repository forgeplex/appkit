package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forgeplex/appkit/ruleset"
)

// rulesE2EEnv 是这个测试的开关：跑一次要拉起两个真检查器，慢且要网络。
const rulesE2EEnv = "APPKIT_RULES_E2E"

// e2eDomain 是测试域名。刻意不叫 ledger——与其它测试的临时仓库区分开，
// 失败信息里一眼看得出是哪个测试留下的。
const e2eDomain = "shield"

const e2eModule = "github.com/forgeplex/" + e2eDomain

// subpkgFiles 是「让每个组件都长出一个子包」的附加文件。
//
// 为什么每个组件都要：模板的 components 给每一个组件都写了 `xxx/**`，
// 那就是在声明「这个组件会有子包」。测试要覆盖的正是模板自己许下的形态，
// 不多也不少——只给 domain 加子包的话，postgres/http/inbox/module 的规则
// 依旧只在「恰好一个包」的骨架上验证过，等于什么也没证明。
//
// 每个子包都被父包用上（且全部导出），否则 unused 会报，把 0 issue 的
// 断言变成噪音。
var subpkgFiles = map[string]string{
	// domain：子包互相 import——这条边最典型（auth 仓里有 20+ 个）。
	"internal/" + e2eDomain + "/cryptox/cryptox.go": `// Package cryptox 是域内的纯函数子包。
package cryptox

// HashToken 把明文令牌折算成可存储的摘要。
func HashToken(tok string) string { return "h:" + tok }
`,
	"internal/" + e2eDomain + "/session/manager.go": `// Package session 是域内子包，依赖同域的 cryptox。
package session

import (
	"context"

	"` + e2eModule + `/internal/` + e2eDomain + `/cryptox"
)

// Store 是本子包自己声明的持久化接口（实现在 internal/postgres）。
type Store interface {
	LiveByTokenHash(ctx context.Context, hash string) (string, error)
}

// Manager 编排会话用例。
type Manager struct{ store Store }

// New 构造 Manager。
func New(s Store) *Manager { return &Manager{store: s} }

// Lookup 查一次活跃会话。错误原样返回：跨层流动的已经是 *apperr.Error，
// 再包一层只会把错误码埋进 message（这正是 wrapcheck 必须放行的形态）。
func (m *Manager) Lookup(ctx context.Context, token string) (string, error) {
	found, err := m.store.LiveByTokenHash(ctx, cryptox.HashToken(token))
	if err != nil {
		return "", err
	}
	return found, nil
}
`,
	// postgres：真实域里这个子包是 sqlc 生成物，store.go 每次查询都 import 它。
	"internal/postgres/sqlc/sqlc.go": `// Package sqlc 模拟 sqlc 生成物所在的子包。
package sqlc

// Queries 是 sqlc 生成的查询集合。
type Queries struct{}

// New 构造 Queries。
func New() *Queries { return &Queries{} }
`,
	"internal/postgres/queries.go": `package postgres

import "` + e2eModule + `/internal/postgres/sqlc"

// Queries 暴露 sqlc 查询集合。
func Queries() *sqlc.Queries { return sqlc.New() }
`,
	// http：auth 仓里就有 internal/http/safehttp。
	"internal/http/safehttp/safehttp.go": `// Package safehttp 是 transport 层的子包。
package safehttp

// SanitizePath 归一化外部传入的路径片段。
func SanitizePath(p string) string { return "/" + p }
`,
	"internal/http/paths.go": `package http

import "` + e2eModule + `/internal/http/safehttp"

// Path 归一化一个路径片段。
func Path(p string) string { return safehttp.SanitizePath(p) }
`,
	// inbox：外域事件的解码逻辑拆子包是常见形态。
	"internal/inbox/decode/decode.go": `// Package decode 是消费者的解码子包。
package decode

// Topic 从事件名解出 topic。
func Topic(name string) string { return name }
`,
	"internal/inbox/topics.go": `package inbox

import "` + e2eModule + `/internal/inbox/decode"

// Topic 解出事件的 topic。
func Topic(name string) string { return decode.Topic(name) }
`,
	// module：组合根拆子包不常见，但模板写了 module/**，就得照着验。
	"internal/module/wire/wire.go": `// Package wire 是装配层的子包。
package wire

// Name 返回装配单元名。
func Name() string { return "` + e2eDomain + `" }
`,
	"internal/module/unit.go": `package module

import "` + e2eModule + `/internal/module/wire"

// Unit 返回装配单元名。
func Unit() string { return wire.Name() }
`,
}

// ruleProbe 是一条「必须被报出来」的已知违规。
//
// 没有这一组，前半段的 0 issue 断言是空的——把规则全部关掉同样能过。
type ruleProbe struct {
	name   string // 失败信息用
	path   string // 植入的文件（其 base name 必须出现在检查器输出里）
	body   string
	linter string // "golangci" | "archlint"
	token  string // 同一行里必须出现的规则名/原因
}

var ruleProbes = []ruleProbe{
	{
		name:   "业务子包 import pgx",
		path:   "internal/" + e2eDomain + "/session/probe_depguard.go",
		linter: "golangci",
		token:  "depguard",
		body: `package session

import "github.com/jackc/pgx/v5"

// ProbeRow 让业务子包持有驱动类型——DESIGN §4 明令禁止。
type ProbeRow struct{ Row pgx.Row }
`,
	},
	{
		name:   "业务子包 import internal/postgres",
		path:   "internal/" + e2eDomain + "/session/probe_archlint.go",
		linter: "archlint",
		token:  "shouldn't depend on",
		body: `package session

import "` + e2eModule + `/internal/postgres"

// ProbeStore 让业务子包直连 repo 层——方向矩阵禁止 domain → postgres。
func ProbeStore() *postgres.Store { return nil }
`,
	},
	{
		name:   "transport import repo 层",
		path:   "internal/http/probe_archlint.go",
		linter: "archlint",
		token:  "shouldn't depend on",
		body: `package http

import "` + e2eModule + `/internal/postgres"

// ProbeStore 让 handler 绕过业务包直连 repo 层。
func ProbeStore() *postgres.Store { return nil }
`,
	},
	{
		name:   "子包漏出外部包的裸错误",
		path:   "internal/" + e2eDomain + "/cryptox/probe_wrapcheck.go",
		linter: "golangci",
		token:  "wrapcheck",
		body: `package cryptox

import "encoding/json"

// ProbeDecode 把 encoding/json 的错误原样漏出边界——放宽域内 import 之后，
// 真正的外部包裸错误必须仍然被抓，否则那次放宽就是把规则关掉了。
func ProbeDecode(raw []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}
`,
	},
	{
		name:   "业务包里 fmt.Print",
		path:   "internal/" + e2eDomain + "/session/probe_forbidigo.go",
		linter: "golangci",
		token:  "forbidigo",
		body: `package session

import "fmt"

// ProbeLog 用 fmt.Print 代替注入的 slog。
func ProbeLog() { fmt.Println("x") }
`,
	},
}

// TestMaterializedRulesOnSubpackagedDomain 用真检查器验证物化的规则集在
// 「有子包的域」上成立——appkit 只生产规则集、自己不消费，这是唯一的消费点。
//
// 为什么需要它：ruleset 原本只有 golden 渲染测试，锁的是「模板文本没变」。
// 那防得住「无意中把规则改松」，防不住「规则从写下来那天就是错的」。v0.2.1
// 修的两条误报就是后者——刚生成的骨架每个组件恰好一个包，组件内的边一条都
// 不存在，两个缺陷都不显形；等下游把域拆出 20+ 个子包才炸。
//
// 两段：
//  1. 干净仓库（每个组件都带子包）跑两个检查器，必须 0 问题；
//  2. 植入已知违规，每一条都必须被报出来——否则第 1 段等于把规则关掉也能过。
func TestMaterializedRulesOnSubpackagedDomain(t *testing.T) {
	if os.Getenv(rulesE2EEnv) == "" {
		t.Skipf("跳过：这个测试要拉起两个真检查器（慢且需要网络）。跑它用 make test-rules，"+
			"或设 %s=1", rulesE2EEnv)
	}

	dir := filepath.Join(t.TempDir(), e2eDomain)
	if err := Domain(Options{Name: e2eDomain, Dir: dir, AppkitVersion: "(devel)"}, nil); err != nil {
		t.Fatalf("Domain: %v", err)
	}
	dir, workFile := writeGoWork(t, dir)
	writeFiles(t, dir, subpkgFiles)
	mustCompile(t, dir, workFile)

	t.Run("有子包的域必须零问题", func(t *testing.T) {
		for _, l := range []string{"golangci", "archlint"} {
			out, err := runLinter(t, dir, workFile, l)
			if err != nil {
				t.Errorf("[%s] 干净的生成仓库不该有问题，但检查器报了 %v：\n%s\n"+
					"——规则集与它服务的架构（每个组件都可能有子包）自相矛盾", l, err, out)
			}
		}
	})

	t.Run("已知违规必须被报出", func(t *testing.T) {
		for _, p := range ruleProbes {
			writeFiles(t, dir, map[string]string{p.path: p.body})
		}
		out := map[string]string{}
		for _, l := range []string{"golangci", "archlint"} {
			body, err := runLinter(t, dir, workFile, l)
			if err == nil {
				t.Errorf("[%s] 植入违规后检查器仍然全绿——规则是空的", l)
			}
			out[l] = body
		}
		for _, p := range ruleProbes {
			if !hasLineWith(out[p.linter], filepath.Base(p.path), p.token) {
				t.Errorf("探针 %q 没被 %s 报出（期望某一行同时含 %q 与 %q）：\n%s",
					p.name, p.linter, filepath.Base(p.path), p.token, out[p.linter])
			}
		}
	})
}

// TestRulesE2EWiredIntoCI 守住上面那个测试真的会被执行。
//
// 它是 opt-in 的，而「配置物化了却从没被执行」正是它要防的那类事故——所以这里
// 反过来钉住 appkit 自己的 CI 必须调 make test-rules，Makefile 必须设开关。
// 不这么钉，这个测试很容易变成又一份写下来就没人跑的规则。
func TestRulesE2EWiredIntoCI(t *testing.T) {
	tests := []struct {
		path  string
		wants []string
	}{
		{"../../.github/workflows/ci.yml", []string{"make test-rules"}},
		{"../../Makefile", []string{"test-rules:", rulesE2EEnv + "=1"}},
	}
	for _, tt := range tests {
		body, err := os.ReadFile(tt.path)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range tt.wants {
			if !strings.Contains(string(body), w) {
				t.Errorf("%s 缺少 %q——%s 会变成没人执行的测试",
					tt.path, w, "TestMaterializedRulesOnSubpackagedDomain")
			}
		}
	}
}

// writeFiles 把 path→内容 写进生成仓库（按需建目录）。
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// mustCompile 断言加了子包之后仓库仍能编译——检查器要类型信息，
// 编译不过的话后面两段断言测的就不是规则而是语法。
func mustCompile(t *testing.T, dir, workFile string) {
	t.Helper()
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK="+workFile, "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("加了子包的生成仓库编译失败: %v\n%s", err, out)
	}
}

// runLinter 在生成仓库里跑一个钉版本的检查器，返回合并输出与退出状态。
// 版本取自 ruleset——与域仓库 Makefile、domain-ci.yml 同源。
// 拉不到检查器（无网络等）降级为跳过，与 buildGenerated 同策略。
func runLinter(t *testing.T, dir, workFile, which string) (string, error) {
	t.Helper()
	var args []string
	switch which {
	case "golangci":
		args = []string{"run", "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@" +
			ruleset.GolangciLintVersion, "run", "./..."}
	case "archlint":
		args = []string{"run", "github.com/fe3dback/go-arch-lint@" +
			ruleset.ArchLintVersion, "check"}
	default:
		t.Fatalf("未知检查器 %q", which)
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK="+workFile, "GOFLAGS=")
	out, err := cmd.CombinedOutput()
	msg := string(out)
	if err != nil {
		for _, marker := range []string{
			"dial tcp", "connection refused", "no such host", "lookup ",
			"proxy", "certificate",
		} {
			if strings.Contains(msg, marker) {
				t.Skipf("环境拉不到 %s（%v）：\n%s", which, err, msg)
			}
		}
	}
	return msg, err
}

// hasLineWith 报告输出里是否有某一行同时含 a 与 b。
// 逐行比对而非整段 Contains：多条探针共用同一个 token（如两条 archlint 探针
// 都报 "shouldn't depend on"），整段匹配会让漏报的那条蒙混过关。
func hasLineWith(out, a, b string) bool {
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, a) && strings.Contains(line, b) {
			return true
		}
	}
	return false
}
