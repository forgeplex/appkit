package scaffold

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/forgeplex/appkit/audit"
	"github.com/forgeplex/appkit/idem"
	"github.com/forgeplex/appkit/outbox"
	"github.com/forgeplex/appkit/pgtx"
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
	files := domainFiles
	if o.Partitioned {
		// 分区域域的 module.go 形态差异大（Schemas 注入/路由/每分区 relay），
		// 用专用模板而不是在一个模板里铺满 {{if}}。
		files = swapTemplate(files, "module.go.tmpl", "module_partitioned.go.tmpl")
	}
	if o.Tenant {
		// 同理：租户域的差别在 Transactor（NewTenant）与 Setup 期 RLS 校验。
		files = swapTemplate(files, "module.go.tmpl", "module_tenant.go.tmpl")
	}
	if err := renderAll("domain", files, d, o.Dir); err != nil {
		return fmt.Errorf("new domain %s: %w", o.Name, err)
	}
	// 基础迁移在生成期调用库函数拼接——outbox/idem/audit 的库函数是
	// 这四张基础设施表 DDL 的唯一事实源，模板里不落任何 DDL 副本。
	sqlPath := filepath.Join(o.Dir, "db", "migrations", "0001_appkit_base.sql")
	if err := writeFile(sqlPath, []byte(baseMigrationSQL(o))); err != nil {
		return fmt.Errorf("new domain %s: %w", o.Name, err)
	}
	if o.Tenant {
		// 租户域多给一张样例迁移：RLS 三件套的写法是模式教学，也让
		// Setup 期的 VerifyTenantRLS 从第一天就有东西可验。
		demo := filepath.Join(o.Dir, "db", "migrations", "0002_demo_notes.sql")
		if err := writeFile(demo, []byte(tenantDemoSQL(o))); err != nil {
			return fmt.Errorf("new domain %s: %w", o.Name, err)
		}
	}
	// 生成即合规：lint / CI 配置直接物化，不留"忘了跑 sync"的窗口。
	// 升级 appkit 后由 appkit sync 刷新，CI 的 sync --check 校验未漂移。
	if _, err := ruleset.Sync(o.Dir, o.AppkitVersion); err != nil {
		return fmt.Errorf("new domain %s: 物化规则集: %w", o.Name, err)
	}
	summarize(out, "域仓库", o.Dir, []string{
		"appkit dev    # 生成 go.work 联调兄弟仓库；要吃本地未发布的 appkit 改动，把 appkit 也纳入",
		"make run-minimal # 显式零依赖试跑；make run-db 进完整模式",
		"make check && make test",
	})
	return nil
}

// swapTemplate 把清单里某个输出路径的模板换成变体（路径不变）。
func swapTemplate(files []fileSpec, from, to string) []fileSpec {
	out := make([]fileSpec, len(files))
	copy(out, files)
	for i := range out {
		if out[i].tmpl == from {
			out[i].tmpl = to
		}
	}
	return out
}

// baseMigrationSQL 拼接 outbox/inbox、idempotency_keys、audit_log 的建表语句。
// schema 本身由 pgmigrate 在应用迁移前创建。
func baseMigrationSQL(o Options) string {
	if o.Partitioned {
		return baseMigrationSQLPartitioned()
	}
	var b strings.Builder
	b.WriteString("-- 0001_appkit_base.sql —— appkit 基础设施表（outbox/inbox/幂等/审计），每 schema 一套（DESIGN §8）。\n")
	b.WriteString("-- 本文件由 appkit new 调用库函数生成（库函数是 DDL 唯一事实源）；升级 appkit 后可重新生成刷新。\n\n")
	// pgmigrate 运行期本来会建 schema；这里再写一份是给 sqlc 的静态分析看的
	// （sqlc 只读迁移文件，看不到运行期行为），幂等重复无害。
	fmt.Fprintf(&b, "CREATE SCHEMA IF NOT EXISTS %q;\n\n", o.Name)
	b.WriteString(outbox.MigrationSQL(o.Name))
	b.WriteString("\n")
	b.WriteString(idem.MigrationSQL(o.Name))
	b.WriteString("\n")
	b.WriteString(audit.MigrationSQL(o.Name))
	if o.Tenant {
		// 租户域的策略函数：业务表的 RLS 策略引用它读事务级 GUC。
		// 它在基础设施表之后建——基础设施表不挂 RLS，函数先建后用皆可。
		b.WriteString("\n")
		b.WriteString(pgtx.TenantScopeSQL(o.Name))
	}
	return b.String()
}

