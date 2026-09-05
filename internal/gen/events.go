package gen

import (
	"bytes"
	"fmt"
	"os"
	"regexp"

	yaml "go.yaml.in/yaml/v3"
)

// eventsDoc 是 events.yaml 的顶层 schema。
type eventsDoc struct {
	Version int        `yaml:"version"`
	Package string     `yaml:"package"`
	Events  []eventDef `yaml:"events"`
}

type eventDef struct {
	Name   string     `yaml:"name"`
	Topic  string     `yaml:"topic"`
	Fields []fieldDef `yaml:"fields"`
	line   int        // yaml 行号，用于报错定位
}

func (e *eventDef) UnmarshalYAML(n *yaml.Node) error {
	type plain eventDef
	if err := n.Decode((*plain)(e)); err != nil {
		return err
	}
	e.line = n.Line
	return nil
}

type fieldDef struct {
	Name     string `yaml:"name"`
	Type     string `yaml:"type"`
	Required bool   `yaml:"required"`
	line     int
}

func (f *fieldDef) UnmarshalYAML(n *yaml.Node) error {
	type plain fieldDef
	if err := n.Decode((*plain)(f)); err != nil {
		return err
	}
	f.line = n.Line
	return nil
}

// goTypes 是 schema 字段类型到 Go 类型的映射。decimal 刻意映射为 string：
// 全系统禁 float 金额，解析统一走 money.Parse。
var goTypes = map[string]string{
	"string":    "string",
	"int64":     "int64",
	"bool":      "bool",
	"decimal":   "string",
	"timestamp": "time.Time",
}

var (
	reEventName = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	reFieldName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	reTopic     = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
)

// Events 读取事件 schema yaml，生成事件类型文件（struct + Topic 常量 +
// Event()/Parse* 方法）。见 appkit gen events。
func Events(inPath, outPath string) error {
	if inPath == "" || outPath == "" {
		return fmt.Errorf("gen events 需要 -in <events.yaml> 与 -out <file.go>")
	}
	data, err := os.ReadFile(inPath)
	if err != nil {
		return fmt.Errorf("读取输入: %w", err)
	}
	source, err := RenderEventsSource(inPath, data)
	if err != nil {
		return err
	}
	return writeGenerated(outPath, source)
}

// RenderEventsSource renders exactly the supplied events.yaml snapshot. It
// performs no filesystem access; sourceName is used only for diagnostics. The
// formatted result belongs to the caller and does not alias data.
func RenderEventsSource(sourceName string, data []byte) ([]byte, error) {
	doc, err := parseEventsSource(sourceName, data)
	if err != nil {
		return nil, err
	}
	return formatGo(sourceName, renderEvents(doc))
}

func parseEventsSource(inPath string, data []byte) (*eventsDoc, error) {
	var doc eventsDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: 解析 yaml: %w", inPath, err)
	}
	if doc.Version != 1 {
		return nil, fmt.Errorf("%s: 不支持的 version %d（当前仅支持 1）", inPath, doc.Version)
	}
	if !rePackage.MatchString(doc.Package) {
		return nil, fmt.Errorf("%s: package %q 非法（须匹配 ^[a-z][a-z0-9]*$）", inPath, doc.Package)
	}
	if len(doc.Events) == 0 {
		return nil, fmt.Errorf("%s: events 为空，没有可生成的内容", inPath)
	}

	seenName := map[string]int{}
	seenTopic := map[string]int{}
	for i := range doc.Events {
		ev := &doc.Events[i]
		at := fmt.Sprintf("%s:%d", inPath, ev.line)
		if !reEventName.MatchString(ev.Name) {
			return nil, fmt.Errorf("%s: 事件名 %q 非法（须为导出 CamelCase，匹配 ^[A-Z][A-Za-z0-9]*$）", at, ev.Name)
		}
		if prev, dup := seenName[ev.Name]; dup {
			return nil, fmt.Errorf("%s: 事件名 %q 与 %s:%d 重复", at, ev.Name, inPath, prev)
		}
		seenName[ev.Name] = ev.line
		if !reTopic.MatchString(ev.Topic) {
			return nil, fmt.Errorf("%s: 事件 %s 的 topic %q 非法（形如 ledger.entry.posted）", at, ev.Name, ev.Topic)
		}
		if prev, dup := seenTopic[ev.Topic]; dup {
			return nil, fmt.Errorf("%s: topic %q 与 %s:%d 重复", at, ev.Topic, inPath, prev)
		}
		seenTopic[ev.Topic] = ev.line

		seenField := map[string]string{} // Go 字段名 → 原始名（含转换后撞名检测）
		for j := range ev.Fields {
			f := &ev.Fields[j]
			fat := fmt.Sprintf("%s:%d", inPath, f.line)
			if !reFieldName.MatchString(f.Name) {
				return nil, fmt.Errorf("%s: 事件 %s 字段名 %q 非法（须匹配 ^[a-z][a-z0-9_]*$）", fat, ev.Name, f.Name)
			}
			if _, ok := goTypes[f.Type]; !ok {
				return nil, fmt.Errorf("%s: 事件 %s 字段 %q 类型 %q 不支持（仅允许 string|int64|bool|decimal|timestamp）", fat, ev.Name, f.Name, f.Type)
			}
			if f.Required && f.Type != "string" {
				return nil, fmt.Errorf("%s: 事件 %s 字段 %q：required 仅支持 string 字段（其余类型无可判定的空值）", fat, ev.Name, f.Name)
			}
			goName := camel(f.Name)
			if goName == "Event" {
				return nil, fmt.Errorf("%s: 事件 %s 字段 %q 会生成字段 Event，与 Event() 方法冲突，请改名", fat, ev.Name, f.Name)
			}
			if orig, dup := seenField[goName]; dup {
				return nil, fmt.Errorf("%s: 事件 %s 字段 %q 与 %q 生成同名 Go 字段 %s", fat, ev.Name, f.Name, orig, goName)
			}
			seenField[goName] = f.Name
		}
	}
	return &doc, nil
}

