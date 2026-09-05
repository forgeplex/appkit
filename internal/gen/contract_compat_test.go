package gen

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

func TestContractCompatibilityRules(t *testing.T) {
	base, err := os.ReadFile("testdata/contract.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*contractDoc)
		reason string
	}{
		{"identical", func(d *contractDoc) {}, ""},
		{"docs", func(d *contractDoc) { d.Methods[0].Doc = "new documentation" }, ""},
		{"optional_field", func(d *contractDoc) {
			d.Methods[0].Request = append(d.Methods[0].Request, fieldDef{Name: "locale", Type: "string"})
		}, ""},
		{"new_type", func(d *contractDoc) {
			d.Types = append(d.Types, typeDef{Name: "Extra", Fields: []fieldDef{{Name: "label", Type: "string"}}})
		}, ""},
		{"package", func(d *contractDoc) { d.Package = "other" }, "package_changed"},
		{"system", func(d *contractDoc) { d.System = "other" }, "system_changed"},
		{"path", func(d *contractDoc) { d.Methods[0].Path = "/renamed" }, "path_changed"},
		{"retry", func(d *contractDoc) { d.Methods[0].Idempotent = !d.Methods[0].Idempotent }, "retry_contract_changed"},
		{"method_removed", func(d *contractDoc) { d.Methods = d.Methods[1:] }, "method_removed"},
		{"method_added", func(d *contractDoc) {
			d.Methods = append(d.Methods, methodDef{Name: "Added", Path: "/added", Doc: "added"})
		}, "service_interface_widened"},
		{"signature", func(d *contractDoc) { d.Methods[0].Request = nil }, "method_signature_changed"},
		{"first_optional_request", func(d *contractDoc) { d.Methods[3].Request = []fieldDef{{Name: "note", Type: "string"}} }, "method_signature_changed"},
		{"first_optional_response", func(d *contractDoc) { d.Methods[3].Response = []fieldDef{{Name: "note", Type: "string"}} }, "method_signature_changed"},
		{"type_changed", func(d *contractDoc) { d.Methods[0].Request[0].Type = "int64"; d.Methods[0].Request[0].Required = false }, "field_type_changed"},
		{"requiredness", func(d *contractDoc) { d.Methods[0].Request[0].Required = false }, "requiredness_changed"},
		{"required_added", func(d *contractDoc) {
			d.Methods[0].Request = append(d.Methods[0].Request, fieldDef{Name: "locale", Type: "string", Required: true})
		}, "required_field_added"},
		{"dto_field_removed", func(d *contractDoc) { d.Types[0].Fields = d.Types[0].Fields[1:] }, "field_removed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := parseContractSource("base", base)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(doc)
			next, err := yaml.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			err = CheckContractCompatibilitySources("base", base, "candidate", next)
			if tc.reason == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrContractIncompatible) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("want %s, got %v", tc.reason, err)
			}
			var report *ContractCompatibilityError
			if !errors.As(err, &report) {
				t.Fatalf("not structured: %v", err)
			}
			again := CheckContractCompatibilitySources("base", base, "candidate", next)
			if !reflect.DeepEqual(err, again) {
				t.Fatal("unstable report")
			}
		})
	}
}

func TestContractCompatibilityRemovedUnusedDTOAndInvalidInput(t *testing.T) {
	base := []byte("version: 1\npackage: fixture\nsystem: fixture\ntypes:\n  - name: Marker\n    fields:\n      - {name: label, type: string}\nmethods:\n  - {name: Ping, path: /ping, doc: ping}\n")
	next := []byte("version: 1\npackage: fixture\nsystem: fixture\nmethods:\n  - {name: Ping, path: /ping, doc: ping}\n")
	if err := CheckContractCompatibilitySources("old", base, "new", next); !errors.Is(err, ErrContractIncompatible) || !strings.Contains(err.Error(), "type_removed") {
		t.Fatalf("removed DTO: %v", err)
	}
	for _, input := range [][]byte{nil, []byte("version: 9"), []byte("[broken")} {
		if err := CheckContractCompatibilitySources("old", base, "new", input); err == nil || errors.Is(err, ErrContractIncompatible) {
			t.Fatalf("parse failure must not be compatibility result: %v", err)
		}
	}
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.yaml")
	newPath := filepath.Join(dir, "new.yaml")
	if err := os.WriteFile(oldPath, base, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, base, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckContractCompatibility(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
}

func TestContractCompatibilityRejectsUngeneratableModel(t *testing.T) {
	base, err := os.ReadFile("testdata/contract.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parseContractSource("base", base)
	if err != nil {
		t.Fatal(err)
	}
	doc.Types = append(doc.Types, typeDef{Name: "Service", Fields: []fieldDef{{Name: "label", Type: "string"}}})
	candidate, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RenderContractSource("candidate", candidate); err == nil {
		t.Fatal("fixture must be rejected by generator")
	}
	if err := CheckContractCompatibilitySources("base", base, "candidate", candidate); err == nil || errors.Is(err, ErrContractIncompatible) {
		t.Fatalf("invalid generator input must not be a compatibility result: %v", err)
	}
	if err := CheckContractCompatibilitySources("base", candidate, "candidate", base); err == nil {
		t.Fatal("invalid base accepted")
	}
}
