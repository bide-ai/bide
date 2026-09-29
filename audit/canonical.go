package audit

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"reflect"
	"unicode/utf8"
)

// UnmarshalStrict decodes a JSON proof artifact (a bundle, certificate, evidence package, or tree
// head) so that what a person reads in the file is exactly what is verified. encoding/json accepts
// a duplicate key and keeps the last value, matches keys case-insensitively (so "Size" and "SIZE"
// are the same field), ignores unknown fields, and rewrites invalid UTF-8; each lets a file show a
// reader one value while the verifier checks another. UnmarshalStrict rejects all four: duplicate
// names (at any depth, including inside embedded raw JSON), names that do not match a field
// exactly, unknown fields, and invalid UTF-8.
func UnmarshalStrict(data []byte, v any) error {
	return jsonv2.Unmarshal(data, v, jsonv2.RejectUnknownMembers(true))
}

// Every leaf, grant, and seal in this package is hashed or signed over a JSON encoding. JSON
// encoding is injective only over valid UTF-8: encoding/json rewrites each invalid byte in a string
// to U+FFFD, so two values that differ only in invalid bytes would encode, hash, and verify
// identically. checkUTF8 rejects such a value before it is committed or verified. Byte slices are
// exempt (they encode as base64, which is injective) and so is json.RawMessage (it is copied
// verbatim).

var rawMessageType = reflect.TypeFor[json.RawMessage]()

// checkUTF8 returns an error naming the first string in v (any exported field, map key or value,
// slice element, or pointer target, recursively) that is not valid UTF-8.
func checkUTF8(v any) error {
	return checkUTF8Value(reflect.ValueOf(v), "")
}

func checkUTF8Value(v reflect.Value, path string) error {
	switch v.Kind() {
	case reflect.String:
		if !utf8.ValidString(v.String()) {
			return fmt.Errorf("invalid UTF-8 in %s%q", pathPrefix(path), v.String())
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			return checkUTF8Value(v.Elem(), path)
		}
	case reflect.Struct:
		t := v.Type()
		for i := range t.NumField() {
			if f := t.Field(i); f.IsExported() {
				if err := checkUTF8Value(v.Field(i), path+"."+f.Name); err != nil {
					return err
				}
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type() == rawMessageType || v.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}
		for i := range v.Len() {
			if err := checkUTF8Value(v.Index(i), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		it := v.MapRange()
		for it.Next() {
			if err := checkUTF8Value(it.Key(), path+" key"); err != nil {
				return err
			}
			if err := checkUTF8Value(it.Value(), fmt.Sprintf("%s[%v]", path, it.Key())); err != nil {
				return err
			}
		}
	}
	return nil
}

func pathPrefix(path string) string {
	if path == "" {
		return ""
	}
	return path[1:] + " "
}
