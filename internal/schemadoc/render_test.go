package schemadoc

import (
	"strings"
	"testing"
)

// 本文件与 diagram_test.go / drift_test.go 全部零 DB：introspect 返回纯粹的
// Schema 值，渲染/分簇/漂移都是它的纯函数。改一行 Markdown 不该先起 Postgres。

// sampleSchema 是一份手搓的域快照：两张有外键关系的业务表 + 一张框架表，
// 覆盖「缺 COMMENT」「一对一外键」「视图」「枚举」几条分支。
func sampleSchema() Schema {
	return Schema{
		Name: "ledger",
		Tables: []Table{
			{
				Name:    "entries",
				Kind:    KindTable,
				Comment: "", // 故意缺说明
				Columns: []Column{
					{Name: "id", Type: "uuid", NotNull: true},
					{Name: "account_id", Type: "uuid", NotNull: true},
					{Name: "amount", Type: "numeric(19,4)", NotNull: true},
					{Name: "note", Type: "text"},
				},
				Constraints: []Constraint{
					{Name: "entries_pkey", Type: ConstraintPrimaryKey, Def: "PRIMARY KEY (id)", Columns: []string{"id"}},
					{
						Name: "entries_account_id_fkey", Type: ConstraintForeignKey,
						Def:     "FOREIGN KEY (account_id) REFERENCES ledger.accounts(id)",
						Columns: []string{"account_id"}, RefTable: "accounts", RefColumns: []string{"id"},
					},
				},
				Indexes: []Index{{Name: "entries_account_idx", Def: "CREATE INDEX entries_account_idx ON ledger.entries USING btree (account_id)"}},
			},
			{
				Name:    "accounts",
				Kind:    KindTable,
				Comment: "账户主表",
				Columns: []Column{
					{Name: "id", Type: "uuid", NotNull: true},
					{Name: "code", Type: "text", NotNull: true, Comment: "科目号"},
				},
				Constraints: []Constraint{
					{Name: "accounts_pkey", Type: ConstraintPrimaryKey, Def: "PRIMARY KEY (id)", Columns: []string{"id"}},
					{Name: "accounts_code_key", Type: ConstraintUnique, Def: "UNIQUE (code)", Columns: []string{"code"}},
				},
			},
			{
				Name:    "outbox",
				Kind:    KindTable,
				Columns: []Column{{Name: "id", Type: "uuid", NotNull: true}},
			},
		},
		Enums: []Enum{{Name: "entry_state", Values: []string{"draft", "posted"}}},
	}
}

func TestRenderLayout(t *testing.T) {
	files, err := Render(sampleSchema())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := []string{
		"db/SCHEMA.md",
		"db/schema/accounts.sql",
		"db/schema/accounts.md",
		"db/schema/entries.sql",
		"db/schema/entries.md",
		"db/schema/_appkit/outbox.sql",
		"db/schema/_appkit/outbox.md",
		"db/schema/_enums.sql",
	}
	for _, p := range want {
		if _, ok := files[p]; !ok {
			t.Errorf("缺产出 %s", p)
		}
	}
	if len(files) != len(want) {
		t.Errorf("产出文件数 = %d，期望 %d：%v", len(files), len(want), keysOf(files))
	}
	for p, body := range files {
		head := sqlHeader
		if strings.HasSuffix(p, ".md") {
			head = mdHeader
		}
		if !strings.HasPrefix(body, head) {
			t.Errorf("%s 缺 DO NOT EDIT 头", p)
		}
	}
}

func TestRenderStable(t *testing.T) {
	a, err := Render(sampleSchema())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// 打乱输入顺序：产出必须逐字节一致，否则每次生成都是一次假漂移。
	s := sampleSchema()
	s.Tables[0], s.Tables[2] = s.Tables[2], s.Tables[0]
	b, err := Render(s)
	if err != nil {
		t.Fatalf("Render（乱序）: %v", err)
	}
	if len(a) != len(b) {
		t.Fatalf("文件数不同：%d vs %d", len(a), len(b))
	}
	for p, x := range a {
		if y := b[p]; y != x {
			t.Errorf("%s 的内容随输入顺序变化", p)
		}
	}
}

func TestRenderTableSQL(t *testing.T) {
	files, err := Render(sampleSchema())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	sql := files["db/schema/entries.sql"]
	for _, want := range []string{
		"CREATE TABLE ledger.entries (",
		"amount",
		"numeric(19,4)",
		"CONSTRAINT entries_pkey PRIMARY KEY (id)",
		"CONSTRAINT entries_account_id_fkey FOREIGN KEY (account_id) REFERENCES ledger.accounts(id)",
		"CREATE INDEX entries_account_idx",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("entries.sql 缺 %q\n---\n%s", want, sql)
		}
	}
	// 列注释走 COMMENT ON COLUMN，不塞进 CREATE TABLE。
	acc := files["db/schema/accounts.sql"]
	if !strings.Contains(acc, "COMMENT ON TABLE ledger.accounts IS '账户主表';") {
		t.Errorf("accounts.sql 缺表注释\n---\n%s", acc)
	}
	if !strings.Contains(acc, "COMMENT ON COLUMN ledger.accounts.code IS '科目号';") {
		t.Errorf("accounts.sql 缺列注释\n---\n%s", acc)
	}
}

func TestRenderMissingCommentMarked(t *testing.T) {
	files, err := Render(sampleSchema())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(files["db/schema/entries.md"], noComment) {
		t.Error("缺 COMMENT 的业务表未标注 ⚠")
	}
	if !strings.Contains(files["db/SCHEMA.md"], noComment) {
		t.Error("总览未标注缺说明的表")
	}
	// 框架表不标：域仓库改不了自己那份已应用的 0001，标一个改不动的警告只是噪声。
	if strings.Contains(files["db/schema/_appkit/outbox.md"], noComment) {
		t.Error("框架表不应标 ⚠ 缺说明")
	}
	if strings.Contains(files["db/schema/accounts.md"], noComment) {
		t.Error("有 COMMENT 的表被误标 ⚠")
	}
}

