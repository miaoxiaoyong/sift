package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
)

// This file extends the gateway with the field-level constraint vocabulary
// needed by the Brain touchpoint contracts (specs/brain.md §7/§8): byte-length
// bounds for strings, item-count bounds for slices, item byte-length bounds
// for string slices, and the keyrequired marker for required-but-nullable
// keys. All constraints derive from the same `sift` struct tag that the
// schema generator reads, so the runtime check and the generated JSON Schema
// cannot diverge.

// fieldRules is the parsed form of a field's `sift` tag.
type fieldRules struct {
	required     bool // key present and non-null
	keyRequired  bool // key present; JSON null is an allowed value
	minBytes     int  // string byte-length lower bound (0 = unset)
	maxBytes     int  // string byte-length upper bound (0 = unset)
	minItems     int  // slice item-count lower bound (0 = unset)
	maxItems     int  // slice item-count upper bound (0 = unset)
	itemMinBytes int  // string-slice item byte-length lower bound (0 = unset)
	itemMaxBytes int  // string-slice item byte-length upper bound (0 = unset)
	min          *int // integer lower bound
	max          *int // integer upper bound
}

func parseRules(tag string) fieldRules {
	var r fieldRules
	for _, opt := range strings.Split(tag, ",") {
		opt = strings.TrimSpace(opt)
		switch {
		case opt == "required":
			r.required = true
		case opt == "keyrequired":
			r.keyRequired = true
		default:
			if k, v, ok := strings.Cut(opt, "="); ok {
				n, err := strconv.Atoi(v)
				if err != nil {
					continue
				}
				switch k {
				case "minbytes":
					r.minBytes = n
				case "maxbytes":
					r.maxBytes = n
				case "minitems":
					r.minItems = n
				case "maxitems":
					r.maxItems = n
				case "itemminbytes":
					r.itemMinBytes = n
				case "itemmaxbytes":
					r.itemMaxBytes = n
				case "min":
					r.min = &n
				case "max":
					r.max = &n
				}
			}
		}
	}
	return r
}

// FieldRules exposes the parsed tag vocabulary to the schema generator so it
// emits the same bounds the runtime enforces.
type FieldRules = fieldRules

// ParseFieldRules parses a `sift` struct tag into [FieldRules].
func ParseFieldRules(tag string) FieldRules { return parseRules(tag) }

// Accessors used by the schema generator; runtime validation reads the fields
// directly inside this package.
func (r fieldRules) Required() bool    { return r.required }
func (r fieldRules) KeyRequired() bool { return r.keyRequired }
func (r fieldRules) MinBytes() int     { return r.minBytes }
func (r fieldRules) MaxBytes() int     { return r.maxBytes }
func (r fieldRules) MinItems() int     { return r.minItems }
func (r fieldRules) MaxItems() int     { return r.maxItems }
func (r fieldRules) ItemMinBytes() int { return r.itemMinBytes }
func (r fieldRules) ItemMaxBytes() int { return r.itemMaxBytes }
func (r fieldRules) Min() (int, bool) {
	if r.min == nil {
		return 0, false
	}
	return *r.min, true
}
func (r fieldRules) Max() (int, bool) {
	if r.max == nil {
		return 0, false
	}
	return *r.max, true
}

// Keyed is implemented by field types that can distinguish an absent JSON key
// from a present key carrying JSON null. Fields tagged `keyrequired` must use
// such a type: the runtime checks presence through this interface because
// encoding/json alone cannot separate the two cases for ordinary pointers.
type Keyed interface {
	KeyPresent() bool
	IsNull() bool
}

// NullString is a required-but-nullable string boundary field. Present
// records whether the key appeared in the input; Null records a JSON null
// value. It implements json.Unmarshaler so `{"k":null}` marks both Present
// and Null, while an absent key leaves Present false.
type NullString struct {
	Present bool   `json:"-"`
	Null    bool   `json:"-"`
	Value   string `json:"-"`
}

func (n NullString) KeyPresent() bool { return n.Present }
func (n NullString) IsNull() bool     { return n.Null }

// UnmarshalJSON implements json.Unmarshaler. encoding/json calls it for a
// present key even when the value is the null literal, which is exactly the
// presence signal [Keyed] needs.
func (n *NullString) UnmarshalJSON(b []byte) error {
	n.Present = true
	if string(b) == "null" {
		n.Null = true
		n.Value = ""
		return nil
	}
	n.Null = false
	return json.Unmarshal(b, &n.Value)
}

// MarshalJSON emits the string value or the null literal.
func (n NullString) MarshalJSON() ([]byte, error) {
	if !n.Present || n.Null {
		return []byte("null"), nil
	}
	return json.Marshal(n.Value)
}

