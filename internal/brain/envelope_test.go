package brain

import (
	"errors"
	"testing"

	"github.com/xsift/sift/internal/storage"
)

func parseErr(t *testing.T, raw string) *EnvelopeError {
	t.Helper()
	_, _, _, err := ParseEnvelope([]byte(raw))
	var ee *EnvelopeError
	if !errors.As(err, &ee) {
		t.Fatalf("want EnvelopeError, got %v", err)
	}
	return ee
}

func TestParseEnvelopeValid(t *testing.T) {
	// Unknown diagnostic fields at the top level and inside usage are
	// accepted for forward compatibility (brain.md §4.1).
	raw := `{"type":"result","session_id":"s","cost":{"usd":0.1},
		"result_text":"{\"disposition\":\"ready\"}",
		"usage":{"input_tokens":11,"output_tokens":7,"cache_read":99}}`
	text, in, out, err := ParseEnvelope([]byte(raw))
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if in != 11 || out != 7 {
		t.Fatalf("usage = %d/%d", in, out)
	}
	if string(text) != `{"disposition":"ready"}` {
		t.Fatalf("result_text = %q", text)
	}
}

func TestParseEnvelopeFailures(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		code string
	}{
		{name: "duplicate_key", raw: `{"result_text":"{}","result_text":"{}","usage":{"input_tokens":1,"output_tokens":1}}`, code: storage.ProviderErrInvalidEnvelope},
		{name: "trailing_text", raw: `{"result_text":"{}","usage":{"input_tokens":1,"output_tokens":1}} extra`, code: storage.ProviderErrInvalidEnvelope},
		{name: "top_array", raw: `[]`, code: storage.ProviderErrInvalidEnvelope},
		{name: "missing_result_text", raw: `{"usage":{"input_tokens":1,"output_tokens":1}}`, code: storage.ProviderErrInvalidEnvelope},
		{name: "missing_usage", raw: `{"result_text":"{}"}`, code: storage.ProviderErrUsageMissing},
		{name: "missing_usage_counter", raw: `{"result_text":"{}","usage":{"input_tokens":1}}`, code: storage.ProviderErrUsageMissing},
		{name: "usage_wrong_type", raw: `{"result_text":"{}","usage":{"input_tokens":"10","output_tokens":1}}`, code: storage.ProviderErrUsageInvalid},
		{name: "usage_negative", raw: `{"result_text":"{}","usage":{"input_tokens":-1,"output_tokens":1}}`, code: storage.ProviderErrUsageInvalid},
		{name: "usage_fractional", raw: `{"result_text":"{}","usage":{"input_tokens":1.5,"output_tokens":1}}`, code: storage.ProviderErrUsageInvalid},
		{name: "result_text_array", raw: `{"result_text":"[]","usage":{"input_tokens":1,"output_tokens":1}}`, code: storage.ProviderErrInvalidEnvelope},
		{name: "result_text_fenced", raw: `{"result_text":"` + "```json\n{}\n```" + `","usage":{"input_tokens":1,"output_tokens":1}}`, code: storage.ProviderErrInvalidEnvelope},
		{name: "result_text_trailing", raw: `{"result_text":"{} {}","usage":{"input_tokens":1,"output_tokens":1}}`, code: storage.ProviderErrInvalidEnvelope},
		{name: "result_text_scalar", raw: `{"result_text":"42","usage":{"input_tokens":1,"output_tokens":1}}`, code: storage.ProviderErrInvalidEnvelope},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ee := parseErr(t, tc.raw)
			if ee.Code != tc.code {
				t.Fatalf("code = %q, want %q (%v)", ee.Code, tc.code, ee)
			}
		})
	}
}

func TestParseEnvelopeNonUTF8(t *testing.T) {
	raw := []byte(`{"result_text":"{}","usage":{"input_tokens":1,"output_tokens":1}}`)
	raw = append(raw[:len(raw)-1], 0xff, 0xfe, '}')
	ee := parseErr(t, string(raw))
	if ee.Code != storage.ProviderErrInvalidEnvelope {
		t.Fatalf("code = %q", ee.Code)
	}
}
