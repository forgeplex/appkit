package schemadoc

import (
	"strings"
	"testing"
)

// tbl 是构造测试用表的简写：cols 里带 "!" 后缀表示 NOT NULL。
func tbl(name string, cols ...string) Table {
	t := Table{Name: name, Kind: KindTable}
	for _, c := range cols {
		col := Column{Name: strings.TrimSuffix(c, "!"), Type: "uuid"}
		col.NotNull = strings.HasSuffix(c, "!")
		t.Columns = append(t.Columns, col)
	}
	return t
}

func pk(t Table, cols ...string) Table {
	t.Constraints = append(t.Constraints, Constraint{
		Name: t.Name + "_pkey", Type: ConstraintPrimaryKey,
		Def: "PRIMARY KEY (" + strings.Join(cols, ", ") + ")", Columns: cols,
	})
	return t
}

func uniq(t Table, cols ...string) Table {
	t.Constraints = append(t.Constraints, Constraint{
		Name: t.Name + "_key", Type: ConstraintUnique,
		Def: "UNIQUE (" + strings.Join(cols, ", ") + ")", Columns: cols,
	})
	return t
}

func fk(t Table, parent string, cols ...string) Table {
	t.Constraints = append(t.Constraints, Constraint{
		Name: t.Name + "_" + strings.Join(cols, "_") + "_fkey", Type: ConstraintForeignKey,
		Def:     "FOREIGN KEY (" + strings.Join(cols, ", ") + ") REFERENCES " + parent + "(id)",
		Columns: cols, RefTable: parent, RefColumns: []string{"id"},
	})
	return t
}

func TestCardinality(t *testing.T) {
	cases := []struct {
		name  string
		table Table
		want  string
	}{
		{
			// 外键非空 + 不唯一 → 恰好一对多
			name:  "非空多对一",
			table: fk(pk(tbl("entries", "id!", "account_id!"), "id"), "accounts", "account_id"),
			want:  "||--o{",
		},
		{
			// 外键可空 → 父侧零或一
			name:  "可空多对一",
			table: fk(pk(tbl("entries", "id!", "account_id"), "id"), "accounts", "account_id"),
			want:  "|o--o{",
		},
		{
			// 外键列组恰好唯一 → 子侧至多一
			name:  "一对一",
			table: fk(uniq(pk(tbl("profiles", "id!", "account_id!"), "id"), "account_id"), "accounts", "account_id"),
			want:  "||--o|",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			edges := edgesOf(Schema{Tables: []Table{c.table}})
			if len(edges) != 1 {
				t.Fatalf("边数 = %d，期望 1", len(edges))
			}
			if got := edges[0].cardinality(); got != c.want {
				t.Errorf("cardinality = %q，期望 %q", got, c.want)
			}
		})
	}
}

func TestComponents(t *testing.T) {
	s := Schema{Tables: []Table{
		pk(tbl("accounts", "id!"), "id"),
		fk(pk(tbl("entries", "id!", "account_id!"), "id"), "accounts", "account_id"),
		fk(pk(tbl("splits", "id!", "entry_id!"), "id"), "entries", "entry_id"),
		pk(tbl("settings", "id!"), "id"), // 孤岛
		pk(tbl("rates", "id!"), "id"),    // 孤岛
	}}
	comps := components(s)
	if len(comps) != 3 {
		t.Fatalf("连通分量数 = %d，期望 3：%v", len(comps), comps)
	}
	// 大簇在前；同大小按首表名。
	want := [][]string{{"accounts", "entries", "splits"}, {"rates"}, {"settings"}}
	for i := range want {
		if strings.Join(comps[i], ",") != strings.Join(want[i], ",") {
			t.Errorf("第 %d 簇 = %v，期望 %v", i+1, comps[i], want[i])
		}
	}
}

// TestComponentsOrderIndependent 锁住分簇与输入顺序无关：并查集按名字定根，
// 否则同一份迁移在不同机器上会生成不同的 SCHEMA.md，drift check 天天报假警。
func TestComponentsOrderIndependent(t *testing.T) {
	a := Schema{Tables: []Table{
		pk(tbl("accounts", "id!"), "id"),
		fk(pk(tbl("entries", "id!", "account_id!"), "id"), "accounts", "account_id"),
		fk(pk(tbl("splits", "id!", "entry_id!"), "id"), "entries", "entry_id"),
	}}
	b := Schema{Tables: []Table{a.Tables[2], a.Tables[0], a.Tables[1]}}
	if got, want := components(b), components(a); strings.Join(got[0], ",") != strings.Join(want[0], ",") {
		t.Errorf("分簇随输入顺序变化：%v vs %v", got, want)
	}
}

func TestNeighborhood(t *testing.T) {
	s := Schema{Tables: []Table{
		pk(tbl("accounts", "id!"), "id"),
		fk(pk(tbl("entries", "id!", "account_id!"), "id"), "accounts", "account_id"),
		fk(pk(tbl("splits", "id!", "entry_id!"), "id"), "entries", "entry_id"),
		pk(tbl("far", "id!"), "id"),
	}}
	edges := edgesOf(s)
	names, near := neighborhood("entries", edges)
	// 一跳：entries 自己 + 它指向的 accounts + 指向它的 splits。far 在两跳外，不进。
	if got, want := strings.Join(names, ","), "accounts,entries,splits"; got != want {
		t.Errorf("邻域 = %s，期望 %s", got, want)
	}
	if len(near) != 2 {
		t.Errorf("邻域边数 = %d，期望 2", len(near))
	}
}

func TestMermaidIsolatedEntityDeclared(t *testing.T) {
	// 一张没有任何外键的表，邻域图也必须画出它自己——否则是一张空图。
	d, err := mermaid([]string{"settings"}, nil, "")
	if err != nil {
		t.Fatalf("mermaid: %v", err)
	}
	if !strings.Contains(d, "erDiagram") || !strings.Contains(d, "settings") {
		t.Errorf("孤立表的图不完整：\n%s", d)
	}
}

func TestMermaidRejectsBadIdent(t *testing.T) {
	if _, err := mermaid([]string{"订单"}, nil, ""); err == nil {
		t.Fatal("Mermaid 实体名放不下的表名应报错")
	}
}

func TestSelfReference(t *testing.T) {
	// 自引用（parent_id → 自己）：不能把自己算成邻居，也不能进「被引用」。
	s := Schema{Tables: []Table{fk(pk(tbl("nodes", "id!", "parent_id"), "id"), "nodes", "parent_id")}}
	edges := edgesOf(s)
	if len(referrers("nodes", edges)) != 0 {
		t.Error("自引用不该出现在「被引用」里")
	}
	names, _ := neighborhood("nodes", edges)
	if len(names) != 1 || names[0] != "nodes" {
		t.Errorf("自引用的邻域 = %v，期望只有自己", names)
	}
	d, err := mermaid(names, edges, "")
	if err != nil {
		t.Fatalf("mermaid: %v", err)
	}
	if !strings.Contains(d, "nodes ") {
		t.Errorf("自引用的图没画出来：\n%s", d)
	}
}
