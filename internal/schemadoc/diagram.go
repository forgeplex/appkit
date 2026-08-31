package schemadoc

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// 选 Mermaid 而不是 Graphviz/PNG：GitHub 原生渲染，且在 git diff 里读得懂——
// 一张二进制图片改了什么，review 时谁也看不出来。
//
// 图只画拓扑，不画列：列已经在同一份文件的表格里了，塞进图只会让上百张表的
// 那张图彻底没法看。

// mermaidIdent 是 Mermaid 实体名可以安全直出的字符集。超出的名字宁可报错，
// 也不猜一个转义写法——画错的关系图比没有关系图更误导人。
var mermaidIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// fkEdge 是关系图里的一条边：child 持有外键指向 parent。
type fkEdge struct {
	Child    string
	Parent   string
	Label    string // 外键列，逗号分隔
	Optional bool   // 外键列可空 → 父侧记零或一
	Unique   bool   // 外键列组唯一 → 子侧记至多一
}

// edgesOf 抽出全部外键边，按 (Child, Parent, Label) 升序，保证输出稳定。
func edgesOf(s Schema) []fkEdge {
	var out []fkEdge
	for _, t := range s.Tables {
		nullable := map[string]bool{}
		for _, c := range t.Columns {
			nullable[c.Name] = !c.NotNull
		}
		for _, c := range t.Constraints {
			if c.Type != ConstraintForeignKey || c.RefTable == "" {
				continue
			}
			e := fkEdge{
				Child:  t.Name,
				Parent: c.RefTable,
				Label:  strings.Join(c.Columns, ", "),
				Unique: coveredByUnique(t, c.Columns),
			}
			for _, col := range c.Columns {
				if nullable[col] {
					e.Optional = true
				}
			}
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Child != out[j].Child {
			return out[i].Child < out[j].Child
		}
		if out[i].Parent != out[j].Parent {
			return out[i].Parent < out[j].Parent
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// coveredByUnique 报告 cols 是否恰好是某个主键/唯一约束的列组——是的话这条外键
// 是一对一，图上不该画成一对多。
func coveredByUnique(t Table, cols []string) bool {
	for _, c := range t.Constraints {
		if c.Type != ConstraintPrimaryKey && c.Type != ConstraintUnique {
			continue
		}
		if sameSet(c.Columns, cols) {
			return true
		}
	}
	return false
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// cardinality 返回 Mermaid 的关系记号（父侧 -- 子侧）。
func (e fkEdge) cardinality() string {
	parent := "||" // 恰好一
	if e.Optional {
		parent = "|o" // 零或一：外键列可空
	}
	child := "o{" // 零或多
	if e.Unique {
		child = "o|" // 零或一：外键列组唯一
	}
	return parent + "--" + child
}

// components 把表按外键连通性分簇（并查集），返回每簇内按表名升序、
// 簇之间按「大簇在前、同大小按首表名」排序的结果。
//
// 上百张表的域大概率就是一个大连通分量，全局图仍然会挤——所以真正让人看得动的
// 是每张表自己的一跳邻域图（neighborhood）。全局分簇负责回答「有哪些独立子系统」。
func components(s Schema) [][]string {
	uf := newUnionFind()
	for _, t := range s.Tables {
		uf.add(t.Name)
	}
	for _, e := range edgesOf(s) {
		uf.union(e.Child, e.Parent)
	}
	groups := map[string][]string{}
	for _, t := range s.Tables {
		root := uf.find(t.Name)
		groups[root] = append(groups[root], t.Name)
	}
	out := make([][]string, 0, len(groups))
	for _, g := range groups {
		sort.Strings(g)
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i][0] < out[j][0]
	})
	return out
}

type unionFind struct{ parent map[string]string }

func newUnionFind() *unionFind { return &unionFind{parent: map[string]string{}} }

func (u *unionFind) add(x string) {
	if _, ok := u.parent[x]; !ok {
		u.parent[x] = x
	}
}

func (u *unionFind) find(x string) string {
	u.add(x)
	for u.parent[x] != x {
		u.parent[x] = u.parent[u.parent[x]] // 路径压缩
		x = u.parent[x]
	}
	return x
}

func (u *unionFind) union(a, b string) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	// 按名字定根，让分簇结果与输入顺序无关（生成物必须逐字节稳定）。
	if ra < rb {
		u.parent[rb] = ra
	} else {
		u.parent[ra] = rb
	}
}

// mermaid 渲染一张关系图：只画 tables 集合内部的边，孤立实体也会被声明出来
// （否则一张只有一个表的邻域图会是空图）。
func mermaid(tables []string, edges []fkEdge, indent string) (string, error) {
	in := map[string]bool{}
	for _, t := range tables {
		if !mermaidIdent.MatchString(t) {
			return "", fmt.Errorf("表名 %q 含 Mermaid 实体名不支持的字符，无法生成关系图", t)
		}
		in[t] = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%serDiagram\n", indent)
	linked := map[string]bool{}
	for _, e := range edges {
		if !in[e.Child] || !in[e.Parent] {
			continue
		}
		fmt.Fprintf(&b, "%s    %s %s %s : %q\n", indent, e.Parent, e.cardinality(), e.Child, e.Label)
		linked[e.Child], linked[e.Parent] = true, true
	}
	for _, t := range tables {
		if !linked[t] {
			fmt.Fprintf(&b, "%s    %s\n", indent, t)
		}
	}
	return b.String(), nil
}

// neighborhood 返回 table 的一跳邻域（它本身 + 直接外键邻居）与相关的边。
func neighborhood(table string, edges []fkEdge) ([]string, []fkEdge) {
	set := map[string]bool{table: true}
	var near []fkEdge
	for _, e := range edges {
		switch {
		case e.Child == table:
			set[e.Parent] = true
		case e.Parent == table:
			set[e.Child] = true
		default:
			continue
		}
		near = append(near, e)
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, near
}
