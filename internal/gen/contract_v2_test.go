package gen

import (
	"bytes"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

func TestContractV2RenderAndOpenAPI(t *testing.T) {
	input, err := os.ReadFile("testdata/contract_v2.yaml")
	if err != nil {
		t.Fatal(err)
	}
	files, err := RenderContractSource("fixture/contract.yaml", input)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(contractV2Filenames) {
		t.Fatalf("generated %d files, want %d", len(files), len(contractV2Filenames))
	}
	for _, name := range contractV2Filenames {
		if _, ok := files[name]; !ok {
			t.Errorf("V2 output missing %s", name)
		}
	}
	for _, name := range []string{"service_v2.gen.go", "client_v2.gen.go", "server_v2.gen.go"} {
		if _, err := parser.ParseFile(token.NewFileSet(), name, files[name], parser.AllErrors); err != nil {
			t.Errorf("generated %s is invalid Go: %v", name, err)
		}
	}
	service := string(files["service_v2.gen.go"])
	for _, want := range []string{
		"type ServiceV2 interface",
		"type StreamingServiceV2 interface",
		"type EventMetaV2 struct",
		"type WatchRequestV2 struct",
		"type WatchResponseV2 struct",
		"Watch(ctx context.Context, cursor string, req WatchRequestV2, sender StreamSenderV2[WatchResponseV2]) error",
		"Chat(ctx context.Context, stream contract.Stream[ChatResponseV2, ChatRequestV2]) error",
	} {
		if !strings.Contains(service, want) {
			t.Errorf("V2 service missing %q", want)
		}
	}
	if strings.Contains(service, "type Service interface") {
		t.Fatal("V2 widened or replaced the existing V1 Service interface")
	}
	var openapi struct {
		OpenAPI string `yaml:"openapi"`
		Paths   map[string]struct {
			Post struct {
				CallShape   string `yaml:"x-appkit-call-shape"`
				RequestBody struct {
					Required bool `yaml:"required"`
					Content  map[string]struct {
						Schema struct {
							Type string `yaml:"type"`
						} `yaml:"schema"`
					} `yaml:"content"`
				} `yaml:"requestBody"`
				Stream struct {
					RequestMessage  string `yaml:"requestMessage"`
					ResponseMessage string `yaml:"responseMessage"`
					CursorField     string `yaml:"cursorField"`
					TerminalEvent   string `yaml:"terminalEvent"`
				} `yaml:"x-appkit-stream"`
			} `yaml:"post"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(files["openapi_v2.yaml"], &openapi); err != nil {
		t.Fatalf("V2 OpenAPI is invalid YAML: %v\n%s", err, files["openapi_v2.yaml"])
	}
	watch := openapi.Paths["/v2/watch"].Post
	if openapi.OpenAPI != "3.1.0" || watch.CallShape != "server_stream" || watch.Stream.CursorField != "event_id" || watch.Stream.TerminalEvent != "feed.completed" {
		t.Fatalf("OpenAPI V2 stream extension = %+v, document version %q", watch, openapi.OpenAPI)
	}
	tail := openapi.Paths["/v2/tail"].Post
	if !tail.RequestBody.Required || tail.RequestBody.Content["application/json"].Schema.Type != "object" || tail.Stream.RequestMessage != "" {
		t.Fatalf("request-less Server Stream has invalid OpenAPI request shape: %+v", tail)
	}
	if string(files["openapi_v2.yaml"]) == string(mustRead(t, "genfixture/openapi.yaml")) {
		t.Fatal("V2 OpenAPI unexpectedly equals the V1 document")
	}
}

func TestContractV2RejectsGeneratedDeclarationCollisions(t *testing.T) {
	base := string(mustRead(t, "testdata/contract_v2.yaml"))
	for _, name := range []string{"Client", "NewHTTPHandler", "StreamSender", "OpenWatchLocal"} {
		t.Run(name, func(t *testing.T) {
			input := strings.ReplaceAll(base, "EventMeta", name)
			_, err := RenderContractSource("collision-v2.yaml", []byte(input))
			if err == nil || !strings.Contains(err.Error(), "生成的 Go 声明") {
				t.Fatalf("collision error = %v, want generated declaration conflict", err)
			}
		})
	}
}

func TestContractV2CoexistsWithV1AndChecksDrift(t *testing.T) {
	dir := t.TempDir()
	if err := Contract("testdata/contract.yaml", dir); err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for _, name := range contractFilenames {
		before[name] = mustRead(t, filepath.Join(dir, name))
	}
	if err := Contract("testdata/contract_v2.yaml", dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range contractFilenames {
		if got := mustRead(t, filepath.Join(dir, name)); !bytes.Equal(got, before[name]) {
			t.Errorf("V2 generation changed existing V1 output %s", name)
		}
	}
	if err := CheckContract("testdata/contract.yaml", dir); err != nil {
		t.Errorf("V1 drift check after V2 generation: %v", err)
	}
	if err := CheckContract("testdata/contract_v2.yaml", dir); err != nil {
		t.Errorf("V2 drift check: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "client_v2.gen.go"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckContract("testdata/contract_v2.yaml", dir); !errors.Is(err, ErrContractDrift) {
		t.Fatalf("stale V2 output not detected: %v", err)
	}
}

func TestContractV2RejectsInvalidShapesAndMetadata(t *testing.T) {
	base := string(mustRead(t, "testdata/contract_v2.yaml"))
	for _, tc := range []struct {
		name string
		from string
		to   string
		want string
	}{
		{"missing kind", "    kind: server_stream\n", "", "kind"},
		{"unknown kind", "kind: bidi_stream", "kind: client_stream", "kind"},
		{"unknown method key", "kind: server_stream\n", "kind: server_stream\n    transprot: sse\n", "unknown method field"},
		{"bad cursor", "cursor_field: event_id", "cursor_field: missing", "cursor_field"},
		{"terminal on unary", "kind: unary\n    idempotent: true", "kind: unary\n    terminal_event: done", "terminal_event"},
		{"idempotent stream", "kind: server_stream", "kind: server_stream\n    idempotent: true", "不能声明 idempotent"},
		{"bidi missing request", "kind: bidi_stream\n    request:\n      - {name: text, type: string, required: true}", "kind: bidi_stream", "必须声明 request 与 response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := base
			if tc.name == "missing kind" {
				input = strings.Replace(input, tc.from, tc.to, 1)
			} else {
				input = strings.Replace(input, tc.from, tc.to, 1)
			}
			if input == base {
				t.Fatal("test mutation did not change the fixture")
			}
			_, err := RenderContractSource("bad-v2.yaml", []byte(input))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestContractV1RejectsV2StreamingSyntax(t *testing.T) {
	input := string(mustRead(t, "testdata/contract.yaml"))
	input = strings.Replace(input, "  - name: Greet\n", "  - name: Greet\n    kind: server_stream\n", 1)
	if _, err := RenderContractSource("v1-with-stream.yaml", []byte(input)); err == nil || !strings.Contains(err.Error(), "V1 Unary contract does not accept V2 method field") {
		t.Fatalf("V1 silently accepted V2 stream syntax: %v", err)
	}
}

func TestContractV2CompatibilityRules(t *testing.T) {
	base, err := os.ReadFile("testdata/contract_v2.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*contractDocV2)
		want   string
	}{
		{"identical", func(*contractDocV2) {}, ""},
		{"docs", func(d *contractDocV2) { d.Methods[0].Doc = "new docs" }, ""},
		{"optional field", func(d *contractDocV2) {
			d.Methods[0].Request = append(d.Methods[0].Request, fieldDef{Name: "locale", Type: "string"})
		}, ""},
		{"shape", func(d *contractDocV2) {
			d.Methods[1].Kind = "bidi_stream"
			d.Methods[1].Request = append(d.Methods[1].Request, fieldDef{Name: "frame", Type: "string"})
		}, "call_shape_changed"},
		{"request signature", func(d *contractDocV2) { d.Methods[1].Request = nil }, "method_signature_changed"},
		{"cursor mapping", func(d *contractDocV2) { d.Methods[1].CursorField = "event" }, "cursor_mapping_changed"},
		{"terminal semantics", func(d *contractDocV2) { d.Methods[1].TerminalEvent = "feed.closed" }, "terminal_semantics_changed"},
		{"field type", func(d *contractDocV2) { d.Methods[1].Response[1].Type = "int64" }, "field_type_changed"},
		{"requiredness", func(d *contractDocV2) { d.Methods[1].Response[1].Required = true }, "requiredness_changed"},
		{"method added", func(d *contractDocV2) {
			d.Methods = append(d.Methods, methodDefV2{Name: "Added", Path: "/added", Doc: "added", Kind: "unary"})
		}, "service_interface_widened"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldDoc, err := parseContractV2Source("base", base)
			if err != nil {
				t.Fatal(err)
			}
			nextDoc, err := parseContractV2Source("base", base)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(nextDoc)
			if tc.name == "identical" {
				nextDoc = oldDoc
			}
			err = checkContractV2Compatibility(oldDoc, nextDoc)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected incompatibility: %v", err)
				}
				return
			}
			var report *ContractCompatibilityError
			if !errors.As(err, &report) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("incompatibility = %v, want %q", err, tc.want)
			}
		})
	}
	if err := CheckContractCompatibilitySources("v1", mustRead(t, "testdata/contract.yaml"), "v2", base); err == nil || !strings.Contains(err.Error(), "contract_version_changed") {
		t.Fatalf("V1 to V2 implicit migration must be rejected: %v", err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}
