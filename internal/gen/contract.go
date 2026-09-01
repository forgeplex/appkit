package gen

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// contractDoc 是 contract.yaml 的顶层 schema——契约包的事实源：
// 接口、DTO、wrapper、client、openapi 导出全部由它派生（DESIGN §3）。
type contractDoc struct {
	Version int         `yaml:"version"`
	Package string      `yaml:"package"`
	System  string      `yaml:"system"`
	Types   []typeDef   `yaml:"types"`
	Methods []methodDef `yaml:"methods"`
}

// typeDef 是命名 DTO（List 类响应的元素类型，如 Entry）。字段仅允许
// 标量/标量数组——嵌套封顶一层，递归引用与拓扑排序就此不存在。
type typeDef struct {
	Name   string     `yaml:"name"`
	Doc    string     `yaml:"doc"`
	Fields []fieldDef `yaml:"fields"`
	line   int
}

func (t *typeDef) UnmarshalYAML(n *yaml.Node) error {
	type plain typeDef
	if err := n.Decode((*plain)(t)); err != nil {
		return err
	}
	t.line = n.Line
	return nil
}

// methodDef 是一条契约方法。request/response 为字段列表：空 request 表示
// 无请求 DTO，空 response 表示仅 error 返回——与 wrap.go 的 shapeDoc 同构。
type methodDef struct {
	Name       string     `yaml:"name"`
	Path       string     `yaml:"path"`
	Doc        string     `yaml:"doc"`
	Idempotent bool       `yaml:"idempotent"`
	Request    []fieldDef `yaml:"request"`
	Response   []fieldDef `yaml:"response"`
	line       int
}

func (m *methodDef) UnmarshalYAML(n *yaml.Node) error {
	type plain methodDef
	if err := n.Decode((*plain)(m)); err != nil {
		return err
	}
	m.line = n.Line
	return nil
}

// rePath 约束契约路径：绝对路径、段内只允许安全字符。
var rePath = regexp.MustCompile(`^/[a-zA-Z0-9/_-]*[a-zA-Z0-9_-]$`)

// Contract 读取契约 yaml，在 outDir 生成契约包的全套文件：
//
//	service.gen.go  Service 接口 + 传值 DTO（本文件渲染）
//	wrap.gen.go     进程内 wrapper（复用 Wrap 链路，扫刚写出的 service.gen.go）
//	client.gen.go   远程 client：同一接口，contract.Call + 幂等有界重试
//	server.gen.go   HTTP 暴露：与 client 互为镜像的编解码
//
// 方法 HTTP 形态唯一：POST + JSON body，错误一律 problem+json——
// 契约调用是 RPC 语义，client/server 两侧的编解码因此只有一份约定。
func Contract(inPath, outDir string) error {
	if inPath == "" || outDir == "" {
		return fmt.Errorf("gen contract 需要 -in <contract.yaml> 与 -dir <pkgdir>")
	}
	doc, err := parseContractDoc(inPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("创建输出目录: %w", err)
	}
	if err := writeGo(filepath.Join(outDir, "service.gen.go"), renderService(doc)); err != nil {
		return err
	}
	// wrap 复用既有链路：扫刚写出的 service.gen.go，wrap.go 自身零改动。
	if err := Wrap(outDir, "Service", doc.System, filepath.Join(outDir, "wrap.gen.go")); err != nil {
		return err
	}
	if err := writeGo(filepath.Join(outDir, "client.gen.go"), renderClient(doc)); err != nil {
		return err
	}
	if err := writeGo(filepath.Join(outDir, "server.gen.go"), renderServer(doc)); err != nil {
		return err
	}
	return nil
}

func parseContractDoc(inPath string) (*contractDoc, error) {
	data, err := os.ReadFile(inPath)
	if err != nil {
		return nil, fmt.Errorf("读取输入: %w", err)
	}
	var doc contractDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: 解析 yaml: %w", inPath, err)
	}
	if doc.Version != 1 {
		return nil, fmt.Errorf("%s: 不支持的 version %d（当前仅支持 1）", inPath, doc.Version)
	}
	if !rePackage.MatchString(doc.Package) {
		return nil, fmt.Errorf("%s: package %q 非法（须匹配 ^[a-z][a-z0-9]*$）", inPath, doc.Package)
	}
	if !rePackage.MatchString(doc.System) {
		return nil, fmt.Errorf("%s: system %q 非法（须匹配 ^[a-z][a-z0-9]*$，span 命名与错误归属用它）", inPath, doc.System)
	}
	if len(doc.Methods) == 0 {
		return nil, fmt.Errorf("%s: methods 为空，没有可生成的内容", inPath)
	}

	named, err := checkTypes(inPath, &doc)
	if err != nil {
		return nil, err
	}
	if err := checkMethodsDoc(inPath, &doc, named); err != nil {
		return nil, err
	}
	return &doc, nil
}

