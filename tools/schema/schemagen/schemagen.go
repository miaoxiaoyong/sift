// Package schemagen generates JSON Schema (Draft 2020-12) for decode gateway
// boundary types via reflection. The struct is the single source of truth
// (DESIGN §5.2): required, type, enum and the `sift` tag constraint vocabulary
// (specs/brain.md §7/§8 byte/item bounds, keyrequired) all derive from the Go
// type, so the schema cannot drift from the runtime contract without also
// changing the struct.
//
// Output is deterministic (sorted map keys via encoding/json, sorted required
// arrays, stable property order from struct declaration order) so that
// committed files are diffable. The CI drift check regenerates and fails on
// any diff.
package schemagen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/xsift/sift/internal/schema"
)

// TopLevelExtender lets a boundary type contribute extra top-level schema
// keywords (for example the T1 disposition mutex matrix, brain.md §7.2). The
// Go type remains the single source: the extension lives next to the struct
// and the runtime Validator re-checks the same rule.
type TopLevelExtender interface {
	ExtendJSONSchema() map[string]any
}

// Target pairs a schema document with its type and decode mode.
type Target struct {
	Name string
	Type reflect.Type
	Mode schema.Mode
}

// TargetFor builds a Target from a boundary type, reading the decode mode
// from its embedded schema.ClosedType / schema.OpenEnvelopeType marker.
// The schema document name is the snake_case type name.
func TargetFor(v any) (Target, error) {
	t := reflect.TypeOf(v)
	mode, err := ModeOfType(t)
	if err != nil {
		return Target{}, fmt.Errorf("%s: %w", t.Name(), err)
	}
	return Target{Name: ToSnake(t.Name()), Type: t, Mode: mode}, nil
}

// Generate renders the deterministic schema document for a Target.
func Generate(t Target) ([]byte, error) {
	doc := BuildSchema(t)
	return MarshalSchema(doc)
}

// ModeOfType reads the decode mode a boundary type declares by embedding
// schema.ClosedType or schema.OpenEnvelopeType.
func ModeOfType(t reflect.Type) (schema.Mode, error) {
	closed := reflect.TypeOf(schema.ClosedType{})
	open := reflect.TypeOf(schema.OpenEnvelopeType{})
	mode := schema.Mode(-1)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.Anonymous {
			continue
		}
		switch f.Type {
		case closed:
			if mode == schema.OpenEnvelope {
				return 0, fmt.Errorf("embeds both ClosedType and OpenEnvelopeType")
			}
			mode = schema.Closed
		case open:
			if mode == schema.Closed {
				return 0, fmt.Errorf("must embed either ClosedType or OpenEnvelopeType, not both")
			}
			mode = schema.OpenEnvelope
		}
	}
	if mode == schema.Mode(-1) {
		return 0, fmt.Errorf("must embed schema.ClosedType or schema.OpenEnvelopeType")
	}
	return mode, nil
}

// BuildSchema assembles the top-level schema document for a target.
func BuildSchema(t Target) map[string]any {
	doc := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$id":     fmt.Sprintf("https://sift.dev/decode/%s.schema.json", t.Name),
		"title":   t.Type.Name(),
		"x-sift": map[string]any{
			"decodeMode": modeString(t.Mode),
			"sourceType": t.Type.String(),
		},
	}
	obj, req := objectSchema(t.Type)
	doc["type"] = "object"
	doc["properties"] = obj
	if len(req) > 0 {
		doc["required"] = req
	}
	// Closed contracts forbid extra fields; open-envelope contracts allow them
	// for forward compatibility (DESIGN §5.2).
	if t.Mode == schema.Closed {
		doc["additionalProperties"] = false
	}
	if ext, ok := reflect.New(t.Type).Interface().(TopLevelExtender); ok {
		for k, v := range ext.ExtendJSONSchema() {
			doc[k] = v
		}
	}
	return doc
}

var nullStringType = reflect.TypeOf(schema.NullString{})

// objectSchema returns the "properties" object and the "required" array for a
// struct type, recursing into nested structs.
func objectSchema(t reflect.Type) (map[string]any, []string) {
	props := map[string]any{}
	var required []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, named := jsonName(f)
		if !named || name == "-" {
			continue
		}
		rules := schema.ParseFieldRules(f.Tag.Get("sift"))
		// Deref the field pointer once so nullability is decided here, not in
		// typeSchema: a required pointer is non-nullable (null is rejected),
		// an optional pointer is nullable. typeSchema keeps its own pointer
		// handling only for slice/map item contexts.
		ft := f.Type
		nullable := false
		if ft.Kind() == reflect.Pointer {
			nullable = !rules.Required()
			ft = ft.Elem()
		}
		props[name] = fieldSchema(ft, nullable, rules)
		if rules.Required() || rules.KeyRequired() {
			required = append(required, name)
		}
	}
	sort.Strings(required)
	return props, required
}

