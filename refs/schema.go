package refs

import (
	"sort"
	"strings"
)

// Format specifies the spelling of an ID, not its existence or ownership.
type Format string

const (
	FormatOpaque Format = "opaque"
	// FormatUUID accepts nonzero, lowercase, hyphenated UUID spellings. It does
	// not restrict the UUID version or variant.
	FormatUUID Format = "uuid"
	// MaxTargetBytes bounds a definition's namespaced target name.
	MaxTargetBytes = 128
)

// Definition declares one single-valued reference role. Target names the
// referenced domain/resource (for example merchant.account), not a Go type or
// a database table. Format must be explicit.
type Definition struct {
	Key       string `json:"key"`
	Target    string `json:"target"`
	Format    Format `json:"format"`
	Required  bool   `json:"required"`
	Immutable bool   `json:"immutable"`
}

// Spec is the versioned contract for a resource's references. Domain and
// Resource use lowercase ASCII letters, digits and underscores, starting with
// a letter. Version is nonzero. Empty Definitions permit only empty Values.
// Distribute the same spec to producers and consumers; there is no registry or
// automatic schema-version selection in Values.
type Spec struct {
	Domain      string       `json:"domain"`
	Resource    string       `json:"resource"`
	Version     uint32       `json:"version"`
	Definitions []Definition `json:"definitions"`
}

// Schema is an immutable, concurrency-safe structural validator. Construct it
// with NewSchema; its zero value is not configured. It performs no I/O and does
// not establish identity, tenant membership, authorization or referential
// integrity.
type Schema struct {
	spec        Spec
	definitions map[string]Definition
}

// NewSchema validates and copies spec. The stored definitions are sorted by key
// so validation and Spec output are deterministic. Names are bounded by
// MaxKeyBytes; namespaced targets by MaxTargetBytes; definitions by MaxEntries.
func NewSchema(spec Spec) (*Schema, error) {
	if !validName(spec.Domain) || !validName(spec.Resource) || spec.Version == 0 {
		return nil, schemaError("refs schema requires canonical domain/resource names and a nonzero version")
	}
	if len(spec.Definitions) > MaxEntries {
		return nil, schemaError("refs schema has too many definitions")
	}
	definitions := make(map[string]Definition, len(spec.Definitions))
	for _, def := range spec.Definitions {
		if !validReferenceKey(def.Key) || !validTarget(def.Target) {
			return nil, schemaError("refs definition requires a canonical key and namespaced target")
		}
		if def.Format != FormatOpaque && def.Format != FormatUUID {
			return nil, schemaError("refs definition requires an explicit supported ID format")
		}
		if _, exists := definitions[def.Key]; exists {
			return nil, schemaError("refs schema contains duplicate keys")
		}
		definitions[def.Key] = def
	}
	copySpec := spec
	copySpec.Definitions = make([]Definition, len(spec.Definitions))
	copy(copySpec.Definitions, spec.Definitions)
	sort.Slice(copySpec.Definitions, func(i, j int) bool {
		return copySpec.Definitions[i].Key < copySpec.Definitions[j].Key
	})
	return &Schema{spec: copySpec, definitions: definitions}, nil
}

// Spec returns a detached copy of the descriptor, with definitions sorted by
// key. For a nil or unconfigured Schema it returns a zero Spec.
func (s *Schema) Spec() Spec {
	if !s.configured() {
		return Spec{}
	}
	spec := s.spec
	spec.Definitions = make([]Definition, len(s.spec.Definitions))
	copy(spec.Definitions, s.spec.Definitions)
	return spec
}

// Validate checks a full resource snapshot: declared keys, ID formats and all
// required references. Unknown keys are rejected, not preserved as metadata.
func (s *Schema) Validate(values Values) error {
	return s.validate(values, true)
}

// ValidateFilter checks declared keys and ID formats in a partial equality
// filter. It does not require Required references; an empty filter is valid.
// Callers must separately enforce tenant scope, authorization, supported query
// plans and query cost. This method does not execute or compile queries.
func (s *Schema) ValidateFilter(values Values) error {
	return s.validate(values, false)
}

// DecodeJSON strictly decodes a refs object and validates it as a full snapshot.
// On any error it returns empty Values, not a partially validated value.
func (s *Schema) DecodeJSON(data []byte) (Values, error) {
	if !s.configured() {
		return Values{}, schemaError("refs schema is not configured")
	}
	values, err := DecodeJSON(data)
	if err != nil {
		return Values{}, err
	}
	if err := s.Validate(values); err != nil {
		return Values{}, err
	}
	return values, nil
}

// ValidateUpdate validates both complete snapshots, then rejects changing or
// removing an Immutable reference already present in before. Initially absent
// optional immutable refs may be assigned once. Read before from trusted
// persisted state, never from a caller-supplied claimed previous value. This is
// not a patch operation or a concurrency control: persist with a version check
// or equivalent atomic guard, and validate ownership through domain contracts.
func (s *Schema) ValidateUpdate(before, after Values) error {
	if err := s.Validate(before); err != nil {
		return err
	}
	if err := s.Validate(after); err != nil {
		return err
	}
	for _, def := range s.spec.Definitions {
		if !def.Immutable {
			continue
		}
		previous, exists := before.Get(def.Key)
		if !exists {
			continue
		}
		if next, present := after.Get(def.Key); !present || next != previous {
			return referenceError(CodeImmutableReference, "reference cannot be changed or removed", def.Key)
		}
	}
	return nil
}

func (s *Schema) configured() bool {
	return s != nil && s.spec.Version != 0 && s.definitions != nil
}

func (s *Schema) validate(values Values, requireAll bool) error {
	if !s.configured() {
		return schemaError("refs schema is not configured")
	}
	entries := values.Map()
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		def, declared := s.definitions[key]
		if !declared {
			return referenceError(CodeUnknownReference, "reference is not declared by the resource schema", key)
		}
		id := entries[key]
		if !validOpaqueID(id) || (def.Format == FormatUUID && !validUUID(id)) {
			return referenceError(CodeInvalidID, "reference ID does not match its declared format", key)
		}
	}
	if requireAll {
		for _, def := range s.spec.Definitions {
			if _, present := entries[def.Key]; def.Required && !present {
				return referenceError(CodeRequiredReference, "required reference is missing", def.Key)
			}
		}
	}
	return nil
}

func validName(name string) bool {
	return len(name) <= MaxKeyBytes && !strings.Contains(name, ".") && validReferenceKey(name)
}

func validTarget(target string) bool {
	if len(target) > MaxTargetBytes {
		return false
	}
	parts := strings.Split(target, ".")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if !validName(part) {
			return false
		}
	}
	return true
}

func validUUID(id string) bool {
	if len(id) != 36 {
		return false
	}
	nonzero := false
	for i := range len(id) {
		c := id[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
		nonzero = nonzero || c != '0'
	}
	return nonzero
}