// checkFieldConstraints enforces the numeric, byte and item bounds declared
// in the `sift` tag. It runs after required/enum checks; fv is the raw struct
// field value.
func checkFieldConstraints(fv reflect.Value, path string, r fieldRules) error {
	if r.keyRequired {
		k, ok := fv.Interface().(Keyed)
		if !ok {
			return &DecodeError{Kind: KindInternal, Field: path, Err: fmt.Errorf("keyrequired field must implement schema.Keyed")}
		}
		if !k.KeyPresent() {
			return &DecodeError{Kind: KindMissingRequired, Field: path, Err: errMissingRequired}
		}
	}
	v := fv
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	// keyrequired nullable payloads: validate the carried value when non-null.
	if k, ok := v.Interface().(Keyed); ok {
		if k.IsNull() {
			return nil
		}
		if ns, ok := v.Interface().(NullString); ok {
			return checkBytes(path, ns.Value, r)
		}
	}
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if min, ok := r.Min(); ok && v.Int() < int64(min) {
			return &DecodeError{Kind: KindInvalidValue, Field: path, Err: fmt.Errorf("requires at least %d, got %d", min, v.Int())}
		}
		if max, ok := r.Max(); ok && v.Int() > int64(max) {
			return &DecodeError{Kind: KindInvalidValue, Field: path, Err: fmt.Errorf("requires at most %d, got %d", max, v.Int())}
		}
	case reflect.String:
		if err := checkBytes(path, v.String(), r); err != nil {
			return err
		}
	case reflect.Slice, reflect.Array:
		n := v.Len()
		if r.minItems > 0 && n < r.minItems {
			return &DecodeError{Kind: KindInvalidValue, Field: path, Err: fmt.Errorf("requires at least %d items, got %d", r.minItems, n)}
		}
		if r.maxItems > 0 && n > r.maxItems {
			return &DecodeError{Kind: KindInvalidValue, Field: path, Err: fmt.Errorf("requires at most %d items, got %d", r.maxItems, n)}
		}
		if r.itemMinBytes > 0 || r.itemMaxBytes > 0 {
			for i := 0; i < n; i++ {
				item := v.Index(i)
				for item.Kind() == reflect.Pointer {
					if item.IsNil() {
						break
					}
					item = item.Elem()
				}
				if item.Kind() != reflect.String {
					continue
				}
				if err := checkBytes(fmt.Sprintf("%s[%d]", path, i), item.String(), fieldRules{minBytes: r.itemMinBytes, maxBytes: r.itemMaxBytes}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func checkBytes(path, s string, r fieldRules) error {
	if r.minBytes > 0 && len(s) < r.minBytes {
		return &DecodeError{Kind: KindInvalidValue, Field: path, Err: fmt.Errorf("requires at least %d bytes, got %d", r.minBytes, len(s))}
	}
	if r.maxBytes > 0 && len(s) > r.maxBytes {
		return &DecodeError{Kind: KindInvalidValue, Field: path, Err: fmt.Errorf("requires at most %d bytes, got %d", r.maxBytes, len(s))}
	}
	return nil
}

// RejectDuplicateKeys scans a JSON document and fails on any object that
// repeats a key. encoding/json silently keeps the last occurrence, which the
// Brain provider envelope protocol forbids (brain.md §4.1).
func RejectDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	type frame struct {
		isObject     bool
		expectingKey bool
		keys         map[string]bool
	}
	var stack []frame
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return classify(err)
		}
		top := len(stack) - 1
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, frame{isObject: true, expectingKey: true, keys: map[string]bool{}})
			case '[':
				stack = append(stack, frame{})
			case '}', ']':
				stack = stack[:top]
				if top-1 >= 0 && stack[top-1].isObject {
					stack[top-1].expectingKey = true
				}
			}
		case string:
			if top >= 0 && stack[top].isObject && stack[top].expectingKey {
				if stack[top].keys[t] {
					return &DecodeError{Kind: KindDuplicateKey, Field: t, Err: fmt.Errorf("duplicate object key %q", t)}
				}
				stack[top].keys[t] = true
				stack[top].expectingKey = false
			} else if top >= 0 && stack[top].isObject {
				stack[top].expectingKey = true
			}
		default:
			if top >= 0 && stack[top].isObject {
				stack[top].expectingKey = true
			}
		}
	}
}

// Canonical serializes v into the canonical JSON form of config.md §4 step 6:
// UTF-8, object keys in dictionary order, no extraneous whitespace, no
// NaN/Infinity. It is the single canonical-JSON implementation for all
// non-config payloads (Brain inputs/outputs, Task Spec snapshots).
func Canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("decode: marshal for canonical JSON: %w", err)
	}
	var tree any
	jd := json.NewDecoder(bytes.NewReader(raw))
	if err := jd.Decode(&tree); err != nil {
		return nil, fmt.Errorf("decode: re-decode for canonical ordering: %w", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(tree); err != nil {
		return nil, fmt.Errorf("decode: encode canonical JSON: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
