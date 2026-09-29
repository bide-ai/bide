// Package schema derives JSON Schema from Go types and emits provider-specific
// dialects. Reflection-based frameworks all trip on the same wall: the schema
// reflection emits ($ref/$defs, missing additionalProperties) gets rejected by OpenAI
// strict mode and Gemini, so everyone reinvents a sanitizer. We emit INLINE (no
// $ref/$defs — recursion-safe) and provide dialect transforms so the tool author never
// hand-edits JSON Schema.
//
// Tools expose the neutral schema (For); each model adapter dialectizes it at request
// time (Anthropic accepts the neutral form; OpenAI needs OpenAIStrict; Gemini needs Gemini).
package schema

import (
	"cmp"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"time"

	"github.com/bide-ai/bide/internal/jsonfields"
)

var (
	timeType        = reflect.TypeOf(time.Time{})
	rawMessageType  = reflect.TypeOf(json.RawMessage{})
	textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()
	jsonUnmarshaler = reflect.TypeFor[json.Unmarshaler]()
)

// ErrUnsupportedType reports a Go type For cannot describe: a document valid under any schema
// for it would fail to decode, so For refuses the type rather than emit such a schema.
var ErrUnsupportedType = errors.New("schema: Go type cannot be described")

// ErrStrictUnsupported reports a schema OpenAIStrict cannot express in strict mode without
// changing which documents it admits (a map, a recursive type, a free-form object).
var ErrStrictUnsupported = errors.New("schema: not expressible in OpenAI strict mode")

// implements reports whether t or *t satisfies the interface iface.
func implements(t, iface reflect.Type) bool {
	return t.Implements(iface) || reflect.PointerTo(t).Implements(iface)
}