// TestMissingComments 锁 ::warning 的点名清单与渲染层 ⚠ 标注是同一条谓词——
// 文档里标了 ⚠ 的表，必须正好是 MissingComments 报出来的表。
func TestMissingComments(t *testing.T) {
	got := MissingComments(sampleSchema())
	want := []string{"ledger.entries"}
	if len(got) != len(want) {
		t.Fatalf("MissingComments = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("MissingComments[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRenderTableMD(t *testing.T) {
	files, err := Render(sampleSchema())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	md := files["db/schema/entries.md"]
	for _, want := range []string{
		"# ledger.entries",
		"| `account_id` | `uuid` | FK→`accounts.id` · NOT NULL |",
		"## 索引",
		"## 邻域",
		"```mermaid",
		"[DDL](entries.sql)",
		"[← 总览](../SCHEMA.md)",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("entries.md 缺 %q\n---\n%s", want, md)
		}
	}
	// 被引用：accounts 页要能反查到 entries 指着它。
	acc := files["db/schema/accounts.md"]
	if !strings.Contains(acc, "## 被引用") || !strings.Contains(acc, "entries") {
		t.Errorf("accounts.md 缺反向引用\n---\n%s", acc)
	}
	// 框架表在子目录里，回总览要多退一层。
	if !strings.Contains(files["db/schema/_appkit/outbox.md"], "[← 总览](../../SCHEMA.md)") {
		t.Error("框架表页回总览的相对路径不对")
	}
}

func TestRenderOverview(t *testing.T) {
	files, err := Render(sampleSchema())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	doc := files["db/SCHEMA.md"]
	for _, want := range []string{
		"# ledger schema",
		"2 张业务表，1 张 appkit 基础设施表",
		"## 表清单",
		"[`accounts`](schema/accounts.md)",
		"## 关系图",
		"```mermaid",
		"## 枚举类型",
		"| `entry_state` | `draft` · `posted` |",
		"## appkit 基础设施表",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("SCHEMA.md 缺 %q\n---\n%s", want, doc)
		}
	}
	// 框架表不进表清单，也不进关系图。
	if strings.Contains(doc, "[`outbox`](") {
		t.Error("框架表不该出现在业务表清单里")
	}
}

func TestRenderEmptyDomain(t *testing.T) {
	files, err := Render(Schema{
		Name:   "fresh",
		Tables: []Table{{Name: "outbox", Kind: KindTable, Columns: []Column{{Name: "id", Type: "uuid", NotNull: true}}}},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	doc := files["db/SCHEMA.md"]
	if !strings.Contains(doc, "本域尚无业务表") {
		t.Errorf("新仓库的总览应给出下一步提示\n---\n%s", doc)
	}
	if strings.Contains(doc, "## 关系图") {
		t.Error("没有业务表时不该输出空的关系图小节")
	}
	if _, ok := files["db/schema/_enums.sql"]; ok {
		t.Error("没有枚举时不该产出 _enums.sql")
	}
}

func TestRenderView(t *testing.T) {
	files, err := Render(Schema{
		Name: "ledger",
		Tables: []Table{{
			Name: "balances", Kind: KindView, Comment: "账户余额",
			Columns: []Column{{Name: "account_id", Type: "uuid"}},
			ViewDef: " SELECT account_id FROM ledger.entries;",
		}},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(files["db/schema/balances.sql"], "CREATE VIEW ledger.balances AS") {
		t.Errorf("视图 DDL 不对\n---\n%s", files["db/schema/balances.sql"])
	}
	if !strings.Contains(files["db/schema/balances.md"], "这是一个视图") {
		t.Error("视图页未说明它是视图")
	}
}

// TestRenderRejectsUnrenderableName 锁住「画不出来就报错」：Mermaid 实体名放不下的
// 表名宁可让命令失败，也不能猜一个转义写法——画错的关系图比没有更误导人。
func TestRenderRejectsUnrenderableName(t *testing.T) {
	s := Schema{
		Name: "ledger",
		Tables: []Table{
			{Name: "订单", Kind: KindTable, Columns: []Column{{Name: "id", Type: "uuid", NotNull: true}}},
			{Name: "items", Kind: KindTable,
				Columns: []Column{{Name: "oid", Type: "uuid", NotNull: true}},
				Constraints: []Constraint{{
					Name: "items_oid_fkey", Type: ConstraintForeignKey, Def: "FOREIGN KEY (oid) REFERENCES ledger.订单(id)",
					Columns: []string{"oid"}, RefTable: "订单", RefColumns: []string{"id"},
				}},
			},
		},
	}
	if _, err := Render(s); err == nil {
		t.Fatal("表名画不进 Mermaid 时应报错，而不是静默输出坏图")
	}
}

func TestMDCellEscapes(t *testing.T) {
	if got := mdCell("a|b"); strings.Contains(got, "|") && !strings.Contains(got, `\|`) {
		t.Errorf("表格单元格未转义竖线：%q", got)
	}
	if got := mdCell("a\nb"); strings.Contains(got, "\n") {
		t.Errorf("表格单元格未压掉换行：%q", got)
	}
}

func TestSQLLiteral(t *testing.T) {
	if got, want := sqlLiteral("it's"), "'it''s'"; got != want {
		t.Errorf("sqlLiteral(%q) = %s，期望 %s", "it's", got, want)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
