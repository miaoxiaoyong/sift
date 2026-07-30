// The decode gateway is the single entry point by which external JSON enters
// the Sift domain layer. It implements the two explicit strategies mandated by
// DESIGN §5.2:
//
//   - Closed rejects unknown fields outright. Used for config, LLM touchpoint
//     outputs and control-plane requests: contracts where Sift owns every
//     field and an extra field is a fail-closed signal.
//   - OpenEnvelope accepts unknown fields for forward compatibility, while
//     still enforcing required/type/enum rules on the fields the adapter
//     actually consumes. Used for Forge raw payloads and the LLM provider
//     outer envelope.
//
// The runtime contract is expressed entirely by Go struct types, which are the
// single source of truth:
//
//   - A required field is a pointer carrying the `sift:"required"` struct tag.
//     Pointers distinguish a missing field (nil) from a present zero value, so
//     the absence of required semantics is always detectable.
//   - An enum is a named string type implementing [Enumerated]; both the
//     runtime check and the generated JSON Schema derive the allowed values
//     from the same method.
//
// The companion JSON Schema artifacts are generated from these struct types
// (see internal/schema/genschema) and checked into git. A CI drift check
// regenerates them and fails on any diff, so the schema and the struct cannot
// silently diverge.
//
// Business code MUST NOT open a second JSON/YAML decoding path. Every config
// file, LLM touchpoint output, control-plane frame and Forge payload flows
// through [Decode]. config.md §1.2 and control-plane.md §1.4 make this a
// hard contract, not a convention.
package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// Mode selects how the gateway treats unknown JSON fields.
type Mode int

const (
	// Closed rejects unknown fields. Equivalent semantics to
	// json.Decoder.DisallowUnknownFields, plus required-field and enum checks.
	Closed Mode = iota
	// OpenEnvelope tolerates unknown fields for forward compatibility while
	// still enforcing required/type/enum rules on consumed fields.
	OpenEnvelope
)

// Decode decodes exactly one JSON value from data into v (a non-nil pointer)
// using mode m, then runs the generic required-field and enum validation.
//
// Trailing data after the first JSON value is rejected in both modes: a frame
// carries a single value, and tolerating garbage would hide framing bugs.
//
// Validation errors are returned as [*DecodeError]. The concrete Kind lets
// callers map to domain error codes (e.g. control-plane invalid_request)
// without parsing message text.
func Decode(data []byte, v any, m Mode) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return &DecodeError{Kind: KindInternal, Err: errNonNilPointer}
	}
	if err := strictDecode(data, v, m == Closed); err != nil {
		return classify(err)
	}
	if err := validate(rv.Elem()); err != nil {
		return err
	}
	if vv, ok := v.(Validator); ok {
		if err := vv.Validate(); err != nil {
			return &DecodeError{Kind: KindInvalidValue, Err: err}
		}
	}
	return nil
}

// strictDecode reads exactly one JSON value. disallowUnknown toggles the
// closed-vs-open-envelope field policy at the encoding/json layer.
func strictDecode(data []byte, v any, disallowUnknown bool) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if disallowUnknown {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errTrailingData
	}
	return nil
}

// classify maps encoding/json errors to a [*DecodeError] Kind.
// DisallowUnknownFields returns an untyped error, so the unknown-field case is
// detected by message prefix; this is the standard Go idiom.
func classify(err error) *DecodeError {
	if err == nil {
		return nil
	}
	var ute *json.UnmarshalTypeError
	if errors.As(err, &ute) {
		return &DecodeError{Kind: KindInvalidType, Field: ute.Field, Err: err}
	}
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return &DecodeError{Kind: KindInvalidJSON, Err: err}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return &DecodeError{Kind: KindInvalidJSON, Err: err}
	}
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "json: unknown field"):
		return &DecodeError{Kind: KindUnknownField, Err: err}
	case errors.Is(err, errTrailingData):
		return &DecodeError{Kind: KindTrailingData, Err: err}
	default:
		return &DecodeError{Kind: KindInvalidJSON, Err: err}
	}
}

var (
	errTrailingData  = errors.New("decode: trailing data after JSON value")
	errNonNilPointer = errors.New("decode: target must be a non-nil pointer")
)

// Validator is an optional hook for cross-field domain rules that the generic
// required/enum check cannot express. If the decoded value implements Validator,
// Validate runs after the generic checks. Required-field presence and enum
// membership should still be expressed via struct tags and [Enumerated] rather
// than reimplemented here.
type Validator interface {
	Validate() error
}

// Enumerated is implemented by named string types that form a closed set of
// allowed values (an enum). EnumValues is the single source for both the
// runtime membership check and the generated JSON Schema "enum" keyword.
type Enumerated interface {
	EnumValues() []string
}

// Kind classifies a decode/validation failure.
type Kind int

const (
	// KindUnknownField: a JSON field absent from the target struct. Closed
	// mode only.
	KindUnknownField Kind = iota
	// KindMissingRequired: a field tagged `sift:"required"` was absent or null.
	KindMissingRequired
	// KindInvalidType: a JSON value did not match the Go field type.
	KindInvalidType
	// KindInvalidEnum: a value was not in its type's allowed set.
	KindInvalidEnum
	// KindInvalidJSON: malformed JSON (syntax error, EOF, non-finite number).
	KindInvalidJSON
	// KindTrailingData: bytes followed the single JSON value.
	KindTrailingData
	// KindInvalidValue: a Validator hook rejected the decoded value.
	KindInvalidValue
	// KindDuplicateKey: an object repeated a key (RejectDuplicateKeys).
	KindDuplicateKey
	// KindInternal: programmer error (e.g. non-pointer target). Indicates a
	// caller bug, not invalid input.
	KindInternal
)

// String returns a stable, machine-readable kind name.
func (k Kind) String() string {
	switch k {
	case KindUnknownField:
		return "unknown_field"
	case KindMissingRequired:
		return "missing_required"
	case KindInvalidType:
		return "invalid_type"
	case KindInvalidEnum:
		return "invalid_enum"
	case KindInvalidJSON:
		return "invalid_json"
	case KindTrailingData:
		return "trailing_data"
	case KindInvalidValue:
		return "invalid_value"
	case KindDuplicateKey:
		return "duplicate_key"
	case KindInternal:
		return "internal"
	default:
		return fmt.Sprintf("kind(%d)", int(k))
	}
}

// DecodeError is the validation failure type returned by Decode. Callers
// should type-assert and switch on Kind rather than matching message text.
type DecodeError struct {
	Kind  Kind
	Field string // JSON path of the offending field, when known.
	Err   error
}

func (e *DecodeError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("decode: %s: %s: %v", e.Kind, e.Field, e.Err)
	}
	return fmt.Sprintf("decode: %s: %v", e.Kind, e.Err)
}

func (e *DecodeError) Unwrap() error { return e.Err }
