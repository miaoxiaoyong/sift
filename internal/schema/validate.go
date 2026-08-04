package schema

import (
	"fmt"
	"reflect"
	"strings"
)

// validate runs the generic, type-driven checks that every boundary contract
// shares, regardless of closed/open-envelope mode:
//
//  1. Every field tagged `sift:"required"` is present (non-nil pointer).
//  2. Every non-nil field whose type implements [Enumerated] holds an allowed
//     value.
//
// It recurses into nested structs, slices and maps so that adding a nested
// boundary type does not require wiring up per-type validation by hand. Domain
// rules that depend on several fields together belong in a [Validator].
func validate(root reflect.Value) error {
	return walkValue(root, "")
}

// walkValue inspects a single value and recurses as needed.
func walkValue(v reflect.Value, path string) error {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		return walkValue(v.Elem(), path)
	case reflect.Struct:
		return walkStruct(v, path)
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := walkValue(v.Index(i), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			if err := walkValue(v.MapIndex(k), fmt.Sprintf("%s[%v]", path, k.Interface())); err != nil {
				return err
			}
		}
	case reflect.String:
		// Named string types carrying an enum surface here after pointer
		// dereference; plain string has nothing to check.
		if err := checkEnum(v, path); err != nil {
			return err
		}
	}
	return nil
}

// walkStruct enforces required fields then recurses into each field value.
func walkStruct(v reflect.Value, path string) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, named := jsonFieldName(f)
		if !named || name == "-" {
			continue
		}
		fieldPath := name
		if path != "" {
			fieldPath = path + "." + name
		}

		fv := v.Field(i)
		rules := parseRules(f.Tag.Get("sift"))
		if rules.required && isNilPointer(fv) {
			return &DecodeError{Kind: KindMissingRequired, Field: fieldPath, Err: errMissingRequired}
		}
		if err := walkValue(fv, fieldPath); err != nil {
			return err
		}
		if err := checkFieldConstraints(fv, fieldPath, rules); err != nil {
			return err
		}
	}
	return nil
}

// checkEnum validates a named string value against its allowed set.
func checkEnum(v reflect.Value, path string) error {
	e, ok := v.Interface().(Enumerated)
	if !ok {
		return nil
	}
	allowed := e.EnumValues()
	actual := v.String()
	for _, a := range allowed {
		if actual == a {
			return nil
		}
	}
	return &DecodeError{
		Kind:  KindInvalidEnum,
		Field: path,
		Err:   fmt.Errorf("value %q not in enum %v", actual, allowed),
	}
}

// jsonFieldName resolves the JSON object key for a struct field, honoring
// `json:"name"` and embedded/anonymous fields. named reports whether the field
// is serialized as its own JSON key (false for fields with json:"-").
func jsonFieldName(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name, true
	}
	parts := strings.Split(tag, ",")
	name := parts[0]
	if name == "-" {
		return "-", false
	}
	if name == "" {
		return f.Name, true
	}
	return name, true
}

func isNilPointer(v reflect.Value) bool {
	return v.Kind() == reflect.Pointer && v.IsNil()
}

var errMissingRequired = fmt.Errorf("required field missing or null")
