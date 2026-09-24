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
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"time"
)

var timeType = reflect.TypeOf(time.Time{})

// For derives an inline JSON Schema from Go type T's exported fields. Nested structs
// are inlined (no $ref/$defs). Field names + optionality come from the `json` tag (a
// field is required unless it is a pointer or its json tag has ",omitempty");
// descriptions come from the `desc` tag.
func For[T any]() (json.RawMessage, error) {
	s := reflectSchema(reflect.TypeFor[T](), map[reflect.Type]bool{})
	return json.Marshal(s)
}

func reflectSchema(t reflect.Type, seen map[reflect.Type]bool) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == timeType {
		return map[string]any{"type": "string", "format": "date-time"}
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 { // []byte → base64 string in JSON
			return map[string]any{"type": "string"}
		}
		return map[string]any{"type": "array", "items": reflectSchema(t.Elem(), seen)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": reflectSchema(t.Elem(), seen)}
	case reflect.Struct:
		if seen[t] { // recursion guard — emit a permissive object rather than loop forever
			return map[string]any{"type": "object"}
		}
		seen[t] = true
		defer delete(seen, t)

		props := map[string]any{}
		var required []string
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, omitempty := jsonField(f)
			if name == "-" {
				continue
			}
			fs := reflectSchema(f.Type, seen)
			if d := f.Tag.Get("desc"); d != "" {
				fs["description"] = d
			}
			props[name] = fs
			if !omitempty && f.Type.Kind() != reflect.Pointer {
				required = append(required, name)
			}
		}
		out := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			sort.Strings(required)
			out["required"] = required
		}
		return out
	default: // interface{}, chan, func, etc. → unconstrained
		return map[string]any{}
	}
}

func jsonField(f reflect.StructField) (name string, omitempty bool) {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name, false
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	if name == "" {
		name = f.Name
	}
	for _, p := range parts[1:] {
		if p == "omitempty" {
			omitempty = true
		}
	}
	return name, omitempty
}

// OpenAIStrict transforms a neutral schema into OpenAI structured-output "strict" form:
// additionalProperties:false on every object and every property listed in required
// (OpenAI strict rejects schemas that omit either). Operates on the inline schema.
func OpenAIStrict(neutral json.RawMessage) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(neutral, &v); err != nil {
		return nil, err
	}
	strictify(v)
	return json.Marshal(v)
}

func strictify(v any) {
	switch t := v.(type) {
	case map[string]any:
		if t["type"] == "object" {
			t["additionalProperties"] = false
			if props, ok := t["properties"].(map[string]any); ok {
				keys := make([]string, 0, len(props))
				for k, pv := range props {
					keys = append(keys, k)
					strictify(pv)
				}
				sort.Strings(keys)
				t["required"] = keys
			}
		}
		if items, ok := t["items"]; ok {
			strictify(items)
		}
	case []any:
		for _, e := range t {
			strictify(e)
		}
	}
}
