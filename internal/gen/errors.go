package gen

import (
	"bytes"
	"fmt"
	"os"
	"regexp"

	yaml "go.yaml.in/yaml/v3"
)

// codesDoc 是 codes.yaml 的顶层 schema。
type codesDoc struct {
	Version int       `yaml:"version"`
	Package string    `yaml:"package"`
	Codes   []codeDef `yaml:"codes"`
}

type codeDef struct {
	Code    string `yaml:"code"`
	Status  int    `yaml:"status"`
	Message string `yaml:"message"`
	line    int
}

func (c *codeDef) UnmarshalYAML(n *yaml.Node) error {
	type plain codeDef
	if err := n.Decode((*plain)(c)); err != nil {
		return err
	}
	c.line = n.Line
	return nil
}

// reCode：SCREAMING_SNAKE_CASE，如 LEDGER_INSUFFICIENT_FUNDS。
var reCode = regexp.MustCompile(`^[A-Z][A-Z0-9]*(_[A-Z0-9]+)*$`)

// Errors 读取错误码 yaml，生成 Code* 常量与 Err* sentinel。见 appkit gen errors。
func Errors(inPath, outPath string) error {
	if inPath == "" || outPath == "" {
		return fmt.Errorf("gen errors 需要 -in <codes.yaml> 与 -out <file.go>")
	}
	doc, err := parseCodesDoc(inPath)
	if err != nil {
		return err
	}
	return writeGo(outPath, renderCodes(doc))
}

func parseCodesDoc(inPath string) (*codesDoc, error) {
	data, err := os.ReadFile(inPath)
	if err != nil {
		return nil, fmt.Errorf("读取输入: %w", err)
	}
	var doc codesDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: 解析 yaml: %w", inPath, err)
	}
	if doc.Version != 1 {
		return nil, fmt.Errorf("%s: 不支持的 version %d（当前仅支持 1）", inPath, doc.Version)
	}
	if !rePackage.MatchString(doc.Package) {
		return nil, fmt.Errorf("%s: package %q 非法（须匹配 ^[a-z][a-z0-9]*$）", inPath, doc.Package)
	}
	if len(doc.Codes) == 0 {
		return nil, fmt.Errorf("%s: codes 为空，没有可生成的内容", inPath)
	}

	seen := map[string]int{}
	seenCamel := map[string]string{}
	for i := range doc.Codes {
		c := &doc.Codes[i]
		at := fmt.Sprintf("%s:%d", inPath, c.line)
		if !reCode.MatchString(c.Code) {
			return nil, fmt.Errorf("%s: 错误码 %q 非法（须为 SCREAMING_SNAKE_CASE，如 LEDGER_INSUFFICIENT_FUNDS）", at, c.Code)
		}
		if prev, dup := seen[c.Code]; dup {
			return nil, fmt.Errorf("%s: 错误码 %q 与 %s:%d 重复", at, c.Code, inPath, prev)
		}
		seen[c.Code] = c.line
		name := camel(c.Code)
		if orig, dup := seenCamel[name]; dup {
			return nil, fmt.Errorf("%s: 错误码 %q 与 %q 生成同名标识符 %s", at, c.Code, orig, name)
		}
		seenCamel[name] = c.Code
		if c.Status < 100 || c.Status > 599 {
			return nil, fmt.Errorf("%s: 错误码 %s 的 status %d 越界（须为 100–599 的 HTTP 状态码）", at, c.Code, c.Status)
		}
		if c.Message == "" {
			return nil, fmt.Errorf("%s: 错误码 %s 缺少 message", at, c.Code)
		}
	}
	return &doc, nil
}

func renderCodes(doc *codesDoc) []byte {
	var b bytes.Buffer
	b.WriteString(header)
	fmt.Fprintf(&b, "package %s\n\n", doc.Package)
	b.WriteString("import \"github.com/forgeplex/appkit/apperr\"\n\n")

	b.WriteString("// 错误码常量：错误身份 = 错误码，判定用 apperr.Is(err, Code...)，\n")
	b.WriteString("// 单体与微服务两种部署形态下行为一致。\n")
	b.WriteString("const (\n")
	for _, c := range doc.Codes {
		fmt.Fprintf(&b, "\tCode%s = %q\n", camel(c.Code), c.Code)
	}
	b.WriteString(")\n\n")

	b.WriteString("// 错误 sentinel：不可变模板，可安全共享；附加上下文用 WithDetail/WithCause。\n")
	b.WriteString("var (\n")
	for _, c := range doc.Codes {
		name := camel(c.Code)
		fmt.Fprintf(&b, "\tErr%s = apperr.New(Code%s, %d, %q)\n", name, name, c.Status, c.Message)
	}
	b.WriteString(")\n")
	return b.Bytes()
}
