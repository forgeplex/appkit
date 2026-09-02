// Package schemadoc 实现 appkit schema：把 db/migrations 派生成一份可 review、
// 可 grep、GitHub 直接渲染的 schema 视图（db/SCHEMA.md + db/schema/）。
//
// 为什么需要它：迁移是不可变的追加日志（pgmigrate 存 sha256，改动已应用文件即
// MIGRATION_DRIFT 拒启动），一张表的真实形状因此散落在 N 个文件里——0003 建表、
// 0007 加列、0012 改默认值。想看「这个域的 schema 长什么样」，只能在脑子里重放
// 整条迁移历史。本包把那次重放交给 Postgres 做一遍，再把结果写成文件。
//
// 事实源仍然是 db/migrations：本包的产出全是生成物，带 DO NOT EDIT 头，
// 由 appkit schema -check 守住不漂移。
//
// 包内分工（这个接缝是刻意的）：
//   - introspect.go 是唯一需要数据库的文件，产出纯粹的 Schema 值；
//   - render.go / diagram.go 是 Schema 的纯函数，因此渲染、分簇、漂移比对
//     全部可以用零 DB 的单测覆盖。
package schemadoc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// 产出布局。docFile 是人的入口，schemaDir 下每张表两个文件：
// .sql 给 agent 与 grep（写迁移时直接读得懂），.md 给人 review（GitHub 渲染）。
const (
	docFile   = "db/SCHEMA.md"
	schemaDir = "db/schema"
	fwSubdir  = "_appkit" // 框架表隔离子目录
	enumsFile = "_enums.sql"

	// migrationsDir 是迁移源目录，也就是本包产出的唯一事实源。
	migrationsDir = "db/migrations"
)

// frameworkTables 是 appkit 自己建的基础设施表：每个域仓库都长得一模一样。
// 隔进 _appkit/ 子目录、总览里只列名字、不进 ER 图——否则新建仓库的 schema
// 视图会是 100% 框架表，业务表反而被淹没。
var frameworkTables = map[string]bool{
	"outbox":            true,
	"inbox":             true,
	"idempotency_keys":  true,
	"audit_log":         true,
	"schema_migrations": true, // pgmigrate 自己的历史表
}

// ErrNotAdopted 表示仓库尚未启用 schema 文档：db/SCHEMA.md 与 db/schema/ 都不存在。
//
// 这是一个「启用门」，不是疏漏。domain-ci.yml 经 @main 被全部存量域仓库共享，
// 新增一个硬检查会让它们在合并那一刻集体变红；而 appkit new 必须离线可跑，
// 生成不出需要数据库的产物，新仓库首推的 CI 也不该是红的。
//
// 代价是诚实的：从不启用的仓库永远不被检查（docs/DESIGN.md §7 记 ▲ 不记 ★）。
// 自愈性在于——跑过一次 make schema 之后就永久转严，而删目录逃检查是 diff
// 里看得见的删除。
var ErrNotAdopted = errors.New("本仓库尚未启用 schema 文档")

// Options 是一次生成/检查的输入。
type Options struct {
	Dir    string // 仓库根目录
	DSN    string // 任一 Postgres 连接串；迁移会应用到由它派生的一次性临时库
	Schema string // 域 schema 名（.appkit.yml 的 domain）
}

// Schema 是一个域 schema 的完整结构快照，也是全部渲染的唯一输入。
type Schema struct {
	Name      string
	Tables    []Table    // 按 Name 升序
	Enums     []Enum     // 按 Name 升序
	Functions []Function // 按 Name 升序
}

// TableKind 区分普通表与（物化）视图。本包只渲染这三种，其余一律报错。
type TableKind string

const (
	KindTable    TableKind = "table"
	KindView     TableKind = "view"
	KindMatView  TableKind = "matview"
	kindRelTable           = "r"
	kindRelView            = "v"
	kindRelMat             = "m"
)

// Table 是一张表/视图。Comment 为空即「缺说明」，渲染时标注 ⚠——
// 表的用途属于 schema 设计，写在迁移的 COMMENT ON TABLE 里才会跟着表一起演进。
type Table struct {
	Name        string
	Kind        TableKind
	Comment     string
	Columns     []Column
	Constraints []Constraint
	Indexes     []Index
	Triggers    []Trigger
	ViewDef     string // 仅 view/matview
	RLS         *RLS   // 开了 RLS 或挂了策略时非 nil（见 RLS）
}

