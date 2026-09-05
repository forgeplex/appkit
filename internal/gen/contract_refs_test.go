package gen

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/forgeplex/appkit/refs"
	yaml "go.yaml.in/yaml/v3"
)

func TestContractRefsTypePositions(t *testing.T) {
	for _, tc := range []struct {
		name, types, fields, direction string
	}{
		{"request", "", "[{name: refs, type: refs}]", "request"},
		{"response", "", "[{name: refs, type: refs}]", "response"},
		{"array", "", "[{name: refs, type: '[]refs'}]", "request"},
		{"nested_array", "", "[{name: refs, type: '[][]refs'}]", "request"},
		{"dto_request", "types: [{name: Entry, fields: [{name: refs, type: refs}]}]\n", "[{name: entry, type: Entry}]", "request"},
		{"dto_response", "types: [{name: Entry, fields: [{name: refs, type: '[]refs'}]}]\n", "[{name: entries, type: '[]Entry'}]", "response"},
		{"time_and_refs", "", "[{name: refs, type: refs}, {name: at, type: timestamp}]", "response"},
		{"unused_dto", "types: [{name: Entry, fields: [{name: refs, type: refs}]}]\n", "[{name: label, type: string}]", "request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := fmt.Sprintf("version: 1\npackage: fixture\nsystem: fixture\n%smethods:\n  - name: M\n    path: /m\n    doc: test\n    %s: %s\n", tc.types, tc.direction, tc.fields)
			files, err := RenderContractSource("refs.yaml", []byte(source))
			if err != nil {
				t.Fatal(err)
			}
			service := string(files["service.gen.go"])
			if !strings.Contains(service, "refs.Values") || strings.Count(service, `"github.com/forgeplex/appkit/refs"`) != 1 {
				t.Fatalf("refs type/import missing or duplicated:\n%s", service)
			}
			strict := tc.direction == "request" && tc.name != "unused_dto"
			if got := strings.Contains(string(files["server.gen.go"]), "decodeRefsRequest(r.Body, &req)"); got != strict {
				t.Fatalf("strict request decoding = %v, want %v", got, strict)
			}
			for _, name := range []string{"client.gen.go", "server.gen.go", "wrap.gen.go"} {
				if strings.Contains(string(files[name]), `"github.com/forgeplex/appkit/refs"`) {
					t.Fatalf("%s contains unused refs import", name)
				}
			}
		})
	}
	if err := genContract(t, "version: 1\npackage: fixture\nsystem: fixture\nmethods: [{name: M, path: /m, doc: test, request: [{name: refs, type: refs, required: true}]}]"); err == nil {
		t.Fatal("refs requiredness must remain a resource Schema concern")
	}
	if _, err := RenderEventsSource("events.yaml", []byte("version: 1\npackage: fixture\nevents: [{name: Posted, topic: order.posted, fields: [{name: refs, type: refs}]}]")); err == nil {
		t.Fatal("contract refs support accidentally widened the events generator")
	}
}

func TestContractRefsOpenAPI(t *testing.T) {
	files, err := RenderContract("testdata/refs_contract.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]map[string]any `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(files["openapi.yaml"], &doc); err != nil {
		t.Fatal(err)
	}
	for _, location := range []struct{ schema, field string }{
		{"ExchangeRequest", "refs"}, {"ExchangeReply", "refs"}, {"Entry", "refs"}, {"LoadReply", "refs"}, {"StoreRequest", "refs"},
	} {
		schema := doc.Components.Schemas[location.schema]["properties"].(map[string]any)[location.field].(map[string]any)
		if schema["type"] != "object" || schema["maxProperties"] != refs.MaxEntries {
			t.Fatalf("%+v has incorrect object schema: %+v", location, schema)
		}
		values := schema["additionalProperties"].(map[string]any)
		keys := schema["propertyNames"].(map[string]any)
		if values["type"] != "string" || values["minLength"] != 1 || values["maxLength"] != refs.MaxIDBytes || keys["maxLength"] != refs.MaxKeyBytes {
			t.Fatalf("%+v has incorrect value/key constraints: %+v", location, schema)
		}
	}
	array := doc.Components.Schemas["ExchangeRequest"]["properties"].(map[string]any)["batches"].(map[string]any)
	inner := array["items"].(map[string]any)
	if array["type"] != "array" || inner["type"] != "array" || inner["items"].(map[string]any)["type"] != "object" {
		t.Fatalf("refs nested array OpenAPI = %+v", array)
	}
}

func TestContractRefsCompatibility(t *testing.T) {
	base := []byte("version: 1\npackage: fixture\nsystem: fixture\nmethods: [{name: M, path: /m, doc: test, request: [{name: refs, type: refs}]}]")
	if err := CheckContractCompatibilitySources("base", base, "same", base); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []string{"string", "[]refs"} {
		candidate := []byte(strings.Replace(string(base), "type: refs", "type: '"+changed+"'", 1))
		if err := CheckContractCompatibilitySources("base", base, "changed", candidate); !errors.Is(err, ErrContractIncompatible) || !strings.Contains(err.Error(), "field_type_changed") {
			t.Fatalf("refs -> %s: %v", changed, err)
		}
	}
	without := []byte(strings.Replace(string(base), "name: refs, type: refs", "name: name, type: string", 1))
	withOptional := []byte(strings.Replace(string(without), "type: string}", "type: string}, {name: refs, type: refs}", 1))
	if err := CheckContractCompatibilitySources("old", without, "with_optional_refs", withOptional); err != nil {
		t.Fatalf("optional refs addition: %v", err)
	}
}

// TestContractRefsGeneratedRoundTrip compiles actual renderer outputs in a
// separate Go module and exercises the generated HTTP server, client and local
// wrapper. No checked-in generated fixture is hand edited or updated here.
func TestContractRefsGeneratedRoundTrip(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	mod = []byte(strings.Replace(string(mod), "module github.com/forgeplex/appkit", "module example.com/refsfixture", 1) + fmt.Sprintf("\nrequire github.com/forgeplex/appkit v0.0.0\nreplace github.com/forgeplex/appkit => %q\n", root))
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", mod)
	sum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	write("go.sum", sum)
	if err := Contract("testdata/refs_contract.yaml", dir); err != nil {
		t.Fatal(err)
	}
	tests, err := os.ReadFile("testdata/refs_roundtrip_test.go")
	if err != nil {
		t.Fatal(err)
	}
	write("refs_roundtrip_test.go", tests)
	// Request-only and response-only packages expose import mistakes that a
	// combined fixture can hide; include a timestamp and nested array too.
	for _, direction := range []string{"request", "response"} {
		source := fmt.Sprintf("version: 1\npackage: fixture\nsystem: fixture\nmethods:\n  - name: M\n    path: /m\n    doc: test\n    %s: [{name: refs, type: '[][]refs'}, {name: at, type: timestamp}]\n", direction)
		files, err := RenderContractSource("fixture.yaml", []byte(source))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(dir, direction), 0o700); err != nil {
			t.Fatal(err)
		}
		for name, content := range files {
			write(filepath.Join(direction, name), content)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-race", "-count=1", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOENV=off", "GOWORK=off", "GOFLAGS=-mod=readonly", "GOPROXY=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated refs module failed: %v\n%s", err, out)
	}
}

func TestContractWithoutRefsKeepsGeneratedImports(t *testing.T) {
	files, err := RenderContract("testdata/contract.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, content, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if imp, ok := node.(*ast.ImportSpec); ok && strings.Contains(imp.Path.Value, "/refs") {
				t.Errorf("%s imported refs without opting in", name)
			}
			return true
		})
	}
}
