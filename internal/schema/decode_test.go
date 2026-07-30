package schema

import (
	"errors"
	"fmt"
	"testing"
)

// localClosed exercises the gateway with package-private types, independent of
// the contract package's V14 suite. Required fields are pointers tagged
// `sift:"required"`; Mode is a closed-set enum.
type localClosed struct {
	Name *string    `json:"name" sift:"required"`
	Mode *localMode `json:"mode,omitempty"`
	Note *string    `json:"note,omitempty"`
}

type localMode string

const (
	localModeA localMode = "a"
	localModeB localMode = "b"
)

func (localMode) EnumValues() []string { return []string{"a", "b"} }

// localValidated demonstrates the optional Validator hook.
type localValidated struct {
	Min *int `json:"min" sift:"required"`
	Max *int `json:"max" sift:"required"`
}

func (l localValidated) Validate() error {
	if *l.Min > *l.Max {
		return fmt.Errorf("min %d > max %d", *l.Min, *l.Max)
	}
	return nil
}

func TestClosedAcceptsValid(t *testing.T) {
	var v localClosed
	if err := Decode([]byte(`{"name":"x"}`), &v, Closed); err != nil {
		t.Fatalf("expected accept, got %v", err)
	}
	if v.Name == nil || *v.Name != "x" {
		t.Fatalf("name = %v", v.Name)
	}
}

func TestClosedRejectsUnknownField(t *testing.T) {
	var v localClosed
	if k := kind(t, Decode([]byte(`{"name":"x","bogus":1}`), &v, Closed)); k != KindUnknownField {
		t.Fatalf("expected unknown_field, got %s", k)
	}
}

func TestOpenEnvelopeAcceptsUnknownField(t *testing.T) {
	var v localClosed
	if err := Decode([]byte(`{"name":"x","bogus":1}`), &v, OpenEnvelope); err != nil {
		t.Fatalf("open-envelope must tolerate unknown fields, got %v", err)
	}
}

func TestMissingRequired(t *testing.T) {
	var v localClosed
	if k := kind(t, Decode([]byte(`{}`), &v, Closed)); k != KindMissingRequired {
		t.Fatalf("expected missing_required, got %s", k)
	}
}

func TestInvalidType(t *testing.T) {
	var v localClosed
	if k := kind(t, Decode([]byte(`{"name":123}`), &v, Closed)); k != KindInvalidType {
		t.Fatalf("expected invalid_type, got %s", k)
	}
}

func TestInvalidEnum(t *testing.T) {
	var v localClosed
	if k := kind(t, Decode([]byte(`{"name":"x","mode":"z"}`), &v, Closed)); k != KindInvalidEnum {
		t.Fatalf("expected invalid_enum, got %s", k)
	}
}

func TestTrailingDataRejected(t *testing.T) {
	var v localClosed
	if k := kind(t, Decode([]byte(`{"name":"x"}{"name":"y"}`), &v, Closed)); k != KindTrailingData {
		t.Fatalf("expected trailing_data, got %s", k)
	}
}

func TestTrailingDataRejectedOpenEnvelope(t *testing.T) {
	var v localClosed
	if err := Decode([]byte(`{"name":"x"}garbage`), &v, OpenEnvelope); err == nil {
		t.Fatal("open-envelope must reject trailing data")
	}
}

func TestInvalidJSON(t *testing.T) {
	var v localClosed
	if k := kind(t, Decode([]byte(`{not json`), &v, Closed)); k != KindInvalidJSON {
		t.Fatalf("expected invalid_json, got %s", k)
	}
}

func TestNonPointerTarget(t *testing.T) {
	var v localClosed
	if err := Decode([]byte(`{"name":"x"}`), v, Closed); err == nil {
		t.Fatal("expected error for non-pointer target")
	}
	if err := Decode([]byte(`{"name":"x"}`), nil, Closed); err == nil {
		t.Fatal("expected error for nil target")
	}
}

func TestValidatorHookReject(t *testing.T) {
	var v localValidated
	// Both required present and well-typed, but the cross-field rule fails.
	if k := kind(t, Decode([]byte(`{"min":5,"max":1}`), &v, Closed)); k != KindInvalidValue {
		t.Fatalf("expected invalid_value from Validator, got %s", k)
	}
}

func TestValidatorHookPass(t *testing.T) {
	var v localValidated
	if err := Decode([]byte(`{"min":1,"max":5}`), &v, Closed); err != nil {
		t.Fatalf("expected accept, got %v", err)
	}
}

func TestKindString(t *testing.T) {
	for _, k := range []Kind{
		KindUnknownField, KindMissingRequired, KindInvalidType, KindInvalidEnum,
		KindInvalidJSON, KindTrailingData, KindInvalidValue, KindInternal,
	} {
		if k.String() == "" {
			t.Fatalf("kind %d has empty string", int(k))
		}
	}
}

// kind extracts the Kind from a *DecodeError, failing the test if the error is
// not classified as one.
func kind(t *testing.T, err error) Kind {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var de *DecodeError
	if !errors.As(err, &de) {
		t.Fatalf("expected *DecodeError, got %T: %v", err, err)
	}
	return de.Kind
}
