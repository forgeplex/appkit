package schemadoc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forgeplex/appkit/audit"
	"github.com/forgeplex/appkit/idem"
	"github.com/forgeplex/appkit/outbox"
	"github.com/forgeplex/appkit/pgtx"
)

// ---- 需要 Postgres 的测试（TEST_DATABASE_URL）----
//
// 只有本文件走真实数据库：introspect 是包里唯一碰 DB 的部分。它建一次性临时库、
// 用生产的 pgmigrate.Runner 应用迁移、再读 pg_catalog，所以 DSN 需要建库权限。

var schemaSeq int

// introspect 把 migrations 写进临时仓库目录，跑一遍真实的 Introspect。
func introspect(t *testing.T, migrations string) (Schema, error) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过需要 Postgres 的测试")
	}
	schemaSeq++
	name := "sd_test"
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "db", "migrations", "0001_init.sql"), migrations)
	return Introspect(context.Background(), Options{Dir: dir, DSN: dsn, Schema: name})
}

func TestIntrospect(t *testing.T) {
	s, err := introspect(t, `
CREATE TYPE sd_test.entry_state AS ENUM ('draft', 'posted');

CREATE TABLE sd_test.accounts (
    id   uuid PRIMARY KEY,
    code text NOT NULL UNIQUE
);
COMMENT ON TABLE sd_test.accounts IS '账户主表';
COMMENT ON COLUMN sd_test.accounts.code IS '科目号';

CREATE TABLE sd_test.entries (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES sd_test.accounts(id),
    state      sd_test.entry_state NOT NULL DEFAULT 'draft',
    amount     numeric(19,4) NOT NULL CHECK (amount <> 0),
    note       text
);
CREATE INDEX entries_account_idx ON sd_test.entries (account_id);

CREATE VIEW sd_test.posted AS SELECT id FROM sd_test.entries WHERE state = 'posted';
`)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	if s.Name != "sd_test" {
		t.Errorf("schema 名 = %q", s.Name)
	}

	byName := map[string]Table{}
	for _, tb := range s.Tables {
		byName[tb.Name] = tb
	}
	// schema_migrations 是 pgmigrate 自己的历史表，也会被读到（归到框架表）。
	for _, want := range []string{"accounts", "entries", "posted", "schema_migrations"} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("没读到表 %s，实际：%v", want, s.Tables)
		}
	}

	acc := byName["accounts"]
	if acc.Comment != "账户主表" {
		t.Errorf("accounts 的表注释 = %q", acc.Comment)
	}
	if acc.Kind != KindTable {
		t.Errorf("accounts 的 kind = %q", acc.Kind)
	}
	if got := colOf(t, acc, "code"); got.Comment != "科目号" || !got.NotNull {
		t.Errorf("accounts.code = %+v", got)
	}

	ent := byName["entries"]
	if got := colOf(t, ent, "id"); got.Identity != "GENERATED ALWAYS AS IDENTITY" {
		t.Errorf("entries.id 的 identity 子句 = %q", got.Identity)
	}
	if got := colOf(t, ent, "amount"); got.Type != "numeric(19,4)" {
		t.Errorf("entries.amount 的类型 = %q（精度丢了就等于漏掉一条约束）", got.Type)
	}
	if got := colOf(t, ent, "state"); got.Default == "" {
		t.Errorf("entries.state 的默认值没读到")
	}
	if got := colOf(t, ent, "note"); got.NotNull {
		t.Errorf("entries.note 不该是 NOT NULL")
	}

	var fkFound, checkFound bool
	for _, c := range ent.Constraints {
		switch c.Type {
		case ConstraintForeignKey:
			fkFound = true
			if c.RefTable != "accounts" || strings.Join(c.RefColumns, ",") != "id" {
				t.Errorf("外键指向 = %s(%v)", c.RefTable, c.RefColumns)
			}
			if strings.Join(c.Columns, ",") != "account_id" {
				t.Errorf("外键本表列 = %v", c.Columns)
			}
		case ConstraintCheck:
			checkFound = true
		}
	}
	if !fkFound || !checkFound {
		t.Errorf("entries 的约束没读全：%+v", ent.Constraints)
	}

	// 约束自带的索引不该重复出现在索引清单里（PK/UNIQUE 已经在 CONSTRAINT 子句里）。
	if len(ent.Indexes) != 1 || ent.Indexes[0].Name != "entries_account_idx" {
		t.Errorf("entries 的索引 = %+v，期望只有 entries_account_idx", ent.Indexes)
	}

	if v := byName["posted"]; v.Kind != KindView || v.ViewDef == "" {
		t.Errorf("posted 应是视图且带定义：kind=%q def=%q", v.Kind, v.ViewDef)
	}

	if len(s.Enums) != 1 || s.Enums[0].Name != "entry_state" ||
		strings.Join(s.Enums[0].Values, ",") != "draft,posted" {
		t.Errorf("枚举 = %+v（顺序必须是声明顺序，不是字典序）", s.Enums)
	}
}