// fieldSchema emits the schema fragment for one struct field, applying the
// `sift` tag constraint vocabulary.
func fieldSchema(t reflect.Type, nullable bool, rules schema.FieldRules) map[string]any {
	if t == nullStringType {
		// Required-but-nullable string (brain.md §7.2): null is a legal value.
		s := map[string]any{"type": []string{"string", "null"}}
		applyBytes(s, rules.MinBytes(), rules.MaxBytes())
		return s
	}
	s := typeSchema(t, nullable)
	switch {
	case t.Kind() >= reflect.Int && t.Kind() <= reflect.Int64:
		if min, ok := rules.Min(); ok {
			s["minimum"] = min
		}
		if max, ok := rules.Max(); ok {
			s["maximum"] = max
		}
	case t.Kind() == reflect.String:
		applyBytes(s, rules.MinBytes(), rules.MaxBytes())
	case t.Kind() == reflect.Slice || t.Kind() == reflect.Array:
		if rules.MinItems() > 0 {
			s["minItems"] = rules.MinItems()
		}
		if rules.MaxItems() > 0 {
			s["maxItems"] = rules.MaxItems()
		}
		if (rules.ItemMinBytes() > 0 || rules.ItemMaxBytes() > 0) && t.Elem().Kind() == reflect.String {
			items := map[string]any{"type": "string"}
			applyBytes(items, rules.ItemMinBytes(), rules.ItemMaxBytes())
			s["items"] = items
		}
	}
	return s
}

// applyBytes emits byte-length bounds as x-sift keywords: JSON Schema
// maxLength counts characters, while the runtime contract is byte length
// (brain.md §7/§8), so the standard keyword would misstate the rule.
func applyBytes(s map[string]any, min, max int) {
	if min > 0 {
		s["x-sift:minBytes"] = min
	}
	if max > 0 {
		s["x-sift:maxBytes"] = max
	}
}

// typeSchema emits the schema fragment for a Go type.
func typeSchema(t reflect.Type, nullable bool) map[string]any {
	if t.Kind() == reflect.Pointer {
		// An inner pointer (e.g. *T inside a struct) is nullable regardless of
		// the caller's flag.
		return typeSchema(t.Elem(), true)
	}
	s := map[string]any{}
	switch t.Kind() {
	case reflect.String:
		s["type"] = "string"
		if allowed := enumValues(t); allowed != nil {
			s["enum"] = allowed
		}
	case reflect.Bool:
		s["type"] = "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		s["type"] = "integer"
	case reflect.Float32, reflect.Float64:
		s["type"] = "number"
	case reflect.Struct:
		if t.Name() == "" {
			s["type"] = "object"
			break
		}
		props, req := objectSchema(t)
		s["type"] = "object"
		s["properties"] = props
		if len(req) > 0 {
			s["required"] = req
		}
		// Nested owned contracts default to closed; an open-envelope inner
		// object would opt in explicitly when such a type exists.
		s["additionalProperties"] = false
	case reflect.Slice, reflect.Array:
		s["type"] = "array"
		s["items"] = typeSchema(t.Elem(), false)
	case reflect.Map:
		s["type"] = "object"
		s["additionalProperties"] = typeSchema(t.Elem(), false)
	default:
		// Unsupported kinds surface as a clearly-marked fragment so a diff
		// flags the gap rather than emitting a silently-wrong schema.
		s["description"] = "unsupported Go kind: " + t.Kind().String()
	}
	if nullable {
		s = withNullable(s)
	}
	return s
}

// withNullable widens a "type" keyword to admit null. Nullable enums are not
// exercised by the seed types; when one is needed it must extend the generator
// rather than emit a schema that silently rejects null.
func withNullable(s map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range s {
		out[k] = v
	}
	switch tv := out["type"].(type) {
	case string:
		out["type"] = []string{tv, "null"}
	case []string:
		already := false
		for _, t := range tv {
			if t == "null" {
				already = true
				break
			}
		}
		if !already {
			out["type"] = append(tv, "null")
		}
	}
	return out
}

// enumValues returns the allowed values for a named string type implementing
// schema.Enumerated, or nil if the type is not an enum.
func enumValues(t reflect.Type) []string {
	z := reflect.Zero(t).Interface()
	e, ok := z.(schema.Enumerated)
	if !ok {
		return nil
	}
	out := make([]string, len(e.EnumValues()))
	copy(out, e.EnumValues())
	return out
}

func jsonName(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name, true
	}
	name := strings.Split(tag, ",")[0]
	if name == "-" {
		return "-", false
	}
	if name == "" {
		return f.Name, true
	}
	return name, true
}

func modeString(m schema.Mode) string {
	switch m {
	case schema.Closed:
		return "closed"
	case schema.OpenEnvelope:
		return "open-envelope"
	default:
		return fmt.Sprintf("mode(%d)", int(m))
	}
}

// ToSnake converts CamelCase to snake_case. It keeps runs of uppercase
// acronyms together except for the final capital of a run followed by a
// lowercase letter (e.g. "OpenEnvelopeExample" -> "openenvelope_example",
// "HTTPRequest" -> "http_request").
func ToSnake(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		isUpper := c >= 'A' && c <= 'Z'
		if isUpper && i > 0 {
			prevLower := s[i-1] >= 'a' && s[i-1] <= 'z'
			nextLower := i+1 < len(s) && s[i+1] <= 'z'
			if prevLower || nextLower {
				b = append(b, '_')
			}
		}
		if isUpper {
			b = append(b, c+('a'-'A'))
		} else {
			b = append(b, c)
		}
	}
	return string(b)
}

// MarshalSchema renders deterministically with sorted keys and two-space
// indentation, followed by a trailing newline.
func MarshalSchema(doc map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