// checkTypes 校验 types 段并返回命名 DTO 集合（含各方法自动生成的
// Request/Reply 名——types 不得与它们撞名）。
func checkTypes(inPath string, doc *contractDoc) (map[string]bool, error) {
	named := map[string]bool{}
	for i := range doc.Types {
		t := &doc.Types[i]
		at := fmt.Sprintf("%s:%d", inPath, t.line)
		if !reEventName.MatchString(t.Name) {
			return nil, fmt.Errorf("%s: 类型名 %q 非法（须为导出 CamelCase，匹配 ^[A-Z][A-Za-z0-9]*$）", at, t.Name)
		}
		if named[t.Name] {
			return nil, fmt.Errorf("%s: 类型名 %q 重复", at, t.Name)
		}
		named[t.Name] = true
		if len(t.Fields) == 0 {
			return nil, fmt.Errorf("%s: 类型 %s 没有字段（空 struct 不是契约）", at, t.Name)
		}
		// named=nil：命名 DTO 的字段不得再引用命名 DTO，嵌套封顶一层。
		if err := checkFields(inPath, "类型 "+t.Name, t.Fields, nil); err != nil {
			return nil, err
		}
	}
	return named, nil
}

func checkMethodsDoc(inPath string, doc *contractDoc, named map[string]bool) error {
	seenName := map[string]int{}
	seenPath := map[string]int{}
	for i := range doc.Methods {
		m := &doc.Methods[i]
		at := fmt.Sprintf("%s:%d", inPath, m.line)
		if !reEventName.MatchString(m.Name) {
			return fmt.Errorf("%s: 方法名 %q 非法（须为导出 CamelCase，匹配 ^[A-Z][A-Za-z0-9]*$）", at, m.Name)
		}
		if prev, dup := seenName[m.Name]; dup {
			return fmt.Errorf("%s: 方法名 %q 与 %s:%d 重复", at, m.Name, inPath, prev)
		}
		seenName[m.Name] = m.line
		if !rePath.MatchString(m.Path) {
			return fmt.Errorf("%s: 方法 %s 的 path %q 非法（须为 / 开头的安全路径）", at, m.Name, m.Path)
		}
		if prev, dup := seenPath[m.Path]; dup {
			return fmt.Errorf("%s: path %q 与 %s:%d 重复", at, m.Path, inPath, prev)
		}
		seenPath[m.Path] = m.line
		if m.Doc == "" {
			// 契约是给别的团队读的：没有 doc 的方法不该进合约仓库。
			return fmt.Errorf("%s: 方法 %s 缺少 doc（契约方法的用途说明）", at, m.Name)
		}
		// 自动生成的 DTO 名与命名 DTO 撞名会让 service.gen.go 编译失败——拦截在生成前。
		for _, goName := range []string{m.Name + "Request", m.Name + "Reply"} {
			if named[goName] {
				return fmt.Errorf("%s: 方法 %s 将生成 DTO %s，与 types 里的命名 DTO 撞名，请改其一", at, m.Name, goName)
			}
		}
		if err := checkFields(inPath, "方法 "+m.Name+" 的 request", m.Request, named); err != nil {
			return err
		}
		if err := checkFields(inPath, "方法 "+m.Name+" 的 response", m.Response, named); err != nil {
			return err
		}
	}
	return nil
}

// checkFields 校验一组 DTO 字段。named 为可引用的命名 DTO 集合，
// nil 表示禁止引用（types 段内部）。
func checkFields(inPath, owner string, fields []fieldDef, named map[string]bool) error {
	seenField := map[string]string{} // Go 字段名 → 原始名（转换后撞名检测）
	for j := range fields {
		f := &fields[j]
		at := fmt.Sprintf("%s:%d", inPath, f.line)
		if !reFieldName.MatchString(f.Name) {
			return fmt.Errorf("%s: %s 字段名 %q 非法（须匹配 ^[a-z][a-z0-9_]*$）", at, owner, f.Name)
		}
		if _, err := resolveType(f, named); err != nil {
			return fmt.Errorf("%s: %s 字段 %q: %w", at, owner, f.Name, err)
		}
		if f.Required && f.Type != "string" {
			return fmt.Errorf("%s: %s 字段 %q：required 仅支持 string 字段（其余类型无可判定的空值）", at, owner, f.Name)
		}
		goName := camel(f.Name)
		if orig, dup := seenField[goName]; dup {
			return fmt.Errorf("%s: %s 字段 %q 与 %q 生成同名 Go 字段 %s", at, owner, f.Name, orig, goName)
		}
		seenField[goName] = f.Name
	}
	return nil
}

