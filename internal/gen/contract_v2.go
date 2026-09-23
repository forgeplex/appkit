package gen

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

type contractDocV2 struct {
	Version int           `yaml:"version"`
	Package string        `yaml:"package"`
	System  string        `yaml:"system"`
	Types   []typeDef     `yaml:"types"`
	Methods []methodDefV2 `yaml:"methods"`
}

type methodDefV2 struct {
	Name          string     `yaml:"name"`
	Path          string     `yaml:"path"`
	Doc           string     `yaml:"doc"`
	Kind          string     `yaml:"kind"`
	Idempotent    bool       `yaml:"idempotent"`
	Request       []fieldDef `yaml:"request"`
	Response      []fieldDef `yaml:"response"`
	CursorField   string     `yaml:"cursor_field"`
	TerminalEvent string     `yaml:"terminal_event"`
	line          int
}

func (m *methodDefV2) UnmarshalYAML(n *yaml.Node) error {
	type plain methodDefV2
	if err := n.Decode((*plain)(m)); err != nil {
		return err
	}
	m.line = n.Line
	return nil
}

func parseContractV2Source(sourceName string, data []byte) (*contractDocV2, error) {
	if err := validateContractV2Keys(sourceName, data); err != nil {
		return nil, err
	}
	var doc contractDocV2
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: 解析 yaml: %w", sourceName, err)
	}
	if doc.Version != 2 {
		return nil, fmt.Errorf("%s: V2 generator requires version 2, got %d", sourceName, doc.Version)
	}
	if !rePackage.MatchString(doc.Package) {
		return nil, fmt.Errorf("%s: package %q 非法（须匹配 ^[a-z][a-z0-9]*$）", sourceName, doc.Package)
	}
	if !rePackage.MatchString(doc.System) {
		return nil, fmt.Errorf("%s: system %q 非法（须匹配 ^[a-z][a-z0-9]*$）", sourceName, doc.System)
	}
	if len(doc.Methods) == 0 {
		return nil, fmt.Errorf("%s: methods 为空，没有可生成的内容", sourceName)
	}

	legacy := &contractDoc{Types: doc.Types}
	named, err := checkTypes(sourceName, legacy)
	if err != nil {
		return nil, err
	}
	seenNames := map[string]int{}
	seenPaths := map[string]int{}
	declared := map[string]string{}
	hasUnary, hasStreaming, hasServerStream := false, false, false
	for _, method := range doc.Methods {
		switch method.Kind {
		case "unary":
			hasUnary = true
		case "server_stream":
			hasStreaming, hasServerStream = true, true
		case "bidi_stream":
			hasStreaming = true
		}
	}
	if hasUnary {
		for name, owner := range map[string]string{
			"ServiceV2":         "V2 unary Service interface",
			"ClientV2":          "V2 Unary client type",
			"WrapServiceV2":     "V2 unary wrapper function",
			"NewClientV2":       "V2 client constructor",
			"NewSecureClientV2": "V2 secure client constructor",
			"NewHTTPHandlerV2":  "V2 HTTP handler constructor",
		} {
			declared[name] = owner
		}
	}
	if hasStreaming {
		declared["StreamingServiceV2"] = "V2 Streaming Service interface"
		declared["StreamReaderV2"] = "V2 StreamReader interface"
	}
	if hasServerStream {
		declared["StreamSenderV2"] = "V2 Server Stream sender interface"
	}
	for _, typ := range doc.Types {
		if err := reserveV2Name(declared, typ.Name+"V2", "type "+typ.Name); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", sourceName, typ.line, err)
		}
	}
	for i := range doc.Methods {
		m := &doc.Methods[i]
		at := fmt.Sprintf("%s:%d", sourceName, m.line)
		if !reEventName.MatchString(m.Name) {
			return nil, fmt.Errorf("%s: 方法名 %q 非法（须为导出 CamelCase）", at, m.Name)
		}
		if prev, ok := seenNames[m.Name]; ok {
			return nil, fmt.Errorf("%s: 方法名 %q 与 %s:%d 重复", at, m.Name, sourceName, prev)
		}
		seenNames[m.Name] = m.line
		if m.Kind == "server_stream" {
			if err := reserveV2Name(declared, "New"+m.Name+"SSEHandlerV2", "SSE handler for "+m.Name); err != nil {
				return nil, fmt.Errorf("%s: %w", at, err)
			}
		}
		if m.Kind == "server_stream" || m.Kind == "bidi_stream" {
			if err := reserveV2Name(declared, "Open"+m.Name+"LocalV2", "Local stream opener for "+m.Name); err != nil {
				return nil, fmt.Errorf("%s: %w", at, err)
			}
		}
		if !rePath.MatchString(m.Path) {
			return nil, fmt.Errorf("%s: 方法 %s 的 path %q 非法（须为 / 开头的安全路径）", at, m.Name, m.Path)
		}
		if prev, ok := seenPaths[m.Path]; ok {
			return nil, fmt.Errorf("%s: path %q 与 %s:%d 重复", at, m.Path, sourceName, prev)
		}
		seenPaths[m.Path] = m.line
		if m.Doc == "" {
			return nil, fmt.Errorf("%s: 方法 %s 缺少 doc（契约方法的用途说明）", at, m.Name)
		}
		switch m.Kind {
		case "unary", "server_stream", "bidi_stream":
		default:
			return nil, fmt.Errorf("%s: 方法 %s 的 kind %q 非法（须为 unary|server_stream|bidi_stream）", at, m.Name, m.Kind)
		}
		if m.Kind != "unary" && m.Idempotent {
			return nil, fmt.Errorf("%s: Streaming 方法 %s 不能声明 idempotent；框架不自动重试 Stream", at, m.Name)
		}
		if err := checkFields(sourceName, "方法 "+m.Name+" 的 request", m.Request, named); err != nil {
			return nil, err
		}
		if err := checkFields(sourceName, "方法 "+m.Name+" 的 response", m.Response, named); err != nil {
			return nil, err
		}
		if len(m.Request) > 0 {
			if err := reserveV2Name(declared, m.Name+"RequestV2", "request DTO for "+m.Name); err != nil {
				return nil, fmt.Errorf("%s: %w", at, err)
			}
		}
		if len(m.Response) > 0 {
			if err := reserveV2Name(declared, m.Name+"ResponseV2", "response DTO for "+m.Name); err != nil {
				return nil, fmt.Errorf("%s: %w", at, err)
			}
		}
		switch m.Kind {
		case "unary":
			if m.CursorField != "" || m.TerminalEvent != "" {
				return nil, fmt.Errorf("%s: Unary 方法 %s 不能声明 cursor_field 或 terminal_event", at, m.Name)
			}
		case "server_stream":
			if len(m.Response) == 0 {
				return nil, fmt.Errorf("%s: Server Stream 方法 %s 必须声明 response 事件字段", at, m.Name)
			}
		case "bidi_stream":
			if len(m.Request) == 0 || len(m.Response) == 0 {
				return nil, fmt.Errorf("%s: Bidi Stream 方法 %s 必须声明 request 与 response 消息字段", at, m.Name)
			}
			if m.CursorField != "" {
				return nil, fmt.Errorf("%s: Bidi Stream 方法 %s 不支持 SSE cursor_field", at, m.Name)
			}
		}
		if m.CursorField != "" {
			if m.Kind != "server_stream" {
				return nil, fmt.Errorf("%s: cursor_field 仅适用于 server_stream", at)
			}
			var found bool
			for _, field := range m.Response {
				if field.Name == m.CursorField && field.Type == "string" {
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("%s: cursor_field %q 必须引用 response 中的 string 字段", at, m.CursorField)
			}
		}
		if m.TerminalEvent != "" && !reTopic.MatchString(m.TerminalEvent) {
			return nil, fmt.Errorf("%s: terminal_event %q 非法（须匹配事件标识格式）", at, m.TerminalEvent)
		}
	}
	return &doc, nil
}

