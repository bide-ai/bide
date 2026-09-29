// Package schema derives JSON Schema from Go types and emits provider-specific
// dialects. Reflection-based frameworks all trip on the same wall: the schema
// reflection emits ($ref/$defs, missing additionalProperties) gets rejected by OpenAI
// strict mode and Gemini, so everyone reinvents a sanitizer. We emit INLINE (no
// $ref/$defs — recursion-safe) and provide dialect transforms so the tool author never
// hand-edits JSON Schema.
//
// Tools expose the neutral schema (For); each model adapter dialectizes it at request
// time (Anthropic accepts the neutral form; OpenAI needs OpenAIStrict).
package schema

import (
	"cmp"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode"
)

var (
	timeType       = reflect.TypeOf(time.Time{})
	rawMessageType = reflect.TypeOf(json.RawMessage{})
	textMarshaler  = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	jsonMarshaler  = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
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
// numbers; a Go array has exactly its length in items. time.Time is a date-time string;
// json.RawMessage and interface{} are unconstrained. A type with a custom marshaler is not
// reflected field by field (reflection cannot see the custom shape): encoding.TextMarshaler is a
// string, any other json.Marshaler is unconstrained. A recursive reference is cut off as an
// unconstrained object.
//
// It returns an error wrapping ErrUnsupportedType for a type encoding/json cannot decode as any
// schema would describe it: a field reached through an embedded pointer to an unexported struct
// type, which encoding/json cannot allocate.
func For[T any]() (json.RawMessage, error) {
	s, err := reflectSchema(reflect.TypeFor[T](), map[reflect.Type]bool{})
	if err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

func reflectSchema(t reflect.Type, seen map[reflect.Type]bool) (map[string]any, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	// Types with custom JSON marshaling can't be inferred from their fields. Handle the
	// common ones precisely, then fall back: TextMarshaler always emits a JSON string;
	// any other json.Marshaler emits a shape we can't see, so leave it unconstrained.
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}, nil
	case t == rawMessageType:
		return map[string]any{}, nil // json.RawMessage is arbitrary JSON
	case implements(t, textMarshaler):
		return map[string]any{"type": "string"}, nil
	case implements(t, jsonMarshaler):
		return map[string]any{}, nil
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}, nil
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
		values, err := reflectSchema(t.Elem(), seen)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "object", "additionalProperties": values}, nil
	case reflect.Struct:
		if seen[t] { // recursion guard — emit a permissive object rather than loop forever
			return map[string]any{"type": "object"}, nil
		}
		seen[t] = true
		defer delete(seen, t)

		props := map[string]any{}
		var required []string
		for _, f := range jsonFields(t) {
			if f.viaUnexportedPtr != "" {
				return nil, fmt.Errorf("%w: %s: field %q is reached through the embedded pointer to the unexported struct type %s, which encoding/json cannot allocate when decoding", ErrUnsupportedType, t, f.name, f.viaUnexportedPtr)
			}
			var fs map[string]any
			if f.quoted {
				fs = quotedSchema(f.typ)
			} else {
				var err error
				if fs, err = reflectSchema(f.field.Type, seen); err != nil {
					return nil, err
				}
			}
			if d := f.field.Tag.Get("desc"); d != "" {
				fs["description"] = d
			}
			props[f.name] = fs
			if !f.optional && f.field.Type.Kind() != reflect.Pointer {
				required = append(required, f.name)
			}
		}
		out := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			slices.Sort(required)
			out["required"] = required
		}
		return out, nil
	default: // interface{}, chan, func, etc. → unconstrained
		return map[string]any{}, nil
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

// jsonField is one field encoding/json reads from a JSON object into a struct.
type jsonField struct {
	name     string
	tagged   bool  // the name came from a json tag
	index    []int // the field's index path from the outer struct
	field    reflect.StructField
	typ      reflect.Type // the field's type, with an unnamed pointer followed
	optional bool         // ",omitempty" or ",omitzero"
	quoted   bool         // ",string" on a string, number, or boolean field
	// viaUnexportedPtr names the unexported struct type of an embedded pointer on the field's
	// path, if any: encoding/json cannot allocate it, so decoding the field fails.
	viaUnexportedPtr string
}

