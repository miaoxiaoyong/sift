package brain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/miaoxiaoyong/sift/internal/schema"
	"github.com/miaoxiaoyong/sift/internal/storage"
)

// claude-json-v1 outer-envelope adapter (brain.md §4.1). The envelope is open:
// unknown diagnostic fields are accepted but ignored; result_text and usage
// stay required with exact types. The inner touchpoint output is decoded
// closed elsewhere — open stops at the envelope.

// Usage is the usage object: unknown counters accepted, input/output tokens
// required, non-negative integers, never coerced from strings. Sign checks
// happen in ParseEnvelope so they map to usage_invalid with attribution.
type Usage struct {
	InputTokens  *int64 `json:"input_tokens" sift:"required"`
	OutputTokens *int64 `json:"output_tokens" sift:"required"`
}

type claudeEnvelope struct {
	schema.OpenEnvelopeType `json:"-"`

	ResultText *string `json:"result_text" sift:"required"`
	Usage      *Usage  `json:"usage" sift:"required"`
}

// EnvelopeError maps an envelope-level failure to a stable
// provider_error_code (brain.md §3).
type EnvelopeError struct {
	Code string // invalid_envelope | usage_missing | usage_invalid
	Err  error
}

func (e *EnvelopeError) Error() string { return fmt.Sprintf("brain: %s: %v", e.Code, e.Err) }
func (e *EnvelopeError) Unwrap() error { return e.Err }

// ParseEnvelope normalizes raw provider stdout into result_text + usage.
// Usage absence/invalidity is a provider error: never guessed, never billed,
// never treated as zero (brain.md §4.1).
func ParseEnvelope(raw []byte) (resultText []byte, inputTokens, outputTokens int64, err error) {
	if !utf8.Valid(raw) {
		return nil, 0, 0, &EnvelopeError{Code: storage.ProviderErrInvalidEnvelope, Err: errors.New("stdout is not valid UTF-8")}
	}
	if err := schema.RejectDuplicateKeys(raw); err != nil {
		return nil, 0, 0, &EnvelopeError{Code: storage.ProviderErrInvalidEnvelope, Err: err}
	}
	var env claudeEnvelope
	decErr := schema.Decode(raw, &env, schema.OpenEnvelope)
	if decErr == nil {
		if *env.Usage.InputTokens < 0 || *env.Usage.OutputTokens < 0 {
			return nil, 0, 0, &EnvelopeError{Code: storage.ProviderErrUsageInvalid, Err: errors.New("negative token count")}
		}
		return singleJSONObject(*env.ResultText, *env.Usage.InputTokens, *env.Usage.OutputTokens)
	}
	var de *schema.DecodeError
	if !errors.As(decErr, &de) {
		return nil, 0, 0, &EnvelopeError{Code: storage.ProviderErrInvalidEnvelope, Err: decErr}
	}
	switch {
	case de.Kind == schema.KindMissingRequired && de.Field == "usage":
		return nil, 0, 0, &EnvelopeError{Code: storage.ProviderErrUsageMissing, Err: decErr}
	case de.Kind == schema.KindMissingRequired && (de.Field == "usage.input_tokens" || de.Field == "usage.output_tokens"):
		return nil, 0, 0, &EnvelopeError{Code: storage.ProviderErrUsageMissing, Err: decErr}
	case de.Field == "usage" || len(de.Field) > 6 && de.Field[:6] == "usage.":
		return nil, 0, 0, &EnvelopeError{Code: storage.ProviderErrUsageInvalid, Err: decErr}
	default:
		return nil, 0, 0, &EnvelopeError{Code: storage.ProviderErrInvalidEnvelope, Err: decErr}
	}
}

// singleJSONObject enforces that result_text carries exactly one JSON object
// (brain.md §4.1): no fences, no arrays, no trailing text.
func singleJSONObject(text string, in, out int64) ([]byte, int64, int64, error) {
	raw := []byte(text)
	if !utf8.Valid(raw) {
		return nil, 0, 0, &EnvelopeError{Code: storage.ProviderErrInvalidEnvelope, Err: errors.New("result_text is not valid UTF-8")}
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, 0, 0, &EnvelopeError{Code: storage.ProviderErrInvalidEnvelope, Err: errors.New("result_text is not a single JSON object")}
	}
	var tree map[string]any
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if err := dec.Decode(&tree); err != nil {
		return nil, 0, 0, &EnvelopeError{Code: storage.ProviderErrInvalidEnvelope, Err: err}
	}
	if dec.More() {
		return nil, 0, 0, &EnvelopeError{Code: storage.ProviderErrInvalidEnvelope, Err: errors.New("result_text has trailing data")}
	}
	return trimmed, in, out, nil
}
