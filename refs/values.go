package refs

import (
	"encoding/json"
	"sort"
	"unicode"
	"unicode/utf8"
)

// Intrinsic limits bound the work and storage of a single reference collection.
// They are enforced independently of a resource's Schema.
const (
	MaxEntries   = 64
	MaxKeyBytes  = 64
	MaxIDBytes   = 256
	MaxJSONBytes = 128 * 1024
)

// Values is a flat collection of named, opaque reference IDs. Its zero value is
// an empty collection. Constructors and Map detach their maps, so values may be
// shared for reading without callers changing the stored references. UnmarshalJSON
// replaces the receiver and must not run concurrently with readers of that receiver.
// A reference is data, never proof of existence, ownership, or authorization.
type Values struct {
	entries map[string]string
}

// NewValues validates and copies input. Names are lowercase ASCII segments
// separated by dots, for example merchant_account_id or merchant.account_id.
// IDs are nonempty UTF-8 strings without Unicode spaces or control characters.
// No normalization, trimming, or business relationship resolution is performed.
func NewValues(input map[string]string) (Values, error) {
	if len(input) > MaxEntries {
		return Values{}, valueError("too many references")
	}
	if len(input) == 0 {
		return Values{}, nil
	}
	// A map's iteration order must not decide which validation error is returned.
	keys := make([]string, 0, len(input))
	for key := range input {
		// Check bounds before sorting so oversized keys cannot amplify comparisons.
		if !validReferenceKey(key) {
			return Values{}, valueError("invalid reference key")
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make(map[string]string, len(input))
	for _, key := range keys {
		if !validOpaqueID(input[key]) {
			return Values{}, valueError("invalid reference ID")
		}
		entries[key] = input[key]
	}
	return Values{entries: entries}, nil
}

// Get returns an ID and whether its key is present. An absent key is not a null
// or empty-string reference: those values are never accepted.
func (v Values) Get(key string) (string, bool) {
	id, ok := v.entries[key]
	return id, ok
}

// Len returns the number of references.
func (v Values) Len() int { return len(v.entries) }

// Map returns a detached, non-nil map, including for an empty Values.
func (v Values) Map() map[string]string {
	result := make(map[string]string, len(v.entries))
	for key, id := range v.entries {
		result[key] = id
	}
	return result
}

// MarshalJSON emits a deterministic JSON object with lexically sorted keys.
// Empty values encode as {}, never null.
func (v Values) MarshalJSON() ([]byte, error) {
	if len(v.entries) == 0 {
		return []byte("{}"), nil
	}
	return json.Marshal(v.entries)
}

// UnmarshalJSON strictly decodes a flat object, replacing the receiver only on
// success. It never merges existing references or treats null as an empty object.
func (v *Values) UnmarshalJSON(data []byte) error {
	if v == nil {
		return valueError("nil reference values receiver")
	}
	decoded, err := DecodeJSON(data)
	if err != nil {
		return err
	}
	*v = decoded
	return nil
}

// DecodeJSON accepts exactly one flat JSON object with string values. It rejects
// duplicate decoded keys, malformed UTF-8, unpaired escaped UTF-16 surrogates,
// null, nested structures, trailing input, and values exceeding the intrinsic
// limits. Errors never include reference IDs or excerpts from the input.
func DecodeJSON(data []byte) (Values, error) {
	if len(data) > MaxJSONBytes {
		return Values{}, valueError("reference JSON exceeds size limit")
	}
	if !utf8.Valid(data) {
		return Values{}, valueError("reference JSON must be valid UTF-8")
	}
	p := valuesJSONParser{data: data}
	if !p.take('{') {
		return Values{}, valueError("references must be a JSON object")
	}
	entries := make(map[string]string)
	if !p.take('}') {
		for {
			key, ok := p.stringValue()
			if !ok || !validReferenceKey(key) {
				return Values{}, valueError("invalid reference JSON key")
			}
			if _, duplicate := entries[key]; duplicate {
				return Values{}, valueError("duplicate reference JSON key")
			}
			if len(entries) >= MaxEntries {
				return Values{}, valueError("too many references")
			}
			if !p.take(':') {
				return Values{}, valueError("invalid reference JSON object")
			}
			id, ok := p.stringValue()
			if !ok || !validOpaqueID(id) {
				return Values{}, valueError("invalid reference JSON ID")
			}
			entries[key] = id
			if p.take('}') {
				break
			}
			if !p.take(',') {
				return Values{}, valueError("invalid reference JSON object")
			}
		}
	}
	p.skipSpace()
	if p.pos != len(data) {
		return Values{}, valueError("unexpected data after reference JSON object")
	}
	if len(entries) == 0 {
		return Values{}, nil
	}
	return Values{entries: entries}, nil
}

func validReferenceKey(key string) bool {
	if len(key) == 0 || len(key) > MaxKeyBytes {
		return false
	}
	segmentStart := true
	for i := 0; i < len(key); i++ {
		c := key[i]
		if segmentStart {
			if c < 'a' || c > 'z' {
				return false
			}
			segmentStart = false
			continue
		}
		if c == '.' {
			segmentStart = true
			continue
		}
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '_' {
			return false
		}
	}
	return !segmentStart
}

func validOpaqueID(id string) bool {
	if len(id) == 0 || len(id) > MaxIDBytes || !utf8.ValidString(id) {
		return false
	}
	for _, r := range id {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// A small flat-object parser is intentional: encoding/json otherwise accepts
// duplicate keys and silently replaces invalid Unicode with U+FFFD. Standard
// JSON string decoding is used only after strict lexical validation below.
type valuesJSONParser struct {
	data []byte
	pos  int
}

func (p *valuesJSONParser) skipSpace() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *valuesJSONParser) take(c byte) bool {
	p.skipSpace()
	if p.pos < len(p.data) && p.data[p.pos] == c {
		p.pos++
		return true
	}
	return false
}

func (p *valuesJSONParser) stringValue() (string, bool) {
	p.skipSpace()
	start := p.pos
	if !p.take('"') {
		return "", false
	}
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		p.pos++
		switch {
		case c == '"':
			var decoded string
			if err := json.Unmarshal(p.data[start:p.pos], &decoded); err != nil {
				return "", false
			}
			return decoded, true
		case c == '\\':
			if p.pos >= len(p.data) {
				return "", false
			}
			escaped := p.data[p.pos]
			p.pos++
			switch escaped {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				unit, ok := p.hexUnit()
				if !ok {
					return "", false
				}
				switch {
				case unit >= 0xD800 && unit <= 0xDBFF:
					if p.pos+2 > len(p.data) || p.data[p.pos] != '\\' || p.data[p.pos+1] != 'u' {
						return "", false
					}
					p.pos += 2
					low, ok := p.hexUnit()
					if !ok || low < 0xDC00 || low > 0xDFFF {
						return "", false
					}
				case unit >= 0xDC00 && unit <= 0xDFFF:
					return "", false
				}
			default:
				return "", false
			}
		case c < 0x20:
			return "", false
		}
	}
	return "", false
}

func (p *valuesJSONParser) hexUnit() (uint16, bool) {
	if p.pos+4 > len(p.data) {
		return 0, false
	}
	var unit uint16
	for range 4 {
		c := p.data[p.pos]
		p.pos++
		unit <<= 4
		switch {
		case c >= '0' && c <= '9':
			unit |= uint16(c - '0')
		case c >= 'a' && c <= 'f':
			unit |= uint16(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			unit |= uint16(c - 'A' + 10)
		default:
			return 0, false
		}
	}
	return unit, true
}