// For derives an inline JSON Schema from Go type T. Nested structs are inlined (no
// $ref/$defs). Descriptions come from the `desc` tag. A field is required unless it is a
// pointer or its json tag has ",omitempty" or ",omitzero".
//
// It mirrors encoding/json's decoding of T exactly: the object's properties are the fields
// encoding/json reads, found by the same rules (json tag names, "-" and "-,", embedded structs
// promoted into the parent unless the tag names them, and Go's dominance rules for fields that
// embedding supplies more than once, where a tie removes the name altogether). A ",string" field
// is a JSON string holding the value. []byte is a base64 string but a byte array is an array of
// numbers; a Go array has exactly its length in items. An integer is bounded by its Go kind
// (minimum 0 for an unsigned kind, and both bounds for a kind of 32 bits or fewer), and the keys
// of a map with integer keys are decimal integers (propertyNames). time.Time is a date-time
// string; json.RawMessage and interface{} are unconstrained. A type that decodes itself is not
// reflected field by field (reflection cannot see the custom shape), and its DECODING methods
// decide, as they do for encoding/json: a json.Unmarshaler is unconstrained, and otherwise an
// encoding.TextUnmarshaler is a string. (A type that only marshals itself is decoded field by
// field, so it is reflected like any other.) A recursive reference is cut off as an unconstrained
// object or array.
//
// It returns an error wrapping ErrUnsupportedType for a type encoding/json cannot decode as any
// schema would describe it: a field reached through an embedded pointer to an unexported struct
// type, which encoding/json cannot allocate; a json tag name encoding/json does not accept (one
// with a quote, a backslash, or another reserved character), which encoding/json reads
// differently depending on how it is built; a kind encoding/json decodes no value but null into
// (a channel, a function, a complex number, an unsafe.Pointer, an interface with methods, or a
// map whose key type is not a string, an integer, or an encoding.TextUnmarshaler); and a pointer
// type that points to itself.
func For[T any]() (json.RawMessage, error) {
	s, err := reflectSchema(reflect.TypeFor[T](), map[reflect.Type]bool{})
	if err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

func reflectSchema(t reflect.Type, seen map[reflect.Type]bool) (map[string]any, error) {
	for ptrs := map[reflect.Type]bool{}; t.Kind() == reflect.Pointer; t = t.Elem() {
		if ptrs[t] { // a pointer type that points to itself: encoding/json recurses forever on it
			return nil, fmt.Errorf("%w: %s is a pointer type that points to itself", ErrUnsupportedType, t)
		}
		ptrs[t] = true
	}
	// Types that decode themselves can't be inferred from their fields. encoding/json prefers
	// UnmarshalJSON, whose accepted shape we can't see, so leave it unconstrained; otherwise
	// UnmarshalText reads a JSON string. (time.Time and json.RawMessage are the common
	// json.Unmarshalers, handled precisely.)
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}, nil
	case t == rawMessageType:
		return map[string]any{}, nil // json.RawMessage is arbitrary JSON
	case implements(t, jsonUnmarshaler):
		return map[string]any{}, nil
	case implements(t, textUnmarshaler):
		return map[string]any{"type": "string"}, nil
	}
	// A named slice, array, or map can refer to itself (type Tree map[string]Tree): cut the
	// recursion off, as for a struct below, with the kind's permissive schema.
	switch t.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		if seen[t] {
			if t.Kind() == reflect.Map {
				return map[string]any{"type": "object"}, nil
			}
			return map[string]any{"type": "array"}, nil
		}
		seen[t] = true
		defer delete(seen, t)
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		s := map[string]any{"type": "integer"}
		if t.Bits() <= 32 { // wider bounds are not exact in the float64 that readers parse into
			s["minimum"], s["maximum"] = -int64(1)<<(t.Bits()-1), int64(1)<<(t.Bits()-1)-1
		}
		return s, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		s := map[string]any{"type": "integer", "minimum": 0}
		if t.Bits() <= 32 {
			s["maximum"] = uint64(math.MaxUint64) >> (64 - t.Bits())
		}
		return s, nil
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}, nil
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 { // []byte → base64 string in JSON
			return map[string]any{"type": "string"}, nil
		}
		items, err := reflectSchema(t.Elem(), seen)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": items}, nil
	case reflect.Array:
		// A byte ARRAY (unlike a []byte SLICE) marshals as a JSON array of numbers. encoding/json
		// zero-fills a short array and drops the items past its length, so the length is fixed.
		items, err := reflectSchema(t.Elem(), seen)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": items, "minItems": t.Len(), "maxItems": t.Len()}, nil
	case reflect.Map:
		// encoding/json reads a key with the key type's UnmarshalText if it has one, else by its
		// kind: a string as is, an integer in decimal (strconv.ParseInt or ParseUint). It reads no
		// other key type, so such a map decodes only when empty.
		var keys string
		switch kt := t.Key(); {
		case reflect.PointerTo(kt).Implements(textUnmarshaler), kt.Kind() == reflect.String:
		case kt.Kind() >= reflect.Int && kt.Kind() <= reflect.Int64:
			keys = `^[+-]?[0-9]+$`
		case kt.Kind() >= reflect.Uint && kt.Kind() <= reflect.Uintptr:
			keys = `^[0-9]+$`
		default:
			return nil, fmt.Errorf("%w: %s has the key type %s, which encoding/json cannot read a key into", ErrUnsupportedType, t, kt)
		}
		values, err := reflectSchema(t.Elem(), seen)
		if err != nil {
			return nil, err
		}
		s := map[string]any{"type": "object", "additionalProperties": values}
		if keys != "" {
			s["propertyNames"] = map[string]any{"pattern": keys}
		}
		return s, nil
	case reflect.Interface:
		if t.NumMethod() > 0 { // encoding/json stores a decoded value only in an empty interface
			return nil, fmt.Errorf("%w: %s is an interface with methods, which encoding/json cannot decode a value into", ErrUnsupportedType, t)
		}
		return map[string]any{}, nil // any value
	case reflect.Struct:
		if seen[t] { // recursion guard — emit a permissive object rather than loop forever
			return map[string]any{"type": "object"}, nil
		}
		seen[t] = true
		defer delete(seen, t)

		props := map[string]any{}
		var required []string
		fields, err := jsonfields.Of(t)
		if err != nil {
			var tn *jsonfields.TagNameError
			if errors.As(err, &tn) {
				// encoding/json v1 falls back to the Go name here and its v2 implementation
				// reads the name differently, so the field has no one JSON name.
				return nil, fmt.Errorf("%w: %s: field %s has the json tag name %q, which encoding/json does not accept as a name", ErrUnsupportedType, tn.Struct, tn.Field, tn.Name)
			}
			return nil, err
		}
		for _, f := range fields {
			if f.ViaUnexportedPtr != "" {
				return nil, fmt.Errorf("%w: %s: field %q is reached through the embedded pointer to the unexported struct type %s, which encoding/json cannot allocate when decoding", ErrUnsupportedType, t, f.Name, f.ViaUnexportedPtr)
			}
			var fs map[string]any
			if f.Quoted {
				fs = quotedSchema(f.Type)
			} else {
				var err error
				if fs, err = reflectSchema(f.Field.Type, seen); err != nil {
					return nil, fmt.Errorf("%s field %s: %w", t, f.Field.Name, err)
				}
			}
			if d := f.Field.Tag.Get("desc"); d != "" {
				fs["description"] = d
			}
			props[f.Name] = fs
			if f.Required() {
				required = append(required, f.Name)
			}
		}
		out := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			slices.Sort(required)
			out["required"] = required
		}
		return out, nil
	default: // chan, func, complex, unsafe.Pointer: encoding/json decodes no value but null into them
		return nil, fmt.Errorf("%w: %s is a kind encoding/json cannot decode a value into", ErrUnsupportedType, t)
	}
}

// quotedSchema is the schema of a ",string" field of kind k: a JSON string holding the value's
// JSON text, which is what encoding/json reads for such a field.
func quotedSchema(t reflect.Type) map[string]any {
	switch t.Kind() {
	case reflect.Bool:
		return map[string]any{"type": "string", "enum": []string{"true", "false"}}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "string", "pattern": `^-?(0|[1-9][0-9]*)$`}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return map[string]any{"type": "string", "pattern": `^(0|[1-9][0-9]*)$`}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "string", "pattern": `^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`}
	default: // reflect.String: the string's own JSON encoding, quotes included
		return map[string]any{"type": "string", "pattern": `^"([^"\\]|\\.)*"$`}
	}
}

