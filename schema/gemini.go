package schema

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// ErrGeminiUnsupported reports a schema Gemini's schema subset cannot express without changing
// which documents it admits (a map, an unconstrained value, an object with no properties).
var ErrGeminiUnsupported = errors.New("schema: not expressible in Gemini's schema subset")

// Gemini transforms a neutral schema into the OpenAPI 3.0 subset Gemini reads in a function
// declaration's parameters and in responseSchema:
//
//   - A type list with "null" becomes the type plus nullable:true; null in an enum, or a
//     {"type":"null"} branch of anyOf, becomes nullable:true too. A string const becomes a
//     one-value enum.
//   - additionalProperties:false is dropped (Gemini objects admit only their properties).
//     Validation keywords Gemini lacks (uniqueItems, exclusiveMinimum, exclusiveMaximum,
//     multipleOf), annotations ($schema, $id, $comment, examples, readOnly, writeOnly,
//     deprecated), and a format Gemini does not know for the type are dropped: they only guide
//     the model, and the tool still checks what it receives.
//   - A construct Gemini cannot express is an error wrapping ErrGeminiUnsupported that names
//     its location: an object with no properties or with additionalProperties (a map, whose keys
//     are data; a recursive type, which For cuts off as an open object), a schema with no type
//     (interface{} or json.RawMessage, which For leaves unconstrained), a list of several
//     non-null types, a non-string enum or const, an array with no items, and any keyword
//     outside the subset ($ref, oneOf, allOf, not, patternProperties, ...).
//
// A tool with no arguments has no schema to send; the Gemini adapter omits its parameters
// rather than calling this with an empty object, which Gemini rejects.
func Gemini(neutral json.RawMessage) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(neutral, &v); err != nil {
		return nil, err
	}
	out, err := geminify(v, "")
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

// geminiKept are the keywords Gemini's schema subset shares with JSON Schema, copied as they are
// (type, enum, properties, items, required, anyOf, format, and const are handled separately).
var geminiKept = map[string]bool{
	"description": true, "title": true, "nullable": true, "default": true, "example": true,
	"minItems": true, "maxItems": true, "minLength": true, "maxLength": true, "pattern": true,
	"minimum": true, "maximum": true, "minProperties": true, "maxProperties": true,
	"propertyOrdering": true,
}

// geminiDropped are keywords with no Gemini counterpart that only annotate or further constrain
// a value, so leaving them out loses guidance but admits no document of another shape.
var geminiDropped = map[string]bool{
	"$schema": true, "$id": true, "$comment": true, "examples": true, "readOnly": true,
	"writeOnly": true, "deprecated": true, "uniqueItems": true, "exclusiveMinimum": true,
	"exclusiveMaximum": true, "multipleOf": true,
}

// geminiHandled are the keywords geminify translates itself.
var geminiHandled = map[string]bool{
	"type": true, "enum": true, "const": true, "anyOf": true, "format": true,
	"properties": true, "required": true, "additionalProperties": true, "items": true,
}

// geminiFormats are the formats Gemini accepts, by type.
var geminiFormats = map[string][]string{
	"string":  {"enum", "date-time"},
	"number":  {"float", "double"},
	"integer": {"int32", "int64"},
}

