package scaffold

import (
	"fmt"
	"io"
	"strings"
)

// systemFiles 是组合仓库骨架的模板清单（DESIGN §5：只做 wiring + 配置 + 部署）。
var systemFiles = []fileSpec{
	{tmpl: "go.mod.tmpl", path: "go.mod"},
	{tmpl: "appkit.yml.tmpl", path: ".appkit.yml"},
	{tmpl: "gitignore.tmpl", path: ".gitignore"},
	{tmpl: "Makefile.tmpl", path: "Makefile"},
	{tmpl: "README.md.tmpl", path: "README.md"},
	{tmpl: "main.go.tmpl", path: "cmd/NAME/main.go"},
	{tmpl: "dev.yaml.tmpl", path: "config/dev.yaml"},
	{tmpl: "prod.yaml.tmpl", path: "config/prod.yaml"},
	{tmpl: "deploy.md.tmpl", path: "deploy/README.md"},
}

// System 生成组合仓库骨架。out 承接进度输出（可为 nil）。
func System(o Options, out io.Writer) error {
	if err := o.normalize(); err != nil {
		return fmt.Errorf("new system: %w", err)
	}
	if err := ensureFreshDir(o.Dir); err != nil {
		return fmt.Errorf("new system: %w", err)
	}
	d := newData(o, strings.ToUpper(o.Name))
	if err := renderAll("system", systemFiles, d, o.Dir); err != nil {
		return fmt.Errorf("new system %s: %w", o.Name, err)
	}
	summarize(out, "组合仓库", o.Dir, []string{
		"go.mod 里 require 各域 repo，在 cmd/" + o.Name + "/main.go 取消对应装配注释",
		"appkit dev    # 生成 go.work 联调本地各仓",
		"make run      # -target=all 单体起服",
	})
	return nil
}