// OpenAIStrict transforms a neutral schema into OpenAI structured-output "strict" form, which
// closes every object (additionalProperties:false) and lists every property in required. It
// changes no set of accepted answers beyond that:
//
//   - A property the neutral schema leaves optional (a pointer or omitempty field) stays
//     optional in meaning: it is required but nullable (type [T, "null"], and null added to an
//     enum), so the model sends null rather than inventing a value. encoding/json decodes null
//     into such a field as its zero value.
//   - An object strict mode cannot close without changing it (a map, whose keys are data; a
//     recursive type, which For cuts off as an open object; any object with no declared
//     properties or with additionalProperties or patternProperties) is an error wrapping
//     ErrStrictUnsupported that names the offending location. Closing it would admit only {},
//     so every answer would arrive empty.
//   - An untyped schema ({}, which For gives any and json.RawMessage, or the schema true) admits
//     every value, which strict mode cannot express; it is an error wrapping ErrStrictUnsupported
//     too. So is an array with no items schema (a recursive slice, which For cuts off that way),
//     whose items are untyped.
//
// Operates on the inline schema.
func OpenAIStrict(neutral json.RawMessage) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(neutral, &v); err != nil {
		return nil, err
	}
	if err := strictify(v, ""); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func strictify(v any, path string) error {
	where := cmp.Or(path, "the root schema")
	if v == true {
		return fmt.Errorf("%w: %s admits any value (the schema true)", ErrStrictUnsupported, where)
	}
	s, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	if unconstrained(s) {
		return fmt.Errorf("%w: %s admits any value (an untyped schema, from any or json.RawMessage)", ErrStrictUnsupported, where)
	}
	at := func(p string) string {
		if path == "" {
			return p
		}
		return path + "." + p
	}
	if isObject(s) {
		if _, ok := s["patternProperties"]; ok {
			return fmt.Errorf("%w: %s has patternProperties", ErrStrictUnsupported, where)
		}
		if ap, ok := s["additionalProperties"]; ok && ap != false {
			return fmt.Errorf("%w: %s allows additional properties (a map, whose keys are data)", ErrStrictUnsupported, where)
		}
		props, ok := s["properties"].(map[string]any)
		if !ok {
			if s["additionalProperties"] == false {
				props = map[string]any{} // already closed and empty: {} is its only instance
			} else {
				return fmt.Errorf("%w: %s declares no properties (a map, a recursive type, or a free-form object)", ErrStrictUnsupported, where)
			}
		}
		required := map[string]bool{}
		if rs, ok := s["required"].([]any); ok {
			for _, r := range rs {
				if name, ok := r.(string); ok {
					required[name] = true
				}
			}
		}
		keys := slices.Sorted(maps.Keys(props))
		for _, k := range keys {
			if err := strictify(props[k], at("properties."+k)); err != nil {
				return err
			}
			if !required[k] {
				props[k] = nullable(props[k])
			}
		}
		s["additionalProperties"] = false
		s["required"] = keys
	}
	if hasType(s, "array") {
		if _, ok := s["items"]; !ok {
			return fmt.Errorf("%w: %s is an array with no items schema, so it admits any items (For cuts off a recursive slice this way)", ErrStrictUnsupported, where)
		}
	}
	if err := strictify(s["items"], at("items")); err != nil {
		return err
	}
	for _, kw := range []string{"anyOf", "oneOf", "allOf"} {
		branches, _ := s[kw].([]any)
		for i, b := range branches {
			if err := strictify(b, at(fmt.Sprintf("%s.%d", kw, i))); err != nil {
				return err
			}
		}
	}
	return nil
}

// unconstrained reports whether schema s names nothing it admits: no type, enum, const, branches,
// or reference. Such a schema admits every value, which strict mode cannot express.
func unconstrained(s map[string]any) bool {
	for _, kw := range []string{"type", "enum", "const", "anyOf", "oneOf", "allOf", "$ref"} {
		if _, ok := s[kw]; ok {
			return false
		}
	}
	return true
}

// isObject reports whether schema s describes an object (type "object", alone or in a list).
func isObject(s map[string]any) bool { return hasType(s, "object") }

// hasType reports whether schema s names the type name, alone or in a list.
func hasType(s map[string]any, name string) bool {
	switch t := s["type"].(type) {
	case string:
		return t == name
	case []any:
		return slices.Contains(t, any(name))
	}
	return false
}

// nullable returns schema p widened to also admit null.
func nullable(p any) any {
	s, ok := p.(map[string]any)
	if !ok {
		return p
	}
	switch t := s["type"].(type) {
	case string:
		if t != "null" {
			s["type"] = []any{t, "null"}
		}
	case []any:
		if !slices.Contains(t, any("null")) {
			s["type"] = append(t, "null")
		}
	}
	if enum, ok := s["enum"].([]any); ok && !slices.Contains(enum, nil) {
		s["enum"] = append(enum, nil)
	}
	if branches, ok := s["anyOf"].([]any); ok {
		s["anyOf"] = append(branches, map[string]any{"type": "null"})
	}
	return s
}
