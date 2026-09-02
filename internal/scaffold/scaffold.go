// Package scaffold 实现 appkit new / appkit dev：
// 生成域仓库与组合仓库骨架（embed 模板 + text/template），
// 以及本地多仓联调的 go.work 维护。
package scaffold

import (
	"bytes"
	"embed"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"text/template"

	"golang.org/x/mod/module"

	"github.com/forgeplex/appkit/ruleset"
)

//go:embed templates
var templatesFS embed.FS

// Options 是 appkit new 的输入。
type Options struct {
	// Name 是域名/系统名，须匹配 ^[a-z][a-z0-9]*$（业务包名 = Postgres schema）。
	Name string
	// Module 是生成仓库的 module path，空则默认 github.com/forgeplex/<name>。
	Module string
	// Dir 是输出目录，空则默认 ./<name>。目录已存在且非空时报错。
	Dir string
	// AppkitVersion 是写进 go.mod 的 appkit 版本（取 cli.Version()）；
	// "(devel)" 表示源码构建：go.mod 不 require appkit（对不存在版本的
	// require 会让 go 命令去代理拉取而失败），改由 appkit dev 的 go.work 提供。
	AppkitVersion string
	// Partitioned 生成「分区域域」形态（仅 domain）：一套代码、N 份数据分区，
	// 迁移与查询全部无 schema 前缀，落位由组合根注入的分区映射（租户 → schema）
	// 经事务级 search_path 路由确定。分区映射的定义放组合根自己的配置文件。
	Partitioned bool
	// Tenant 生成「租户域」形态（仅 domain）：单 schema、行级隔离——业务表
	// 带 tenant_id 列并挂 RLS 三件套（pgtx.TenantPolicySQL），每次事务把
	// 租户身份落成事务级 GUC（pgtx.NewTenant）。与 Partitioned 互斥：
	// schema 隔离与行级隔离不组合。
	Tenant bool
}

// nameRe 与 DESIGN 的域名约束一致：业务包名 = Postgres schema。
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// reservedNames 是不能用作域名/系统名的名字：与骨架内固定包名或 Go 工具链冲突。
var reservedNames = map[string]bool{
	"main": true, "internal": true, "cmd": true, "db": true, "vendor": true,
	"postgres": true, "http": true, "inbox": true, "module": true,
	"sqlc": true, "appkit": true, "go": true, "test": true,
}

// normalize 校验并补全默认值。
func (o *Options) normalize() error {
	if !nameRe.MatchString(o.Name) {
		return fmt.Errorf("名字 %q 不合法：须匹配 %s（业务包名 = Postgres schema）", o.Name, nameRe)
	}
	if reservedNames[o.Name] {
		return fmt.Errorf("名字 %q 是保留字：与骨架内的包名冲突", o.Name)
	}
	if o.Module == "" {
		o.Module = "github.com/forgeplex/" + o.Name
	}
	if err := module.CheckPath(o.Module); err != nil {
		return fmt.Errorf("module path %q 不合法: %w", o.Module, err)
	}
	if o.Dir == "" {
		o.Dir = o.Name
	}
	if o.AppkitVersion == "" {
		o.AppkitVersion = "(devel)"
	}
	if o.Tenant && o.Partitioned {
		return fmt.Errorf("-tenant 与 -partitioned 互斥：行级隔离与 schema 隔离不组合，先选一种隔离形态")
	}
	return nil
}

// tmplData 是全部模板的渲染数据。
type tmplData struct {
	Name          string // ledger
	Upper         string // LEDGER（错误码前缀）
	Module        string // github.com/forgeplex/ledger
	AppkitVersion string // v1.2.3；Devel 时模板不引用
	Devel         bool   // appkit 无发布版本（源码构建）：go.mod 不 require appkit
	Partitioned   bool   // 分区域域：迁移/查询无前缀，落位经 search_path 路由
	Tenant        bool   // 租户域：业务表挂 RLS，租户身份经事务级 GUC 下沉到存储
	EnvPrefix     string // 环境变量前缀（LEDGERD / PSP）
	PgxVersion    string // 生成 go.mod 里 pgx 的版本（跟随 appkit 自身依赖）
	// lint 工具链版本取自 ruleset：Makefile 与 domain-ci.yml 必须同版本。
	GolangciVersion string
	ArchLintVersion string
}

func newData(o Options, envPrefix string) tmplData {
	d := tmplData{
		Name:          o.Name,
		Upper:         strings.ToUpper(o.Name),
		Module:        o.Module,
		AppkitVersion: o.AppkitVersion,
		EnvPrefix:     envPrefix,
		PgxVersion:    pgxVersion(),
		Partitioned:   o.Partitioned,
		Tenant:        o.Tenant,

		GolangciVersion: ruleset.GolangciLintVersion,
		ArchLintVersion: ruleset.ArchLintVersion,
	}
	if o.AppkitVersion == "(devel)" {
		d.Devel = true
		d.AppkitVersion = "v0.0.0"
	}
	return d
}

// pgxVersion 从自身构建信息取 pgx 版本，保证生成 go.mod 与 appkit 依赖一致。
func pgxVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/jackc/pgx/v5" {
				return dep.Version
			}
		}
	}
	return "v5.10.0" // 兜底：与 appkit 当前 require 一致
}

// fileSpec 把一个模板映射到输出路径；路径中的 "NAME" 以域名/系统名替换。
type fileSpec struct {
	tmpl string // templates/<kind>/ 下的文件名
	path string // 仓库内相对路径
}

// renderAll 渲染 kind 目录下的模板集并写盘。
func renderAll(kind string, files []fileSpec, d tmplData, dir string) error {
	tset, err := template.ParseFS(templatesFS, "templates/"+kind+"/*.tmpl")
	if err != nil {
		return fmt.Errorf("解析内置模板 templates/%s: %w", kind, err)
	}
	for _, f := range files {
		var buf bytes.Buffer
		if err := tset.ExecuteTemplate(&buf, f.tmpl, d); err != nil {
			return fmt.Errorf("渲染模板 %s: %w", f.tmpl, err)
		}
		content := buf.Bytes()
		rel := strings.ReplaceAll(f.path, "NAME", d.Name)
		// 生成的 Go 文件统一过 gofmt：import 顺序/对齐不随名字长度漂移，
		// 语法错误在生成期即暴露（指出模板与输出文件）。
		if strings.HasSuffix(rel, ".go") {
			formatted, err := format.Source(content)
			if err != nil {
				return fmt.Errorf("模板 %s 渲染出的 %s 不是合法 Go 源码: %w", f.tmpl, rel, err)
			}
			content = formatted
		}
		if err := writeFile(filepath.Join(dir, rel), content); err != nil {
			return err
		}
	}
	return nil
}

// ensureFreshDir 确保输出目录不存在或为空，避免覆盖既有仓库。
func ensureFreshDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("检查输出目录 %s: %w", dir, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("输出目录 %s 已存在且非空，拒绝覆盖", dir)
	}
	return nil
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建目录 %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("写入 %s: %w", path, err)
	}
	return nil
}

// summarize 打印生成结果与后续步骤。
func summarize(out io.Writer, kind, dir string, next []string) {
	if out == nil {
		out = io.Discard
	}
	fmt.Fprintf(out, "已生成%s骨架：%s\n", kind, dir)
	fmt.Fprintln(out, "下一步：")
	for _, s := range next {
		fmt.Fprintf(out, "  %s\n", s)
	}
}
