package gen

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

var ErrContractIncompatible = errors.New("APPKIT_CONTRACT_INCOMPATIBLE")

type ContractCompatibilityIssue struct {
	Location string `json:"location"`
	Reason   string `json:"reason"`
}

type ContractCompatibilityError struct {
	Issues []ContractCompatibilityIssue `json:"issues"`
}

func (e *ContractCompatibilityError) Error() string {
	parts := make([]string, len(e.Issues))
	for i, issue := range e.Issues {
		parts[i] = issue.Location + ": " + issue.Reason
	}
	return ErrContractIncompatible.Error() + ": " + strings.Join(parts, "; ")
}
func (e *ContractCompatibilityError) Unwrap() error { return ErrContractIncompatible }

// CheckContractCompatibility checks AppKit's generated contract model, not
// arbitrary OpenAPI or implementation semantics. Pass means the conservative
// rules below found no incompatible changes; consumer compilation/runtime tests
// remain required (for example, unkeyed Go struct literals are not modeled).
func CheckContractCompatibility(base, candidate string) error {
	old, err := os.ReadFile(base)
	if err != nil {
		return err
	}
	next, err := os.ReadFile(candidate)
	if err != nil {
		return err
	}
	return CheckContractCompatibilitySources(base, old, candidate, next)
}

// CheckContractCompatibilitySources reads neither sourceName from disk.
// Existing methods, DTOs and fields must remain; method additions also fail
// because widening the shared Service interface breaks existing implementors.
// New DTOs and optional fields are permitted, except that adding the first
// request/response field changes a Go method signature and is rejected.
func CheckContractCompatibilitySources(baseName string, base []byte, candidateName string, candidate []byte) error {
	// Parsing alone is insufficient: a valid model can still collide with a
	// generated symbol or contain text the renderer cannot represent. Never
	// report compatibility for an input rejected by the shared generator.
	if _, err := RenderContractSource(baseName, base); err != nil {
		return err
	}
	if _, err := RenderContractSource(candidateName, candidate); err != nil {
		return err
	}
	baseVersion, err := contractSourceVersion(baseName, base)
	if err != nil {
		return err
	}
	candidateVersion, err := contractSourceVersion(candidateName, candidate)
	if err != nil {
		return err
	}
	if baseVersion == 2 && candidateVersion == 2 {
		old, err := parseContractV2Source(baseName, base)
		if err != nil {
			return err
		}
		next, err := parseContractV2Source(candidateName, candidate)
		if err != nil {
			return err
		}
		return checkContractV2Compatibility(old, next)
	}
	if baseVersion != 1 || candidateVersion != 1 {
		return &ContractCompatibilityError{Issues: []ContractCompatibilityIssue{{Location: "version", Reason: fmt.Sprintf("contract_version_changed:%d->%d", baseVersion, candidateVersion)}}}
	}
	old, err := parseContractSource(baseName, base)
	if err != nil {
		return err
	}
	next, err := parseContractSource(candidateName, candidate)
	if err != nil {
		return err
	}
	var issues []ContractCompatibilityIssue
	add := func(at, why string) { issues = append(issues, ContractCompatibilityIssue{at, why}) }
	if old.Package != next.Package {
		add("package", "package_changed")
	}
	if old.System != next.System {
		add("system", "system_changed")
	}
	methods := make(map[string]methodDef, len(next.Methods))
	for _, method := range next.Methods {
		methods[method.Name] = method
	}
	oldMethods := make(map[string]bool, len(old.Methods))
	for _, method := range old.Methods {
		oldMethods[method.Name] = true
		at := "methods." + method.Name
		other, ok := methods[method.Name]
		if !ok {
			add(at, "method_removed")
			continue
		}
		if method.Path != other.Path {
			add(at+".path", "path_changed")
		}
		if method.Idempotent != other.Idempotent {
			add(at+".idempotent", "retry_contract_changed")
		}
		if (len(method.Request) == 0) != (len(other.Request) == 0) {
			add(at+".request", "method_signature_changed")
		}
		if (len(method.Response) == 0) != (len(other.Response) == 0) {
			add(at+".response", "method_signature_changed")
		}
		compareFields(at+".request", method.Request, other.Request, add)
		compareFields(at+".response", method.Response, other.Response, add)
	}
	for _, method := range next.Methods {
		if !oldMethods[method.Name] {
			add("methods."+method.Name, "service_interface_widened")
		}
	}
	types := make(map[string]typeDef, len(next.Types))
	for _, typ := range next.Types {
		types[typ.Name] = typ
	}
	for _, typ := range old.Types {
		at := "types." + typ.Name
		other, ok := types[typ.Name]
		if !ok {
			add(at, "type_removed")
			continue
		}
		compareFields(at+".fields", typ.Fields, other.Fields, add)
	}
	if len(issues) == 0 {
		return nil
	}
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Location != issues[j].Location {
			return issues[i].Location < issues[j].Location
		}
		return issues[i].Reason < issues[j].Reason
	})
	return &ContractCompatibilityError{Issues: issues}
}