func renderEvents(doc *eventsDoc) []byte {
	usesTime := false
	for _, ev := range doc.Events {
		for _, f := range ev.Fields {
			if f.Type == "timestamp" {
				usesTime = true
			}
		}
	}

	var b bytes.Buffer
	b.WriteString(header)
	fmt.Fprintf(&b, "package %s\n\n", doc.Package)
	b.WriteString("import (\n\t\"encoding/json\"\n\t\"fmt\"\n")
	if usesTime {
		b.WriteString("\t\"time\"\n")
	}
	b.WriteString("\n\t\"github.com/forgeplex/appkit\"\n)\n\n")

	for _, ev := range doc.Events {
		fmt.Fprintf(&b, "// Topic%s 是 %s 事件的 topic。\n", ev.Name, ev.Name)
		fmt.Fprintf(&b, "const Topic%s = %q\n\n", ev.Name, ev.Topic)

		fmt.Fprintf(&b, "// %s 事件 payload（topic: %s）。\n", ev.Name, ev.Topic)
		fmt.Fprintf(&b, "type %s struct {\n", ev.Name)
		for _, f := range ev.Fields {
			goName := camel(f.Name)
			if f.Type == "decimal" {
				fmt.Fprintf(&b, "\t// %s 是十进制数字符串：用 money.Parse 解析，全系统禁 float 金额。\n", goName)
			}
			fmt.Fprintf(&b, "\t%s %s `json:%q`\n", goName, goTypes[f.Type], f.Name)
		}
		b.WriteString("}\n\n")

		fmt.Fprintf(&b, "// Event 把事件序列化为 appkit.Event。ID 留空，由 outbox 发布时填充。\n")
		fmt.Fprintf(&b, "func (e %s) Event() (appkit.Event, error) {\n", ev.Name)
		fmt.Fprintf(&b, "\tpayload, err := json.Marshal(e)\n")
		fmt.Fprintf(&b, "\tif err != nil {\n\t\treturn appkit.Event{}, fmt.Errorf(\"序列化 %s: %%w\", err)\n\t}\n", ev.Name)
		fmt.Fprintf(&b, "\treturn appkit.Event{Topic: Topic%s, Payload: payload}, nil\n}\n\n", ev.Name)

		fmt.Fprintf(&b, "// Parse%s 从 appkit.Event 还原 %s：校验 topic 与必填字段。\n", ev.Name, ev.Name)
		fmt.Fprintf(&b, "func Parse%s(evt appkit.Event) (%s, error) {\n", ev.Name, ev.Name)
		fmt.Fprintf(&b, "\tif evt.Topic != Topic%s {\n", ev.Name)
		fmt.Fprintf(&b, "\t\treturn %s{}, fmt.Errorf(\"Parse%s: topic 不匹配：期望 %%q，实际 %%q\", Topic%s, evt.Topic)\n\t}\n", ev.Name, ev.Name, ev.Name)
		fmt.Fprintf(&b, "\tvar e %s\n", ev.Name)
		fmt.Fprintf(&b, "\tif err := json.Unmarshal(evt.Payload, &e); err != nil {\n")
		fmt.Fprintf(&b, "\t\treturn %s{}, fmt.Errorf(\"Parse%s: 解析 payload: %%w\", err)\n\t}\n", ev.Name, ev.Name)
		for _, f := range ev.Fields {
			if f.Required && f.Type == "string" {
				fmt.Fprintf(&b, "\tif e.%s == \"\" {\n", camel(f.Name))
				fmt.Fprintf(&b, "\t\treturn %s{}, fmt.Errorf(\"Parse%s: 必填字段 %s 为空\")\n\t}\n", ev.Name, ev.Name, f.Name)
			}
		}
		b.WriteString("\treturn e, nil\n}\n\n")
	}
	return b.Bytes()
}
