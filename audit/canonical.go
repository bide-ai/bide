package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

// UnmarshalStrict decodes a JSON proof artifact (a bundle, certificate, evidence package, or tree
// head) so that what a person reads in the file is exactly what is verified. encoding/json accepts
// a duplicate key and keeps the last value, matches keys case-insensitively (so "Size" and "SIZE"
// are the same field), ignores unknown fields, and rewrites invalid UTF-8; each lets a file show a
// reader one value while the verifier checks another. UnmarshalStrict rejects all four: duplicate
// names (at any depth, including inside embedded raw JSON), names that do not match a field
// exactly, unknown fields, and invalid UTF-8.
//
// It uses only the standard encoding/json: the input is checked token by token against the target
// type before it is decoded, so the rules above hold without depending on encoding/json/v2.
func UnmarshalStrict(data []byte, v any) error {
	if !utf8.Valid(data) {
		return errors.New("audit: strict json: invalid UTF-8")
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("audit: strict json: decode target must be a non-nil pointer, got %T", v)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := strictValue(dec, rv.Type().Elem(), "$"); err != nil {
		return fmt.Errorf("audit: strict json: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("audit: strict json: trailing data after the value")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

var unmarshalerType = reflect.TypeFor[json.Unmarshaler]()

// strictValue consumes one JSON value from dec, checking it against t: an object decoded into a
// struct may use only that struct's exact JSON field names, each at most once; every other object
// (a map, raw JSON, a type with its own UnmarshalJSON, an interface) is checked for duplicate names
// only, at any depth.
func strictValue(dec *json.Decoder, t reflect.Type, path string) error {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	delim, isDelim := tok.(json.Delim)
	if !isDelim {
		return nil // a scalar: encoding/json checks its type when decoding
	}
	custom := t == nil || t == rawMessageType || t.Kind() == reflect.Interface ||
		reflect.PointerTo(t).Implements(unmarshalerType) || t.Implements(unmarshalerType)
	switch delim {
	case '[':
		var elem reflect.Type
		if !custom && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
			elem = t.Elem()
		}
		for i := 0; dec.More(); i++ {
			if err := strictValue(dec, elem, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		_, err := dec.Token() // ]
		return err
	case '{':
		var fields map[string]reflect.Type
		var mapElem reflect.Type
		if !custom {
			switch t.Kind() {
			case reflect.Struct:
				fields = jsonFields(t)
			case reflect.Map:
				mapElem = t.Elem()
			}
		}
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			key := kt.(string)
			if seen[key] {
				return fmt.Errorf("%s: duplicate name %q", path, key)
			}
			seen[key] = true
			var next reflect.Type
			switch {
			case fields != nil:
				ft, ok := fields[key]
				if !ok {
					return fmt.Errorf("%s: %q is not a field of %s (names must match exactly)", path, key, t)
				}
				next = ft
			case mapElem != nil:
				next = mapElem
			}
			if err := strictValue(dec, next, path+"."+key); err != nil {
				return err
			}
		}
		_, err := dec.Token() // }
		return err
	}
	return nil
}

// jsonFields returns the exact JSON names encoding/json decodes into struct type t, with each
// field's type, by encoding/json's own rules: exported fields by tag name or Go name (a "-" tag is
// skipped), and the fields of an untagged embedded struct promoted into t. When several fields
// claim one name, the shallowest wins, and at equal depth a tagged field wins over an untagged one.
// A name still claimed by several fields is one encoding/json ignores; it is kept here, and the
// decode that follows (DisallowUnknownFields) rejects it.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	type cand struct {
		t      reflect.Type
		depth  int
		tagged bool
	}
	cands := map[string][]cand{}
	var walk func(t reflect.Type, depth int, visited map[reflect.Type]bool)
	walk = func(t reflect.Type, depth int, visited map[reflect.Type]bool) {
		if visited[t] {
			return
		}
		visited[t] = true
		defer delete(visited, t)
		for i := range t.NumField() {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name, _, _ := strings.Cut(tag, ",")
			if f.Anonymous && name == "" {
				et := f.Type
				if et.Kind() == reflect.Pointer {
					et = et.Elem()
				}
				if et.Kind() == reflect.Struct {
					walk(et, depth+1, visited)
					continue
				}
			}
			if !f.IsExported() {
				continue
			}
			tagged := name != ""
			if !tagged {
				name = f.Name
			}
			cands[name] = append(cands[name], cand{f.Type, depth, tagged})
		}
	}
	walk(t, 0, map[reflect.Type]bool{})
	out := map[string]reflect.Type{}
	for name, cs := range cands {
		minDepth := cs[0].depth
		for _, c := range cs {
			minDepth = min(minDepth, c.depth)
		}
		var top []cand
		for _, c := range cs {
			if c.depth == minDepth {
				top = append(top, c)
			}
		}
		if len(top) > 1 {
			var tagged []cand
			for _, c := range top {
				if c.tagged {
					tagged = append(tagged, c)
				}
			}
			top = tagged
		}
		if len(top) > 0 {
			out[name] = top[0].t
		}
	}
	return out
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