// resolveType 把字段类型串解析为 Go 类型表达式：标量走 goTypes，
// [] 前缀表示数组，其余匹配 types 段声明的命名 DTO。
func resolveType(f *fieldDef, named map[string]bool) (string, error) {
	t := f.Type
	if strings.HasPrefix(t, "[]") {
		elem, err := resolveType(&fieldDef{Type: t[2:]}, named)
		if err != nil {
			return "", err
		}
		return "[]" + elem, nil
	}
	if goType, ok := goTypes[t]; ok {
		return goType, nil
	}
	if named != nil && named[t] {
		return t, nil
	}
	if named == nil {
		return "", fmt.Errorf("类型 %q 不支持（命名 DTO 的字段仅允许 string|int64|bool|decimal|timestamp 及其数组，嵌套封顶一层）", t)
	}
	return "", fmt.Errorf("类型 %q 不支持（仅允许 string|int64|bool|decimal|timestamp、其数组、或 types 段声明的命名 DTO）", t)
}

// usesTime 报告生成物是否需要 import time。
func (d *contractDoc) usesTime() bool {
	has := func(fields []fieldDef) bool {
		for _, f := range fields {
			if f.Type == "timestamp" || f.Type == "[]timestamp" {
				return true
			}
		}
		return false
	}
	for _, t := range d.Types {
		if has(t.Fields) {
			return true
		}
	}
	for _, m := range d.Methods {
		if has(m.Request) || has(m.Response) {
			return true
		}
	}
	return false
}

// renderStruct 写出一个 DTO struct。doc 为 GoDoc 首行，可为空。
func renderStruct(b *bytes.Buffer, name, doc string, fields []fieldDef, named map[string]bool) {
	if doc != "" {
		fmt.Fprintf(b, "// %s %s\n", name, doc)
	}
	fmt.Fprintf(b, "type %s struct {\n", name)
	for _, f := range fields {
		goType, _ := resolveType(&f, named) // checkFields 已校验，err 不可能非 nil
		goName := camel(f.Name)
		if base := strings.TrimPrefix(f.Type, "[]"); base == "decimal" {
			fmt.Fprintf(b, "\t// %s 是十进制数字符串：用 money.Parse 解析，全系统禁 float 金额。\n", goName)
		}
		if f.Required {
			fmt.Fprintf(b, "\t// %s 必填。\n", goName)
		}
		fmt.Fprintf(b, "\t%s %s `json:%q`\n", goName, goType, f.Name)
	}
	b.WriteString("}\n\n")
}

func renderService(doc *contractDoc) []byte {
	var b bytes.Buffer
	b.WriteString(header)
	fmt.Fprintf(&b, "package %s\n\n", doc.Package)
	if doc.usesTime() {
		b.WriteString("import (\n\t\"context\"\n\t\"time\"\n)\n\n")
	} else {
		b.WriteString("import \"context\"\n\n")
	}

	named := map[string]bool{}
	for _, t := range doc.Types {
		named[t.Name] = true
	}
	for _, t := range doc.Types {
		renderStruct(&b, t.Name, t.Doc, t.Fields, nil)
	}
	for _, m := range doc.Methods {
		if len(m.Request) > 0 {
			renderStruct(&b, m.Name+"Request", "是 "+m.Name+" 的请求 DTO：契约边界传值、可序列化。", m.Request, named)
		}
		if len(m.Response) > 0 {
			renderStruct(&b, m.Name+"Reply", "是 "+m.Name+" 的响应 DTO。", m.Response, named)
		}
	}

	fmt.Fprintf(&b, "// Service 是 %s 域的契约接口（事实源 contract.yaml）。进程内 wrapper\n", doc.System)
	b.WriteString("// 与远程 client 实现同一个接口，调用方无从（也不需要）分辨对端形态。\n")
	b.WriteString("type Service interface {\n")
	for _, m := range doc.Methods {
		docLine := m.Doc
		if m.Idempotent {
			docLine += "幂等：重复执行安全，client 会对可用性故障做有界重试。"
		}
		fmt.Fprintf(&b, "\t// %s %s\n", m.Name, docLine)
		fmt.Fprintf(&b, "\t%s\n", methodSig(&m))
	}
	b.WriteString("}\n")
	return b.Bytes()
}

// methodSig 渲染接口方法签名（四种合法形态）。
func methodSig(m *methodDef) string {
	req := ""
	if len(m.Request) > 0 {
		req = ", req " + m.Name + "Request"
	}
	if len(m.Response) > 0 {
		return fmt.Sprintf("%s(ctx context.Context%s) (%sReply, error)", m.Name, req, m.Name)
	}
	return fmt.Sprintf("%s(ctx context.Context%s) error", m.Name, req)
}