// RLS 是一张表的行级安全状态。租户域的业务表经 pgtx.TenantPolicySQL 挂上
// ENABLE/FORCE/策略三件套；这里如实渲染进 db/schema/<表>.sql——策略被删、
// 被改名、FORCE 被摘，schema 文档与 CI 漂移检查都会跟着变红。
type RLS struct {
	Enabled  bool
	Force    bool
	Policies []RLSPolicy
}

// RLSPolicy 对应 pg_policies 的一行，表达式是 Postgres 展开后的文本。
type RLSPolicy struct {
	Name      string
	Cmd       string // ALL / SELECT / INSERT / UPDATE / DELETE
	Roles     string // 数组字面量，如 {PUBLIC}
	Using     string
	WithCheck string
}

// Framework 报告本表是否为 appkit 基础设施表。
func (t Table) Framework() bool { return frameworkTables[t.Name] }

// Column 是一列。Default 为空表示无默认值；
// Identity 非空时是 "GENERATED ALWAYS AS IDENTITY" 这样的原文子句。
type Column struct {
	Name     string
	Type     string // format_type 的输出，如 numeric(19,4)
	NotNull  bool
	Default  string
	Identity string
	Comment  string
}

// ConstraintType 对应 pg_constraint.contype 中本包支持的四种。
type ConstraintType string

const (
	ConstraintPrimaryKey ConstraintType = "p"
	ConstraintUnique     ConstraintType = "u"
	ConstraintForeignKey ConstraintType = "f"
	ConstraintCheck      ConstraintType = "c"
)

// Constraint 是一条表约束。Def 是 pg_get_constraintdef 的原文——难的部分一律
// 交给 Postgres 自己渲染，本包只在需要结构化信息（画 ER 图、标注列）时才拆解。
type Constraint struct {
	Name       string
	Type       ConstraintType
	Def        string
	Columns    []string // p/u/f 的本表列
	RefTable   string   // 仅 f
	RefColumns []string // 仅 f
}

// Index 是一个非约束附带的索引，Def 为 pg_get_indexdef 原文。
type Index struct {
	Name string
	Def  string
}

// Trigger 是一个触发器，Def 为 pg_get_triggerdef 原文。
type Trigger struct {
	Name string
	Def  string
}

// Enum 是一个枚举类型。少了它，schema 视图里会出现一个叫 order_status 的类型
// 却说不出它能取哪些值——那正是本包要消灭的东西。
type Enum struct {
	Name   string
	Values []string
}

// Function 是 schema 里的一个函数/存储过程，只记签名不记函数体。
//
// 记它是为了不静默漏东西：本包的产出叫「schema 视图」，一个 schema 里有函数
// 却只字不提，读者会以为没有。函数体不进产出——那是代码，事实源在迁移文件里。
type Function struct {
	Name      string
	Signature string // 参数列表，如 "id uuid, at timestamptz"
	Returns   string
}

// Generate 重新生成 schema 文档并写入 o.Dir，返回写入的仓库相对路径（有序）。
// db/schema/ 下不再对应任何表的陈旧文件会被删除（迁移删表后必须跟着消失）。
func Generate(ctx context.Context, o Options) ([]string, error) {
	s, err := Introspect(ctx, o)
	if err != nil {
		return nil, err
	}
	files, err := Render(s)
	if err != nil {
		return nil, err
	}
	if err := pruneStale(o.Dir, files); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		abs := filepath.Join(o.Dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return nil, fmt.Errorf("创建目录 %s: %w", filepath.Dir(p), err)
		}
		if err := os.WriteFile(abs, []byte(files[p]), 0o644); err != nil {
			return nil, fmt.Errorf("写入 %s: %w", p, err)
		}
	}
	return paths, nil
}

// Check 比对磁盘上的产出与重新生成的内容，漂移时返回错误。
// 两份产出都不存在（尚未启用）时返回 ErrNotAdopted，由调用方决定如何处理。
//
// 无论是否漂移，都一并返回缺 COMMENT ON TABLE 的业务表清单——那是软约束
// （渲染层标 ⚠，CI 打 ::warning），不该让检查变红，但值得被点名。
func Check(ctx context.Context, o Options) ([]string, error) {
	on, err := Adopted(o.Dir)
	if err != nil {
		return nil, err
	}
	if !on {
		return nil, ErrNotAdopted
	}
	s, err := Introspect(ctx, o)
	if err != nil {
		return nil, err
	}
	want, err := Render(s)
	if err != nil {
		return nil, err
	}
	got, err := readOnDisk(o.Dir)
	if err != nil {
		return nil, err
	}
	return MissingComments(s), diff(want, got)
}