// TestIntrospectIsPureFunctionOfMigrations 锁住临时库这条设计：产出只取决于
// db/migrations。若改成直接读 -dsn 指的库，谁手工 ALTER 过一次生成物就跟着歪。
func TestIntrospectIsPureFunctionOfMigrations(t *testing.T) {
	const mig = "CREATE TABLE sd_test.t (id uuid PRIMARY KEY);"
	a, err := introspect(t, mig)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	b, err := introspect(t, mig)
	if err != nil {
		t.Fatalf("Introspect（第二次）: %v", err)
	}
	fa, err := Render(a)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	fb, err := Render(b)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for p, x := range fa {
		if fb[p] != x {
			t.Errorf("%s 两次生成不一致——drift check 会天天报假警", p)
		}
	}
}

// TestIntrospectRejectsUnrenderable 锁住「渲染不了就报错」：一份残缺但可信的
// schema 视图，比没有视图危险得多。
func TestIntrospectRejectsUnrenderable(t *testing.T) {
	cases := []struct {
		name, sql, want string
	}{
		{
			name: "分区表",
			sql: `CREATE TABLE sd_test.ev (id bigint, at timestamptz NOT NULL) PARTITION BY RANGE (at);
CREATE TABLE sd_test.ev_2026 PARTITION OF sd_test.ev FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');`,
			want: "分区",
		},
		{
			name: "生成列",
			sql:  `CREATE TABLE sd_test.t (a int, b int GENERATED ALWAYS AS (a * 2) STORED);`,
			want: "生成列",
		},
		{
			name: "排他约束",
			sql: `CREATE EXTENSION IF NOT EXISTS btree_gist;
CREATE TABLE sd_test.t (id uuid PRIMARY KEY, span tstzrange, EXCLUDE USING gist (span WITH &&));`,
			want: "排他约束",
		},
		{
			name: "domain 类型",
			sql:  `CREATE DOMAIN sd_test.positive AS int CHECK (VALUE > 0);`,
			want: "domain 类型",
		},
		{
			name: "独立序列",
			sql:  `CREATE SEQUENCE sd_test.loose;`,
			want: "独立序列",
		},
		{
			name: "表继承",
			sql: `CREATE TABLE sd_test.base (id uuid PRIMARY KEY);
CREATE TABLE sd_test.child () INHERITS (sd_test.base);`,
			want: "继承",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := introspect(t, c.sql)
			if err == nil {
				t.Fatal("应报错而不是输出漏掉这个特性的 DDL")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误没点名特性 %q：%v", c.want, err)
			}
		})
	}
}