func checkContractV2Compatibility(old, next *contractDocV2) error {
	var issues []ContractCompatibilityIssue
	add := func(at, why string) {
		issues = append(issues, ContractCompatibilityIssue{Location: at, Reason: why})
	}
	if old.Package != next.Package {
		add("package", "package_changed")
	}
	if old.System != next.System {
		add("system", "system_changed")
	}
	methods := make(map[string]methodDefV2, len(next.Methods))
	for _, method := range next.Methods {
		methods[method.Name] = method
	}
	oldMethods := make(map[string]bool, len(old.Methods))
	for _, method := range old.Methods {
		oldMethods[method.Name] = true
		at := "methods." + method.Name
		other, ok := methods[method.Name]
		if !ok {
			add(at, "method_removed")
			continue
		}
		if method.Kind != other.Kind {
			add(at+".kind", "call_shape_changed:"+method.Kind+"->"+other.Kind)
		}
		if (len(method.Request) == 0) != (len(other.Request) == 0) {
			add(at+".request", "method_signature_changed")
		}
		if (len(method.Response) == 0) != (len(other.Response) == 0) {
			add(at+".response", "method_signature_changed")
		}
		if method.Path != other.Path {
			add(at+".path", "path_changed")
		}
		if method.Idempotent != other.Idempotent {
			add(at+".idempotent", "retry_contract_changed")
		}
		if method.CursorField != other.CursorField {
			add(at+".cursor_field", "cursor_mapping_changed")
		}
		if method.TerminalEvent != other.TerminalEvent {
			add(at+".terminal_event", "terminal_semantics_changed")
		}
		compareFields(at+".request", method.Request, other.Request, add)
		compareFields(at+".response", method.Response, other.Response, add)
	}
	for _, method := range next.Methods {
		if !oldMethods[method.Name] {
			add("methods."+method.Name, "service_interface_widened")
		}
	}
	types := make(map[string]typeDef, len(next.Types))
	for _, typ := range next.Types {
		types[typ.Name] = typ
	}
	for _, typ := range old.Types {
		at := "types." + typ.Name
		other, ok := types[typ.Name]
		if !ok {
			add(at, "type_removed")
			continue
		}
		compareFields(at+".fields", typ.Fields, other.Fields, add)
	}
	if len(issues) == 0 {
		return nil
	}
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Location != issues[j].Location {
			return issues[i].Location < issues[j].Location
		}
		return issues[i].Reason < issues[j].Reason
	})
	return &ContractCompatibilityError{Issues: issues}
}

func compareFields(at string, old, next []fieldDef, add func(string, string)) {
	fields := make(map[string]fieldDef, len(next))
	for _, field := range next {
		fields[field.Name] = field
	}
	oldFields := make(map[string]bool, len(old))
	for _, field := range old {
		oldFields[field.Name] = true
		other, ok := fields[field.Name]
		if !ok {
			add(at+"."+field.Name, "field_removed")
			continue
		}
		if field.Type != other.Type {
			add(at+"."+field.Name, fmt.Sprintf("field_type_changed:%s->%s", field.Type, other.Type))
		}
		if field.Required != other.Required {
			add(at+"."+field.Name, "requiredness_changed")
		}
	}
	for _, field := range next {
		if !oldFields[field.Name] && field.Required {
			add(at+"."+field.Name, "required_field_added")
		}
	}
}