// MissingComments 返回缺 COMMENT ON TABLE 的业务表（schema.table 形式，有序）。
// 谓词必须与 render.go 打 ⚠ 的那条保持一致：文档里标了 ⚠ 的表就是这里点名的表。
// 框架表不算——它的 DDL 归 appkit 库函数维护，域仓库改不动自己那份已应用的 0001。
func MissingComments(s Schema) []string {
	var out []string
	for _, t := range s.Tables {
		if t.Comment == "" && !t.Framework() {
			out = append(out, s.Name+"."+t.Name)
		}
	}
	return out
}

// Adopted 报告仓库是否已启用：任一产出存在即算启用，两者都不在才是未启用。
// 只删其中一半不能让检查静音——那样「删掉就不检查了」会变成一条真实的逃生路。
//
// 它是纯文件系统判断、不连库，所以调用方可以在要求 DSN **之前**先问一次：
// 未启用的仓库不该为了被告知「未启用」而先去准备一个数据库。
func Adopted(dir string) (bool, error) {
	for _, p := range []string{docFile, schemaDir} {
		_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p)))
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, os.ErrNotExist):
		default:
			return false, fmt.Errorf("检查 %s: %w", p, err)
		}
	}
	return false, nil
}

// readOnDisk 读出磁盘上现有的产出（db/SCHEMA.md 与 db/schema/ 下全部文件）。
func readOnDisk(dir string) (map[string]string, error) {
	got := make(map[string]string)
	body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(docFile)))
	switch {
	case err == nil:
		got[docFile] = string(body)
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, fmt.Errorf("读取 %s: %w", docFile, err)
	}

	root := filepath.Join(dir, filepath.FromSlash(schemaDir))
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		got[filepath.ToSlash(rel)] = string(body)
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("扫描 %s: %w", schemaDir, err)
	}
	return got, nil
}

// diff 比对期望与实际，把缺失、多余、漂移三类问题一次报全。
//
// 与 ruleset.Check 的差别在「多余」这一类：那里文件集是固定的，这里是动态的——
// 迁移删掉一张表之后，残留的 db/schema/x.sql 会一直躺在仓库里骗人，必须报出来。
func diff(want, got map[string]string) error {
	var missing, extra, drifted []string
	for p, w := range want {
		g, ok := got[p]
		switch {
		case !ok:
			missing = append(missing, p)
		case g != w:
			drifted = append(drifted, p)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			extra = append(extra, p)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	sort.Strings(drifted)

	var b strings.Builder
	for _, g := range []struct {
		label string
		paths []string
	}{
		{"缺失", missing},
		{"内容漂移（生成物被手改，或迁移变了没重新生成）", drifted},
		{"多余（对应的表已不存在）", extra},
	} {
		if len(g.paths) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n  %s：\n", g.label)
		for _, p := range g.paths {
			fmt.Fprintf(&b, "    %s\n", p)
		}
	}
	if b.Len() == 0 {
		return nil
	}
	return fmt.Errorf("schema 文档与 %s 不一致：%s\n  修复：make schema（或 appkit schema）后提交", migrationsDir, b.String())
}

// pruneStale 删除 db/schema/ 下不在本次产出里的文件。
// 只碰 schemaDir 之内、且确实由本包生成的路径，其余一律不动。
func pruneStale(dir string, want map[string]string) error {
	root := filepath.Join(dir, filepath.FromSlash(schemaDir))
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if _, ok := want[filepath.ToSlash(rel)]; ok {
			return nil
		}
		return os.Remove(p)
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("清理 %s 下的陈旧文件: %w", schemaDir, err)
	}
	return nil
}

// tablePath 返回一张表的产出路径（框架表落进 _appkit/ 子目录）。
func tablePath(t Table, ext string) string {
	if t.Framework() {
		return path.Join(schemaDir, fwSubdir, t.Name+ext)
	}
	return path.Join(schemaDir, t.Name+ext)
}
