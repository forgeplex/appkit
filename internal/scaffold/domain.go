package scaffold

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/forgeplex/appkit/audit"
	"github.com/forgeplex/appkit/idem"
	"github.com/forgeplex/appkit/outbox"
	"github.com/forgeplex/appkit/ruleset"
)

// domainFiles 是域仓库骨架的模板清单（DESIGN §4 的 Go 惯用形态）。
var domainFiles = []fileSpec{
	{tmpl: "go.mod.tmpl", path: "go.mod"},
	{tmpl: "appkit.yml.tmpl", path: ".appkit.yml"},
	{tmpl: "gitignore.tmpl", path: ".gitignore"},
	{tmpl: "gitattributes.tmpl", path: ".gitattributes"},
	{tmpl: "Makefile.tmpl", path: "Makefile"},
	{tmpl: "README.md.tmpl", path: "README.md"},
	{tmpl: "AGENTS.md.tmpl", path: "AGENTS.md"},
	{tmpl: "CLAUDE.md.tmpl", path: "CLAUDE.md"},
	{tmpl: "root.go.tmpl", path: "NAME.go"},
	{tmpl: "main.go.tmpl", path: "cmd/NAMEd/main.go"},
	{tmpl: "service.go.tmpl", path: "internal/NAME/service.go"},
	{tmpl: "store.go.tmpl", path: "internal/NAME/store.go"},
	{tmpl: "errors.go.tmpl", path: "internal/NAME/errors.go"},
	{tmpl: "postgres.go.tmpl", path: "internal/postgres/store.go"},
	{tmpl: "handler.go.tmpl", path: "internal/http/handler.go"},
	{tmpl: "consumer.go.tmpl", path: "internal/inbox/consumer.go"},
	{tmpl: "module.go.tmpl", path: "internal/module/module.go"},
	{tmpl: "sqlc.yaml.tmpl", path: "sqlc.yaml"},
	{tmpl: "queries.sql.tmpl", path: "db/queries/example.sql"},
	{tmpl: "dev.yaml.tmpl", path: "config/dev.yaml"},
}

// Domain 生成业务域仓库骨架。out 承接进度输出（可为 nil）。
func Domain(o Options, out io.Writer) error {
	if err := o.normalize(); err != nil {
		return fmt.Errorf("new domain: %w", err)
	}
	if err := ensureFreshDir(o.Dir); err != nil {
		return fmt.Errorf("new domain: %w", err)
	}
	d := newData(o, strings.ToUpper(o.Name)+"D")
	if err := renderAll("domain", domainFiles, d, o.Dir); err != nil {
		return fmt.Errorf("new domain %s: %w", o.Name, err)
	}
	// 基础迁移在生成期调用库函数拼接——outbox/idem/audit 的库函数是
	// 这四张基础设施表 DDL 的唯一事实源，模板里不落任何 DDL 副本。
	sqlPath := filepath.Join(o.Dir, "db", "migrations", "0001_appkit_base.sql")
	if err := writeFile(sqlPath, []byte(baseMigrationSQL(o.Name))); err != nil {
		return fmt.Errorf("new domain %s: %w", o.Name, err)
	}
	// 生成即合规：lint / CI 配置直接物化，不留"忘了跑 sync"的窗口。
	// 升级 appkit 后由 appkit sync 刷新，CI 的 sync --check 校验未漂移。
	if _, err := ruleset.Sync(o.Dir, o.AppkitVersion); err != nil {
		return fmt.Errorf("new domain %s: 物化规则集: %w", o.Name, err)
	}
	summarize(out, "域仓库", o.Dir, []string{
		"appkit dev    # 生成 go.work 联调本地 appkit（appkit 未发版时必需）",
		"make run      # 零依赖试跑（最小模式）；make run-db 进完整模式",
		"make check && make test",
	})
	return nil
}

// baseMigrationSQL 拼接 outbox/inbox、idempotency_keys、audit_log 的建表语句。
// schema 本身由 pgmigrate 在应用迁移前创建。
func baseMigrationSQL(name string) string {
	var b strings.Builder
	b.WriteString("-- 0001_appkit_base.sql —— appkit 基础设施表（outbox/inbox/幂等/审计），每 schema 一套（DESIGN §8）。\n")
	b.WriteString("-- 本文件由 appkit new 调用库函数生成（库函数是 DDL 唯一事实源）；升级 appkit 后可重新生成刷新。\n\n")
	// pgmigrate 运行期本来会建 schema；这里再写一份是给 sqlc 的静态分析看的
	// （sqlc 只读迁移文件，看不到运行期行为），幂等重复无害。
	fmt.Fprintf(&b, "CREATE SCHEMA IF NOT EXISTS %q;\n\n", name)
	b.WriteString(outbox.MigrationSQL(name))
	b.WriteString("\n")
	b.WriteString(idem.MigrationSQL(name))
	b.WriteString("\n")
	b.WriteString(audit.MigrationSQL(name))
	return b.String()
}