// TestRLSRendering 锁住 RLS 的如实渲染：租户域靠它做行级隔离，ENABLE/FORCE
// 与两条策略（隔离 + 只读的读全部）必须出现在 db/schema/<表>.sql 里——
// 策略被删、FORCE 被摘，漂移检查要能看出来。装饰态（挂了策略但没 ENABLE）
// 点名而非装作没有。
// 回放（TestRoundTrip）不含 RLS：策略表达式引用的函数不进 DDL 渲染
// （函数体事实源在迁移），这里单独验渲染文本。
func TestRLSRendering(t *testing.T) {
	mig := pgtx.TenantScopeSQL("sd_test") + `
CREATE TABLE sd_test.documents (id text PRIMARY KEY, tenant_id text NOT NULL);
` + pgtx.TenantPolicySQL("sd_test", "documents")
	s, err := introspect(t, mig)
	if err != nil {
		t.Fatalf("Introspect（RLS 已支持）: %v", err)
	}
	var doc Table
	for _, tb := range s.Tables {
		if tb.Name == "documents" {
			doc = tb
		}
	}
	if doc.RLS == nil || !doc.RLS.Enabled || !doc.RLS.Force || len(doc.RLS.Policies) != 2 {
		t.Fatalf("RLS 读取不符: %+v", doc.RLS)
	}
	files, err := Render(s)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	sql := files[tablePath(doc, ".sql")]
	for _, want := range []string{
		"ALTER TABLE sd_test.documents ENABLE ROW LEVEL SECURITY;",
		"ALTER TABLE sd_test.documents FORCE ROW LEVEL SECURITY;",
		"CREATE POLICY tenant_isolation ON sd_test.documents",
		// 表达式是 pg_policies 的原文（含 Postgres 自己加的外层括号）。
		"USING ((tenant_id = sd_test.appkit_current_tenant()))",
		"WITH CHECK ((tenant_id = sd_test.appkit_current_tenant()))",
		// 读全部策略只限 SELECT：FOR 子句必须渲染出来，否则回放会放开写。
		"CREATE POLICY tenant_isolation_read_all ON sd_test.documents FOR SELECT",
		"USING (sd_test.appkit_read_all_tenants())",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("渲染缺 %q：\n%s", want, sql)
		}
	}
	if strings.Contains(sql, " TO ") {
		t.Errorf("默认角色（public）不该渲染 TO 子句：\n%s", sql)
	}
}

// TestRoundTrip 是本包最值钱的一条：把生成的 DDL 当成迁移再跑一遍，重新 introspect，
// 两次的产出必须逐字节相同。
//
// 它一次性证明了两件事——生成的 .sql 是**合法**的 SQL，而且是**完整**的：漏掉任何
// 一个约束、索引、默认值、注释，第二次的产出就会与第一次不同。少了它，「渲染不了
// 就报错」只挡得住我想到的特性，挡不住我以为渲染对了、其实悄悄丢了一条约束。
func TestRoundTrip(t *testing.T) {
	const mig = `
CREATE TYPE sd_test.entry_state AS ENUM ('draft', 'posted');

CREATE TABLE sd_test.accounts (
    id   uuid PRIMARY KEY,
    code text NOT NULL UNIQUE
);
COMMENT ON TABLE sd_test.accounts IS '账户主表';
COMMENT ON COLUMN sd_test.accounts.code IS '科目号';

CREATE TABLE sd_test.entries (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES sd_test.accounts(id),
    state      sd_test.entry_state NOT NULL DEFAULT 'draft',
    amount     numeric(19,4) NOT NULL CHECK (amount <> 0),
    memo       text
);
CREATE INDEX entries_account_idx ON sd_test.entries (account_id);

CREATE TABLE sd_test.entry_tags (
    entry_id bigint NOT NULL REFERENCES sd_test.entries(id),
    tag      text   NOT NULL,
    PRIMARY KEY (entry_id, tag)
);

CREATE VIEW sd_test.posted AS SELECT id, amount FROM sd_test.entries WHERE state = 'posted';
`
	first, err := introspect(t, mig)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	files, err := Render(first)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	second, err := introspect(t, replayable(t, first, files))
	if err != nil {
		t.Fatalf("回放生成的 DDL: %v", err)
	}
	again, err := Render(second)
	if err != nil {
		t.Fatalf("Render（回放后）: %v", err)
	}
	for p, want := range files {
		if got := again[p]; got != want {
			t.Errorf("%s 回放后不一致——渲染丢了信息：\n--- 期望 ---\n%s\n--- 实际 ---\n%s", p, want, got)
		}
	}
}

