package scaffold

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Guidance is part of the generated agent interface: both partitioned domain
// variants must advertise the supported logical template, not a runtime scan.
// Build and run the real CLI against every scaffold shape as well; parser-only
// checks cannot prove that generated repositories compile or pass appkit check.
func TestSchemaGuidanceScaffoldMatrix(t *testing.T) {
	var cliBinary string
	if !testing.Short() {
		cliBinary = filepath.Join(t.TempDir(), "appkit")
		if runtime.GOOS == "windows" {
			cliBinary += ".exe"
		}
		runSchemaGuidanceCommand(t, appkitRoot(t), "off", "go", "build", "-buildvcs=false", "-o", cliBinary, "./cmd/appkit")
	}
	for _, tc := range []struct {
		name        string
		tenant      bool
		partitioned bool
		system      bool
	}{
		{name: "ordinary"},
		{name: "tenant", tenant: true},
		{name: "partitioned", partitioned: true},
		{name: "partitioned-tenant", tenant: true, partitioned: true},
		{name: "system", system: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "sample")
			opts := Options{Name: "sample", Dir: dir, AppkitVersion: "(devel)", Tenant: tc.tenant, Partitioned: tc.partitioned}
			generate := Domain
			if tc.system {
				generate = System
			}
			if err := generate(opts, nil); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{".appkit.yml", "README.md", "AGENTS.md"} {
				content := readFile(t, dir, name)
				for _, stale := range []string{"schema 暂不支持", "schema 文档暂不支持", "make schema` 不适用本形态"} {
					if strings.Contains(content, stale) {
						t.Errorf("%s contains stale schema guidance %q", name, stale)
					}
				}
				if tc.partitioned {
					mustContain(t, name, content, "logical-template", "未枚举或检查运行时分区")
				} else if strings.Contains(content, "logical-template") {
					t.Errorf("%s advertises partitioned mode on a nonpartitioned scaffold", name)
				}
				if !tc.system && name != ".appkit.yml" {
					mustContain(t, name, content, "make schema", "db/SCHEMA.md", "db/schema/", "COMMENT ON TABLE")
				}
				if tc.tenant && name != ".appkit.yml" {
					mustContain(t, name, content, "RLS 策略", "tx.WithReadAllTenants")
				}
			}
			assertRendered(t, dir)
			assertGoParses(t, dir)
			mustContain(t, "config/dev.yaml", readFile(t, dir, "config/dev.yaml"), "security:", "mode: disabled", "staging/prod")
			mustContain(t, "Makefile", readFile(t, dir, "Makefile"), "run-minimal:", "-minimal")
			if !tc.system {
				for _, name := range []string{"cmd/sampled/main.go", "internal/module/module.go"} {
					content := readFile(t, dir, name)
					mustContain(t, name, content, "reg.MountPublic(")
					if strings.Contains(content, "reg.Mount(") {
						t.Errorf("%s has an unclassified root route", name)
					}
				}
				mustContain(t, "internal/module/module.go", readFile(t, dir, "internal/module/module.go"),
					"reg.Permissions(sample.PermissionCatalog()...)", "reg.MountPermission(")
				mustContain(t, "AGENTS.md", readFile(t, dir, "AGENTS.md"), "security.mode", "reg.MountAuthenticated", "reg.MountInternalService")
				if tc.partitioned {
					mustContain(t, "internal/module/module.go", readFile(t, dir, "internal/module/module.go"),
						"callctx.From(ctx).Partition", "unsigned X-Partition", "已验证 principal")
				}
			}
			if !testing.Short() {
				dir, workFile := writeGoWork(t, dir)
				runSchemaGuidanceCommand(t, dir, workFile, "go", "build", "-buildvcs=false", "./...")
				// The source-built CLI resolves its devel workflow provenance
				// from this framework checkout, not the new (non-git) project.
				runSchemaGuidanceCommand(t, appkitRoot(t), workFile, cliBinary, "check", "-dir", dir)
			}
		})
	}
}

func runSchemaGuidanceCommand(t *testing.T, dir, workFile, command string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOENV=off", "GOWORK="+workFile, "GOFLAGS=", "GOPROXY=off", "GOSUMDB=off",
		"GOPRIVATE=", "GONOPROXY=none", "GOVCS=*:off", "GOTOOLCHAIN=local", "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s failed (offline cached dependencies required): %v\n%s", command, strings.Join(args, " "), err, output)
	}
}