func geminify(v any, path string) (map[string]any, error) {
	where := cmp.Or(path, "the root schema")
	at := func(p string) string {
		if path == "" {
			return p
		}
		return path + "." + p
	}
	s, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: %s is %v, not a schema object", ErrGeminiUnsupported, where, v)
	}
	for _, k := range slices.Sorted(maps.Keys(s)) {
		if !geminiKept[k] && !geminiDropped[k] && !geminiHandled[k] {
			return nil, fmt.Errorf("%w: %s uses %q, which Gemini's schema subset does not have", ErrGeminiUnsupported, where, k)
		}
	}
	out := map[string]any{}
	nullable := false

	// type: one name, or a list of one name plus "null".
	typ := ""
	switch t := s["type"].(type) {
	case nil:
	case string:
		typ = t
	case []any:
		var names []string
		for _, n := range t {
			switch n {
			case "null":
				nullable = true
			default:
				name, ok := n.(string)
				if !ok {
					return nil, fmt.Errorf("%w: %s has type %v", ErrGeminiUnsupported, where, t)
				}
				names = append(names, name)
			}
		}
		if len(names) != 1 {
			return nil, fmt.Errorf("%w: %s has type %v (Gemini takes one type, optionally nullable)", ErrGeminiUnsupported, where, t)
		}
		typ = names[0]
	default:
		return nil, fmt.Errorf("%w: %s has type %v", ErrGeminiUnsupported, where, t)
	}
	if typ == "null" {
		return nil, fmt.Errorf("%w: %s admits only null", ErrGeminiUnsupported, where)
	}

	// enum and const: strings only; null becomes nullable.
	var enum []any
	if e, ok := s["enum"]; ok {
		list, ok := e.([]any)
		if !ok {
			return nil, fmt.Errorf("%w: %s has enum %v", ErrGeminiUnsupported, where, e)
		}
		for _, x := range list {
			switch x.(type) {
			case nil:
				nullable = true
			case string:
				enum = append(enum, x)
			default:
				return nil, fmt.Errorf("%w: %s has the non-string enum value %v", ErrGeminiUnsupported, where, x)
			}
		}
	}
	if c, ok := s["const"]; ok {
		cs, ok := c.(string)
		if !ok {
			return nil, fmt.Errorf("%w: %s has the non-string const %v", ErrGeminiUnsupported, where, c)
		}
		enum = append(enum, cs)
	}
	if enum != nil {
		if typ != "" && typ != "string" {
			return nil, fmt.Errorf("%w: %s has an enum on type %s (Gemini enums are strings)", ErrGeminiUnsupported, where, typ)
		}
		typ = "string"
		out["enum"] = enum
	}

	// anyOf: each branch translated; a null branch becomes nullable.
	if a, ok := s["anyOf"]; ok {
		branches, ok := a.([]any)
		if !ok {
			return nil, fmt.Errorf("%w: %s has anyOf %v", ErrGeminiUnsupported, where, a)
		}
		var kept []any
		for i, b := range branches {
			if bm, ok := b.(map[string]any); ok && len(bm) == 1 && bm["type"] == "null" {
				nullable = true
				continue
			}
			g, err := geminify(b, at(fmt.Sprintf("anyOf.%d", i)))
			if err != nil {
				return nil, err
			}
			kept = append(kept, g)
		}
		if len(kept) == 0 {
			return nil, fmt.Errorf("%w: %s admits only null", ErrGeminiUnsupported, where)
		}
		out["anyOf"] = kept
	}

	if typ == "" && out["anyOf"] == nil {
		return nil, fmt.Errorf("%w: %s has no type (an unconstrained value, such as interface{} or json.RawMessage)", ErrGeminiUnsupported, where)
	}
	if typ != "" {
		out["type"] = typ
	}

	switch typ {
	case "object":
		if ap, ok := s["additionalProperties"]; ok && ap != false {
			return nil, fmt.Errorf("%w: %s allows additional properties (a map, whose keys are data)", ErrGeminiUnsupported, where)
		}
		props, _ := s["properties"].(map[string]any)
		if len(props) == 0 {
			return nil, fmt.Errorf("%w: %s declares no properties (a recursive type, an empty struct, or a free-form object)", ErrGeminiUnsupported, where)
		}
		gp := map[string]any{}
		for _, k := range slices.Sorted(maps.Keys(props)) {
			g, err := geminify(props[k], at("properties."+k))
			if err != nil {
				return nil, err
			}
			gp[k] = g
		}
		out["properties"] = gp
		if r, ok := s["required"].([]any); ok && len(r) > 0 {
			out["required"] = r
		}
	case "array":
		items, ok := s["items"]
		if !ok {
			return nil, fmt.Errorf("%w: %s is an array with no items schema", ErrGeminiUnsupported, where)
		}
		g, err := geminify(items, at("items"))
		if err != nil {
			return nil, err
		}
		out["items"] = g
	}

	if f, ok := s["format"].(string); ok && slices.Contains(geminiFormats[typ], f) {
		out["format"] = f
	}

	for _, k := range slices.Sorted(maps.Keys(s)) {
		switch {
		case geminiKept[k]:
			out[k] = s[k]
		case (k == "properties" || k == "required" || k == "additionalProperties") && typ != "object":
			return nil, fmt.Errorf("%w: %s has %s but is not an object", ErrGeminiUnsupported, where, k)
		case k == "items" && typ != "array":
			return nil, fmt.Errorf("%w: %s has items but is not an array", ErrGeminiUnsupported, where)
		}
	}
	if nullable {
		out["nullable"] = true
	}
	return out, nil
}