// replayable 把生成的 DDL 按依赖顺序拼成一个迁移：枚举 → 表（被引用的在前）→ 视图。
//
// 生成物本身不承诺顺序（它是给人读的，不是拿来执行的），排序是这个测试的事。
// schema_migrations 排除在外：pgmigrate 会先建好它，再建一次必然冲突。
func replayable(t *testing.T, s Schema, files map[string]string) string {
	t.Helper()
	var b strings.Builder
	if enums, ok := files[filepath.ToSlash(filepath.Join(schemaDir, enumsFile))]; ok {
		b.WriteString(enums)
	}

	parents := map[string][]string{}
	byName := map[string]Table{}
	for _, tb := range s.Tables {
		byName[tb.Name] = tb
		for _, c := range tb.Constraints {
			if c.Type == ConstraintForeignKey && c.RefTable != tb.Name {
				parents[tb.Name] = append(parents[tb.Name], c.RefTable)
			}
		}
	}

	done := map[string]bool{"schema_migrations": true}
	emit := func(tb Table) {
		done[tb.Name] = true
		b.WriteString("\n")
		b.WriteString(files[tablePath(tb, ".sql")])
	}
	for progress := true; progress; {
		progress = false
		for _, tb := range s.Tables { // s.Tables 已按名字排序，回放顺序因此确定
			if done[tb.Name] || tb.Kind != KindTable {
				continue
			}
			ready := true
			for _, p := range parents[tb.Name] {
				if !done[p] {
					ready = false
				}
			}
			if ready {
				emit(tb)
				progress = true
			}
		}
	}
	for _, tb := range s.Tables {
		if done[tb.Name] {
			continue
		}
		if tb.Kind == KindTable {
			t.Fatalf("外键成环，测试数据排不出回放顺序：%s", tb.Name)
		}
		emit(tb) // 视图最后
	}
	return b.String()
}

func TestIntrospectMissingMigrations(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过需要 Postgres 的测试")
	}
	_, err := Introspect(context.Background(), Options{
		Dir: t.TempDir(), DSN: os.Getenv("TEST_DATABASE_URL"), Schema: "sd_test",
	})
	if err == nil || !strings.Contains(err.Error(), migrationsDir) {
		t.Errorf("缺迁移目录时应点名 %s：%v", migrationsDir, err)
	}
}

// TestFrameworkTablesAllDocumented 是「框架自己也守这条规矩」的机检：
// appkit 建的每张基础设施表都必须有 COMMENT ON TABLE。
//
// 它不是字符串匹配，而是把三个库函数的 DDL 真跑一遍再从 pg_catalog 读回来——
// 于是新增一张框架表却忘了写说明，这里就会红；schema_migrations 由 pgmigrate
// 在应用迁移前自己建，一并覆盖到。
func TestFrameworkTablesAllDocumented(t *testing.T) {
	s, err := introspect(t, strings.Join([]string{
		outbox.MigrationSQL("sd_test"),
		idem.MigrationSQL("sd_test"),
		audit.MigrationSQL("sd_test"),
	}, "\n"))
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	var seen int
	for _, tb := range s.Tables {
		if !tb.Framework() {
			t.Errorf("%s 不在 frameworkTables 里，但它由框架 DDL 建出", tb.Name)
			continue
		}
		seen++
		if tb.Comment == "" {
			t.Errorf("框架表 %s 缺 COMMENT ON TABLE——框架自己得先守这条规矩", tb.Name)
		}
	}
	if want := len(frameworkTables); seen != want {
		t.Errorf("建出 %d 张框架表，frameworkTables 登记了 %d 张：两边得对上", seen, want)
	}
}

func colOf(t *testing.T, tb Table, name string) Column {
	t.Helper()
	for _, c := range tb.Columns {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("%s 没有列 %s", tb.Name, name)
	return Column{}
}