// jsonFields returns the fields encoding/json reads for struct type t, in index order. It is a
// port of encoding/json's typeFields: a breadth-first walk over embedded structs, then Go's
// dominance rules (the shallowest name wins, a json tag breaks a tie at one depth, and any other
// tie removes the name).
func jsonFields(t reflect.Type) []jsonField {
	type level struct {
		typ              reflect.Type
		index            []int
		viaUnexportedPtr string
	}
	var current []level
	next := []level{{typ: t}}
	var count, nextCount map[reflect.Type]int
	visited := map[reflect.Type]bool{}
	var fields []jsonField

	for len(next) > 0 {
		current, next = next, current[:0]
		count, nextCount = nextCount, map[reflect.Type]int{}
		for _, f := range current {
			if visited[f.typ] {
				continue
			}
			visited[f.typ] = true
			for i := 0; i < f.typ.NumField(); i++ {
				sf := f.typ.Field(i)
				if sf.Anonymous {
					et := sf.Type
					if et.Kind() == reflect.Pointer {
						et = et.Elem()
					}
					if !sf.IsExported() && et.Kind() != reflect.Struct {
						continue // an embedded unexported non-struct type has no fields to promote
					}
				} else if !sf.IsExported() {
					continue
				}
				tag := sf.Tag.Get("json")
				if tag == "-" {
					continue
				}
				name, opts, _ := strings.Cut(tag, ",")
				if !validTagName(name) {
					name = ""
				}
				index := append(slices.Clip(f.index), i)
				ft := sf.Type
				if ft.Name() == "" && ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				quoted := false
				if hasOption(opts, "string") {
					switch ft.Kind() {
					case reflect.Bool,
						reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
						reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
						reflect.Float32, reflect.Float64,
						reflect.String:
						quoted = true
					}
				}
				if name != "" || !sf.Anonymous || ft.Kind() != reflect.Struct {
					field := jsonField{
						name: cmp.Or(name, sf.Name), tagged: name != "", index: index, field: sf, typ: ft,
						optional: hasOption(opts, "omitempty") || hasOption(opts, "omitzero"),
						quoted:   quoted, viaUnexportedPtr: f.viaUnexportedPtr,
					}
					fields = append(fields, field)
					if count[f.typ] > 1 {
						// The embedding struct is reached more than once at this depth: record a
						// second copy so the dominance pass sees the tie and removes the name.
						fields = append(fields, field)
					}
					continue
				}
				// An embedded struct whose tag names nothing: promote its fields next round.
				nextCount[ft]++
				if nextCount[ft] == 1 {
					via := f.viaUnexportedPtr
					if via == "" && sf.Type.Kind() == reflect.Pointer && !sf.IsExported() {
						via = ft.String()
					}
					next = append(next, level{typ: ft, index: index, viaUnexportedPtr: via})
				}
			}
		}
	}

	slices.SortFunc(fields, func(a, b jsonField) int {
		if c := strings.Compare(a.name, b.name); c != 0 {
			return c
		}
		if c := cmp.Compare(len(a.index), len(b.index)); c != 0 {
			return c
		}
		if a.tagged != b.tagged {
			if a.tagged {
				return -1
			}
			return +1
		}
		return slices.Compare(a.index, b.index)
	})
	out := fields[:0]
	for i := 0; i < len(fields); {
		j := i + 1
		for j < len(fields) && fields[j].name == fields[i].name {
			j++
		}
		// The first field of a name dominates, unless the second ties it on depth and tagging.
		if j-i == 1 || len(fields[i].index) != len(fields[i+1].index) || fields[i].tagged != fields[i+1].tagged {
			out = append(out, fields[i])
		}
		i = j
	}
	slices.SortFunc(out, func(a, b jsonField) int { return slices.Compare(a.index, b.index) })
	return out
}

// validTagName reports whether encoding/json accepts s as a field name in a json tag.
func validTagName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c):
			// Backslash and quote are reserved; other punctuation is allowed.
		case !unicode.IsLetter(c) && !unicode.IsDigit(c):
			return false
		}
	}
	return true
}

// hasOption reports whether a json tag's comma-separated options include opt.
func hasOption(opts, opt string) bool {
	for opts != "" {
		var o string
		o, opts, _ = strings.Cut(opts, ",")
		if o == opt {
			return true
		}
	}
	return false
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
	s, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	at := func(p string) string {
		if path == "" {
			return p
		}
		return path + "." + p
	}
	if isObject(s) {
		where := cmp.Or(path, "the root object")
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

// isObject reports whether schema s describes an object (type "object", alone or in a list).
func isObject(s map[string]any) bool {
	switch t := s["type"].(type) {
	case string:
		return t == "object"
	case []any:
		return slices.Contains(t, any("object"))
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
