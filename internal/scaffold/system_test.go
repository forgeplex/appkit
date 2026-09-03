package scaffold

import (
	"path/filepath"
	"strings"
	"testing"
)

// systemWantFiles 是组合仓库骨架的完整文件集（name=psp）。
var systemWantFiles = []string{
	".appkit.yml",
	".gitignore",
	"Makefile",
	"README.md",
	"AGENTS.md",
	"CLAUDE.md",
	"go.mod",
	"cmd/psp/main.go",
	"config/dev.yaml",
	"config/prod.yaml",
	"deploy/README.md",
}

func TestSystemScaffold(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "psp")
	if err := System(Options{Name: "psp", Dir: dir, AppkitVersion: "(devel)"}, nil); err != nil {
		t.Fatalf("System: %v", err)
	}

	assertFileSet(t, dir, systemWantFiles)
	assertRendered(t, dir)
	assertGoParses(t, dir)
	assertGofmt(t, dir)

	t.Run("appkit.yml 组合仓库标记", func(t *testing.T) {
		yml := readFile(t, dir, ".appkit.yml")
		mustContain(t, ".appkit.yml", yml,
			"version: 1",
			"kind: system",
			`domain: ""`,
			"module: github.com/forgeplex/psp",
		)
	})

	t.Run("main.go 组合根样例", func(t *testing.T) {
		main := readFile(t, dir, "cmd/psp/main.go")
		// 组合根只声明装什么、绑谁；-target/配置/遥测/连接池/关停都在
		// bootstrap 里（module cache 只读），生成物里改不到。
		mustContain(t, "cmd/psp/main.go", main,
			"bootstrap.Main(bootstrap.Options{",
			`Service: "psp"`,
			"ledger.Module(",     // Modules 注释样例
			"appkit.Remote(",     // Remote 注释样例
			"bootstrap.EventBus", // 换总线注释样例
		)
	})

	t.Run("go.mod devel 提示联调", func(t *testing.T) {
		gomod := readFile(t, dir, "go.mod")
		mustContain(t, "go.mod", gomod,
			"module github.com/forgeplex/psp", "appkit dev", "forgeplex/ledger")
		// devel 模式不 require appkit（由 go.work 提供），与域仓库一致。
		if strings.Contains(gomod, "require github.com/forgeplex/appkit") {
			t.Errorf("devel 模式 go.mod 不应 require appkit：\n%s", gomod)
		}
	})

	t.Run("配置与部署说明", func(t *testing.T) {
		mustContain(t, "config/dev.yaml", readFile(t, dir, "config/dev.yaml"), "env: dev")
		mustContain(t, "Makefile", readFile(t, dir, "Makefile"), "run-minimal:", "-minimal")
		mustContain(t, "config/prod.yaml", readFile(t, dir, "config/prod.yaml"),
			"env: prod", "pprof: false")
		mustContain(t, "deploy/README.md", readFile(t, dir, "deploy/README.md"),
			"-target=all", "-target=relay",
			// 迁移与告警是部署时才想起来的两件事，写在这里而不是散在别处。
			"-migrate", "appkit.SkipMigrations()", "MIGRATION_DRIFT",
			"appkit.outbox.oldest_pending.age")
	})
}

// TestSystemCompiles 用 go.work（use 本地 appkit）完整编译生成仓库。
func TestSystemCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过编译")
	}
	dir := filepath.Join(t.TempDir(), "psp")
	if err := System(Options{Name: "psp", Dir: dir}, nil); err != nil {
		t.Fatalf("System: %v", err)
	}
	buildGenerated(t, dir)
}
