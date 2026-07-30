package schema

import (
	"errors"
	"testing"
)

type constrained struct {
	ClosedType `json:"-"`

	Name  *string    `json:"name" sift:"required,minbytes=2,maxbytes=4"`
	Tags  *[]string  `json:"tags" sift:"required,minitems=1,maxitems=2,itemminbytes=1,itemmaxbytes=3"`
	Maybe NullString `json:"maybe" sift:"keyrequired,maxbytes=3"`
}

func TestFieldConstraints(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		wantErr Kind
	}{
		{name: "valid", json: `{"name":"ab","tags":["x"],"maybe":null}`, wantErr: noErrKind},
		{name: "valid_value", json: `{"name":"abcd","tags":["x","yz"],"maybe":"abc"}`, wantErr: noErrKind},
		{name: "name_too_short", json: `{"name":"a","tags":["x"],"maybe":null}`, wantErr: KindInvalidValue},
		{name: "name_too_long", json: `{"name":"abcde","tags":["x"],"maybe":null}`, wantErr: KindInvalidValue},
		{name: "tags_empty", json: `{"name":"ab","tags":[],"maybe":null}`, wantErr: KindInvalidValue},
		{name: "tags_too_many", json: `{"name":"ab","tags":["a","b","c"],"maybe":null}`, wantErr: KindInvalidValue},
		{name: "tag_item_too_long", json: `{"name":"ab","tags":["abcd"],"maybe":null}`, wantErr: KindInvalidValue},
		{name: "tag_item_empty", json: `{"name":"ab","tags":[""],"maybe":null}`, wantErr: KindInvalidValue},
		{name: "maybe_key_absent", json: `{"name":"ab","tags":["x"]}`, wantErr: KindMissingRequired},
		{name: "maybe_too_long", json: `{"name":"ab","tags":["x"],"maybe":"abcd"}`, wantErr: KindInvalidValue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var v constrained
			err := Decode([]byte(tc.json), &v, Closed)
			if tc.wantErr == noErrKind {
				if err != nil {
					t.Fatalf("Decode: %v", err)
				}
				return
			}
			var de *DecodeError
			if !errors.As(err, &de) {
				t.Fatalf("want DecodeError, got %v", err)
			}
			if de.Kind != tc.wantErr {
				t.Fatalf("kind = %v, want %v (%v)", de.Kind, tc.wantErr, err)
			}
		})
	}
}

const noErrKind Kind = -1

func TestNullStringPresence(t *testing.T) {
	var v struct {
		ClosedType
		Maybe NullString `json:"maybe" sift:"keyrequired"`
	}
	if err := Decode([]byte(`{"maybe":null}`), &v, Closed); err != nil {
		t.Fatalf("null: %v", err)
	}
	if !v.Maybe.Present || !v.Maybe.Null {
		t.Fatalf("null case: %+v", v.Maybe)
	}
	if err := Decode([]byte(`{"maybe":"x"}`), &v, Closed); err != nil {
		t.Fatalf("value: %v", err)
	}
	if !v.Maybe.Present || v.Maybe.Null || v.Maybe.Value != "x" {
		t.Fatalf("value case: %+v", v.Maybe)
	}
	var v2 struct {
		ClosedType
		Maybe NullString `json:"maybe" sift:"keyrequired"`
	}
	if err := Decode([]byte(`{}`), &v2, Closed); err == nil {
		t.Fatal("absent key must fail keyrequired")
	}
	// Marshal round-trip.
	b, err := NullString{Present: true, Null: true}.MarshalJSON()
	if err != nil || string(b) != "null" {
		t.Fatalf("marshal null: %q %v", b, err)
	}
	b, err = NullString{Present: true, Value: "x"}.MarshalJSON()
	if err != nil || string(b) != `"x"` {
		t.Fatalf("marshal value: %q %v", b, err)
	}
}

func TestRejectDuplicateKeys(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		wantErr bool
	}{
		{name: "flat_ok", json: `{"a":1,"b":2}`},
		{name: "flat_dup", json: `{"a":1,"a":2}`, wantErr: true},
		{name: "nested_ok", json: `{"a":{"a":1},"b":[{"a":2}]}`},
		{name: "nested_dup", json: `{"a":{"x":1,"x":2}}`, wantErr: true},
		{name: "array_dup", json: `[{"k":true,"k":false}]`, wantErr: true},
		{name: "dup_after_nested", json: `{"a":{"y":1},"a":2}`, wantErr: true},
		{name: "string_with_brace", json: `{"a":"}{","b":1}`},
		{name: "empty", json: `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := RejectDuplicateKeys([]byte(tc.json))
			if tc.wantErr {
				var de *DecodeError
				if !errors.As(err, &de) || de.Kind != KindDuplicateKey {
					t.Fatalf("want duplicate_key, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
		})
	}
}

func TestCanonical(t *testing.T) {
	out, err := Canonical(map[string]any{"b": 1, "a": map[string]any{"y": 2, "x": 3}})
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	want := `{"a":{"x":3,"y":2},"b":1}`
	if string(out) != want {
		t.Fatalf("canonical = %s, want %s", out, want)
	}
	// NaN/Infinity rejected by the first marshal.
	if _, err := Canonical(map[string]any{"x": make(chan int)}); err == nil {
		t.Fatal("unsupported value must fail")
	}
}
