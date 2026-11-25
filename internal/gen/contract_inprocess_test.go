package gen

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestContractInProcessMode(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{
			name: "无 path 的 inprocess 契约",
			yaml: `version: 1
mode: inprocess
package: billingv1
system: billing
types:
  - name: Entry
    doc: 分录视图。
    fields:
      - { name: id, type: string, required: true }
methods:
  - name: Charge
    doc: 执行扣费。
    idempotent: true
    request:
      - { name: account_id, type: string, required: true }
      - { name: amount, type: decimal }
    response:
      - { name: entries, type: "[]Entry" }
`,
		},
		{
			name: "带保留 path 的 inprocess 契约",
			yaml: `version: 1
mode: inprocess
package: billingv1
system: billing
methods:
  - name: Ping
    path: /ping
    doc: 健康检查。
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			in := filepath.Join(dir, "contract.yaml")
			out := filepath.Join(dir, "billingv1")
			if err := os.WriteFile(in, []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := Contract(in, out); err != nil {
				t.Fatalf("Contract: %v", err)
			}
			if err := CheckContract(in, out); err != nil {
				t.Fatalf("CheckContract: %v", err)
			}
			entries, err := os.ReadDir(out)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 2 {
				t.Fatalf("期望仅生成 2 个文件，实际 %d", len(entries))
			}
			for _, name := range []string{"service.gen.go", "wrap.gen.go"} {
				p := filepath.Join(out, name)
				fset := token.NewFileSet()
				f, err := parser.ParseFile(fset, p, nil, parser.AllErrors)
				if err != nil {
					t.Fatalf("解析 %s: %v", name, err)
				}
				for _, imp := range f.Imports {
					v, _ := strconv.Unquote(imp.Path.Value)
					if v == "net/http" || strings.HasPrefix(v, "net/http/") {
						t.Errorf("%s 在 inprocess 模式下不得 import %s", name, v)
					}
				}
			}
			for _, unexpected := range []string{"client.gen.go", "server.gen.go", "openapi.yaml"} {
				if _, err := os.Stat(filepath.Join(out, unexpected)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("inprocess 模式不应生成 %s", unexpected)
				}
			}
		})
	}

	t.Run("生成产物可独立 go build 编译通过", func(t *testing.T) {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		mod := strings.Replace(string(mustReadCompat(t, filepath.Join(root, "go.mod"))), "module github.com/forgeplex/appkit", "module example.com/inprocessfixture", 1)
		mod += "\nrequire github.com/forgeplex/appkit v0.0.0\nreplace github.com/forgeplex/appkit => \"" + root + "\"\n"
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "go.sum"), mustReadCompat(t, filepath.Join(root, "go.sum")), 0o600); err != nil {
			t.Fatal(err)
		}
		in := filepath.Join(dir, "contract.yaml")
		if err := os.WriteFile(in, []byte(cases[0].yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := Contract(in, filepath.Join(dir, "billingv1")); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", "build", "./billingv1")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOENV=off", "GOWORK=off", "GOFLAGS=-mod=readonly", "GOPROXY=off")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("inprocess contract compile failed: %v\n%s", err, out)
		}
	})
}

func TestContractInProcessInvalidAndDrift(t *testing.T) {
	dir := t.TempDir()
	badMode := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(badMode, []byte(`version: 1
mode: invalid
package: p
system: s
methods:
  - { name: M, doc: d }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RenderContract(badMode); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("期望 mode 非法报错，实际 %v", err)
	}

	// full -> inprocess 后目录残留 client.gen.go 时，CheckContract 必须检出 unexpected 漂移。
	in := filepath.Join(dir, "contract.yaml")
	out := filepath.Join(dir, "out")
	if err := os.WriteFile(in, []byte(`version: 1
mode: inprocess
package: p
system: s
methods:
  - { name: M, doc: d }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Contract(in, out); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "client.gen.go"), []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := CheckContract(in, out)
	if !errors.Is(err, ErrContractDrift) || !strings.Contains(err.Error(), "unexpected client.gen.go") {
		t.Fatalf("期望 unexpected client.gen.go 漂移报错，实际 %v", err)
	}
}

func TestContractInProcessCompatibility(t *testing.T) {
	inprocA := []byte(`version: 1
mode: inprocess
package: billingv1
system: billing
methods:
  - name: Charge
    doc: 执行扣费。
    request:
      - { name: id, type: string, required: true }
`)
	inprocB := []byte(`version: 1
mode: inprocess
package: billingv1
system: billing
methods:
  - name: Charge
    doc: 执行扣费。
    request:
      - { name: id, type: string, required: true }
      - { name: memo, type: string }
`)
	fullVer := []byte(`version: 1
mode: full
package: billingv1
system: billing
methods:
  - name: Charge
    path: /charge
    doc: 执行扣费。
    request:
      - { name: id, type: string, required: true }
`)

	if err := CheckContractCompatibilitySources("old", inprocA, "new", inprocB); err != nil {
		t.Fatalf("inprocess -> inprocess 添加可选字段应兼容: %v", err)
	}
	if err := CheckContractCompatibilitySources("old", inprocA, "new", fullVer); err != nil {
		t.Fatalf("inprocess -> full 升级应兼容: %v", err)
	}
	err := CheckContractCompatibilitySources("old", fullVer, "new", inprocA)
	if !errors.Is(err, ErrContractIncompatible) || !strings.Contains(err.Error(), "mode_downgraded:full->inprocess") {
		t.Fatalf("full -> inprocess 降级应报不兼容，实际: %v", err)
	}
}

func mustReadCompat(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