func validateContractV2Keys(sourceName string, data []byte) error {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("%s: 解析 yaml: %w", sourceName, err)
	}
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = *root.Content[0]
	}
	if err := checkV2MappingKeys(sourceName, root, "contract", "version", "package", "system", "types", "methods"); err != nil {
		return err
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i].Value, root.Content[i+1]
		switch key {
		case "types":
			if value.Kind != yaml.SequenceNode {
				continue
			}
			for _, typ := range value.Content {
				if err := checkV2MappingKeys(sourceName, *typ, "type", "name", "doc", "fields"); err != nil {
					return err
				}
				for j := 0; j+1 < len(typ.Content); j += 2 {
					if typ.Content[j].Value != "fields" || typ.Content[j+1].Kind != yaml.SequenceNode {
						continue
					}
					for _, field := range typ.Content[j+1].Content {
						if err := checkV2MappingKeys(sourceName, *field, "field", "name", "type", "required"); err != nil {
							return err
						}
					}
				}
			}
		case "methods":
			if value.Kind != yaml.SequenceNode {
				continue
			}
			for _, method := range value.Content {
				if err := checkV2MappingKeys(sourceName, *method, "method", "name", "path", "doc", "kind", "idempotent", "request", "response", "cursor_field", "terminal_event"); err != nil {
					return err
				}
				for j := 0; j+1 < len(method.Content); j += 2 {
					key := method.Content[j].Value
					if (key != "request" && key != "response") || method.Content[j+1].Kind != yaml.SequenceNode {
						continue
					}
					for _, field := range method.Content[j+1].Content {
						if err := checkV2MappingKeys(sourceName, *field, "field", "name", "type", "required"); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}

func checkV2MappingKeys(sourceName string, node yaml.Node, owner string, allowed ...string) error {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	keys := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		keys[key] = true
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		if !keys[key.Value] {
			return fmt.Errorf("%s:%d: unknown %s field %q", sourceName, key.Line, owner, key.Value)
		}
	}
	return nil
}

func reserveV2Name(names map[string]string, name, owner string) error {
	if previous, exists := names[name]; exists {
		return fmt.Errorf("生成的 Go 声明 %s 与 %s 冲突", name, previous)
	}
	names[name] = owner
	return nil
}

func (d *contractDocV2) legacyDoc() *contractDoc {
	legacy := &contractDoc{Version: 2, Package: d.Package, System: d.System}
	named := d.namedTypes()
	for _, typ := range d.Types {
		copyType := typ
		copyType.Name += "V2"
		copyType.Fields = v2Fields(typ.Fields, named)
		legacy.Types = append(legacy.Types, copyType)
	}
	for _, method := range d.Methods {
		legacy.Methods = append(legacy.Methods, methodDef{
			Name: method.Name, Path: method.Path, Doc: method.Doc,
			Idempotent: method.Idempotent,
			Request:    v2Fields(method.Request, named),
			Response:   v2Fields(method.Response, named),
			line:       method.line,
		})
	}
	return legacy
}

func (d *contractDocV2) namedTypes() map[string]bool {
	named := make(map[string]bool, len(d.Types))
	for _, typ := range d.Types {
		named[typ.Name] = true
	}
	return named
}

func v2Fields(fields []fieldDef, named map[string]bool) []fieldDef {
	out := append([]fieldDef(nil), fields...)
	for i := range out {
		out[i].Type = v2TypeName(out[i].Type, named)
	}
	return out
}

func v2TypeName(name string, named map[string]bool) string {
	if strings.HasPrefix(name, "[]") {
		return "[]" + v2TypeName(name[2:], named)
	}
	if named[name] {
		return name + "V2"
	}
	return name
}

func v2RequestType(m methodDefV2) string {
	if len(m.Request) == 0 {
		return "struct{}"
	}
	return m.Name + "RequestV2"
}

func v2ResponseType(m methodDefV2) string {
	if len(m.Response) == 0 {
		return "struct{}"
	}
	return m.Name + "ResponseV2"
}

func renderContractV2(doc *contractDocV2) map[string][]byte {
	return map[string][]byte{
		"service_v2.gen.go": renderServiceV2(doc),
		"client_v2.gen.go":  renderClientV2(doc),
		"server_v2.gen.go":  renderServerV2(doc),
		"openapi_v2.yaml":   renderOpenAPIV2(doc),
	}
}

func renderServiceV2(doc *contractDocV2) []byte {
	legacy := doc.legacyDoc()
	var b bytes.Buffer
	b.WriteString(header)
	fmt.Fprintf(&b, "package %s\n\n", doc.Package)
	b.WriteString("import (\n\t\"context\"\n")
	if legacy.usesTime() {
		b.WriteString("\t\"time\"\n")
	}
	if hasV2BidiStream(doc) {
		b.WriteString("\n\t\"github.com/forgeplex/appkit/contract\"\n")
	}
	if legacy.usesRefs() {
		b.WriteString("\t\"github.com/forgeplex/appkit/refs\"\n")
	}
	b.WriteString(")\n\n")

	named := map[string]bool{}
	for _, typ := range legacy.Types {
		named[typ.Name] = true
	}
	for _, typ := range legacy.Types {
		renderStruct(&b, typ.Name, typ.Doc, typ.Fields, named)
	}
	for i, method := range doc.Methods {
		legacyMethod := legacy.Methods[i]
		if len(method.Request) > 0 {
			renderStruct(&b, method.Name+"RequestV2", method.Name+" V2 request DTO.", legacyMethod.Request, named)
		}
		if len(method.Response) > 0 {
			docText := method.Name + " V2 response DTO."
			if method.Kind == "server_stream" || method.Kind == "bidi_stream" {
				docText = method.Name + " V2 response message DTO."
			}
			renderStruct(&b, method.Name+"ResponseV2", docText, legacyMethod.Response, named)
		}
	}

	if hasV2Unary(doc) {
		b.WriteString("// ServiceV2 contains only V2 Unary methods. It is separate from the V1 Service interface.\ntype ServiceV2 interface {\n")
		for _, method := range doc.Methods {
			if method.Kind == "unary" {
				writeV2UnarySignature(&b, method)
			}
		}
		b.WriteString("}\n\n")
	}
	if hasV2Streaming(doc) {
		b.WriteString("// StreamingServiceV2 contains V2 stream producers. Server Stream request is received once; Bidi Stream uses request and response as peer message types.\ntype StreamingServiceV2 interface {\n")
		for _, method := range doc.Methods {
			if method.Kind == "unary" {
				continue
			}
			if method.Kind == "server_stream" {
				fmt.Fprintf(&b, "\t// %s sends ordered response events. cursor is the opaque application cursor from Last-Event-ID.\n", method.Name)
				request := ""
				if len(method.Request) > 0 {
					request = ", req " + v2RequestType(method)
				}
				fmt.Fprintf(&b, "\t%s(ctx context.Context, cursor string%s, sender StreamSenderV2[%s]) error\n", method.Name, request, v2ResponseType(method))
			} else {
				fmt.Fprintf(&b, "\t// %s exchanges ordered peer messages.\n\t%s(ctx context.Context, stream contract.Stream[%s, %s]) error\n", method.Name, method.Name, v2ResponseType(method), v2RequestType(method))
			}
		}
		b.WriteString("}\n")
		if hasV2ServerStream(doc) {
			b.WriteString("\n// StreamSenderV2 sends one ordered response message. A full local queue applies backpressure.\ntype StreamSenderV2[T any] interface {\n\tSend(context.Context, T) error\n}\n")
		}
	}
	return b.Bytes()
}

func writeV2UnarySignature(b *bytes.Buffer, m methodDefV2) {
	request := ""
	if len(m.Request) > 0 {
		request = ", req " + v2RequestType(m)
	}
	if len(m.Response) > 0 {
		fmt.Fprintf(b, "\t// %s %s\n\t%s(ctx context.Context%s) (%s, error)\n", m.Name, m.Doc, m.Name, request, v2ResponseType(m))
		return
	}
	fmt.Fprintf(b, "\t// %s %s\n\t%s(ctx context.Context%s) error\n", m.Name, m.Doc, m.Name, request)
}

func hasV2Unary(doc *contractDocV2) bool {
	for _, method := range doc.Methods {
		if method.Kind == "unary" {
			return true
		}
	}
	return false
}

func hasV2Streaming(doc *contractDocV2) bool {
	return len(doc.Methods) != v2UnaryCount(doc)
}

func hasV2ServerStream(doc *contractDocV2) bool {
	for _, method := range doc.Methods {
		if method.Kind == "server_stream" {
			return true
		}
	}
	return false
}

func hasV2BidiStream(doc *contractDocV2) bool {
	for _, method := range doc.Methods {
		if method.Kind == "bidi_stream" {
			return true
		}
	}
	return false
}

func v2UnaryCount(doc *contractDocV2) int {
	n := 0
	for _, method := range doc.Methods {
		if method.Kind == "unary" {
			n++
		}
	}
	return n
}

func renderOpenAPIV2(doc *contractDocV2) []byte {
	legacy := doc.legacyDoc()
	named := map[string]bool{}
	for _, typ := range legacy.Types {
		named[typ.Name] = true
	}
	var b bytes.Buffer
	b.WriteString("# Code generated by appkit gen. DO NOT EDIT.\n")
	b.WriteString("# V2 contract.yaml is the source of truth; streaming shapes use x-appkit-* extensions.\n")
	b.WriteString("openapi: 3.1.0\ninfo:\n")
	fmt.Fprintf(&b, "  title: %s\n", strconv.Quote(doc.System+" V2 契约"))
	b.WriteString("  version: \"2\"\npaths:\n")
	for _, method := range doc.Methods {
		fmt.Fprintf(&b, "  %s:\n    post:\n", strconv.Quote(method.Path))
		fmt.Fprintf(&b, "      operationId: %s\n", strconv.Quote(method.Name))
		fmt.Fprintf(&b, "      description: %s\n", strconv.Quote(method.Doc))
		fmt.Fprintf(&b, "      x-appkit-call-shape: %s\n", strconv.Quote(method.Kind))
		if method.Idempotent {
			b.WriteString("      x-idempotent: true\n")
		}
		if len(method.Request) > 0 && method.Kind != "bidi_stream" || method.Kind == "server_stream" {
			b.WriteString("      requestBody:\n        required: true\n        content:\n          application/json:\n            schema:\n")
			if len(method.Request) > 0 {
				fmt.Fprintf(&b, "              $ref: %s\n", strconv.Quote("#/components/schemas/"+method.Name+"RequestV2"))
			} else {
				b.WriteString("              type: object\n")
			}
		}
		b.WriteString("      responses:\n")
		switch method.Kind {
		case "unary":
			b.WriteString("        \"200\":\n          description: 成功\n")
			if len(method.Response) > 0 {
				b.WriteString("          content:\n            application/json:\n              schema:\n")
				fmt.Fprintf(&b, "                $ref: %s\n", strconv.Quote("#/components/schemas/"+method.Name+"ResponseV2"))
			}
		case "server_stream":
			b.WriteString("        \"200\":\n          description: SSE response messages; stream errors after response commit are in-band.\n          content:\n            text/event-stream:\n              schema:\n                type: string\n")
		case "bidi_stream":
			b.WriteString("        \"200\":\n          description: Transport-neutral bidirectional stream; wire transport is defined separately.\n")
		}
		b.WriteString("        default:\n          description: 错误（错误身份 = code 扩展成员）\n          content:\n            application/problem+json:\n              schema:\n                $ref: '#/components/schemas/Problem'\n")
		if method.Kind != "unary" {
			b.WriteString("      x-appkit-stream:\n")
			if len(method.Request) > 0 {
				fmt.Fprintf(&b, "        requestMessage: %s\n", strconv.Quote(method.Name+"RequestV2"))
			}
			fmt.Fprintf(&b, "        responseMessage: %s\n", strconv.Quote(method.Name+"ResponseV2"))
			if method.TerminalEvent != "" {
				fmt.Fprintf(&b, "        terminalEvent: %s\n", strconv.Quote(method.TerminalEvent))
			}
			if method.CursorField != "" {
				fmt.Fprintf(&b, "        cursorField: %s\n", strconv.Quote(method.CursorField))
			}
		}
	}
	b.WriteString("components:\n  schemas:\n")
	for _, typ := range legacy.Types {
		renderOpenAPISchema(&b, typ.Name, typ.Doc, typ.Fields, nil)
	}
	for i, method := range doc.Methods {
		legacyMethod := legacy.Methods[i]
		if len(method.Request) > 0 {
			renderOpenAPISchema(&b, method.Name+"RequestV2", method.Name+" V2 request DTO.", legacyMethod.Request, named)
		}
		if len(method.Response) > 0 {
			renderOpenAPISchema(&b, method.Name+"ResponseV2", method.Name+" V2 response DTO.", legacyMethod.Response, named)
		}
	}
	b.WriteString("    Problem:\n      type: object\n      description: RFC 9457 problem+json (apperr.Problem).\n      properties:\n        title:\n          type: string\n        status:\n          type: integer\n        code:\n          type: string\n      required:\n        - title\n        - status\n        - code\n")
	return b.Bytes()
}