// tenantDemoSQL 是租户域的样例业务表迁移：tenant_id 列 + 租户打头索引 +
// RLS 三件套（pgtx.TenantPolicySQL）。整张表可删，写法要照抄——这是
// 「每个域的租户实现长得一样」的落点。
func tenantDemoSQL(o Options) string {
	var b strings.Builder
	b.WriteString("-- 0002_demo_notes.sql —— 租户业务表的样例（可删；建真表时照抄这里的形态）。\n")
	b.WriteString("-- 三件必做的事：\n")
	b.WriteString("-- 1. tenant_id 列 NOT NULL——租户值来自 callctx.From(ctx).TenantID（authn 从\n")
	b.WriteString("--    令牌 tid 焊入，业务代码不读头）；\n")
	b.WriteString("-- 2. 以 tenant_id 打头的索引——RLS 过滤也走索引，全表扫描的隔离不是隔离；\n")
	b.WriteString("-- 3. RLS 三件套（ENABLE + FORCE + 策略）——行级隔离在存储层强制：漏写 WHERE\n")
	b.WriteString("--    的查询只会查不到别的租户的行，跨租户写入直接被拒，都不再是静默泄漏。\n")
	b.WriteString("--    启动期 pgtx.VerifyTenantRLS 会校验：有 tenant_id 列却没挂三件套的表\n")
	b.WriteString("--    会让服务拒绝启动（见 internal/module/module.go 的 Setup）。\n\n")
	fmt.Fprintf(&b, "CREATE TABLE %q.notes (\n", o.Name)
	b.WriteString("    id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,\n")
	b.WriteString("    tenant_id text NOT NULL,\n")
	b.WriteString("    body      text NOT NULL,\n")
	b.WriteString("    created_at timestamptz NOT NULL DEFAULT now()\n")
	b.WriteString(");\n")
	fmt.Fprintf(&b, "CREATE INDEX notes_tenant_idx ON %q.notes (tenant_id, created_at);\n", o.Name)
	fmt.Fprintf(&b, "COMMENT ON TABLE %q.notes IS '样例租户表——删除我之前，先照抄我的形态';\n\n", o.Name)
	b.WriteString(pgtx.TenantPolicySQL(o.Name, "notes"))
	return b.String()
}

// baseMigrationSQLPartitioned 是分区域域的基础迁移：全文件无 schema 前缀，
// 落位由 pgmigrate 按分区经 SET LOCAL search_path 决定。不能写
// CREATE SCHEMA——每个分区的 schema 名不同（组合根注入的映射决定），schema
// 由 pgmigrate 应用时创建；不写前缀也让 sqlc 的静态分析与无前缀查询自洽。
func baseMigrationSQLPartitioned() string {
	var b strings.Builder
	b.WriteString("-- 0001_appkit_base.sql —— appkit 基础设施表（outbox/inbox/幂等/审计），每分区一套（DESIGN §8）。\n")
	b.WriteString("-- 分区域域：本文件全无前缀，落位由 pgmigrate 按分区经 search_path 决定；\n")
	b.WriteString("-- 新分区 = 组合根的分区映射加一条 + 重启，迁移自动建 schema——本文件勿手写任何 schema 名或建 schema 语句。\n")
	b.WriteString("-- 本文件由 appkit new 调用库函数生成（库函数是 DDL 唯一事实源）；升级 appkit 后可重新生成刷新。\n\n")
	b.WriteString(outbox.MigrationSQLBare())
	b.WriteString("\n")
	b.WriteString(idem.MigrationSQLBare())
	b.WriteString("\n")
	b.WriteString(audit.MigrationSQLBare())
	return b.String()
}
