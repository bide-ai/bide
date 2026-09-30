package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"unicode/utf8"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/strictjson"
)

// UnmarshalStrict decodes a JSON proof artifact (a bundle, certificate, evidence package, or tree
// head) so that what a person reads in the file is exactly what is verified. encoding/json accepts
// a duplicate key and keeps the last value, matches keys case-insensitively (so "Size" and "SIZE"
// are the same field), ignores unknown fields, and rewrites invalid UTF-8; each lets a file show a
// reader one value while the verifier checks another. UnmarshalStrict rejects all four: duplicate
// names (at any depth, including inside embedded raw JSON), names that do not match a field
// exactly, unknown fields, and invalid UTF-8.
//
// It also rejects the spellings encoding/json decodes lossily, so each decoded value has one
// spelling: an escaped lone surrogate ("\ud800"), which becomes U+FFFD, in any string decoded into
// Go, and, in a []byte field, base64 that is not the standard encoding of the bytes it decodes to
// (line breaks, which encoding/json skips, or unused bits set in the last character). JSON kept
// verbatim (json.RawMessage) is checked for duplicate names only: it is committed as written.
//
// A message (agent.Message) decodes with its own UnmarshalJSON, which matches names loosely, so
// UnmarshalStrict checks it against its wire shape: exactly "role" and "parts", and each part
// exactly the fields of the part its "type" names.
//
// An artifact with a format (ProofBundle, AbsenceBundle, RunCertificate, CurrentGrantProof,
// EventInclusion, EvidencePackage) is checked for its "format" first: one that has none, or not the
// one this version reads, is refused with an error wrapping ErrFormat, before any rule above can
// report a field its own format names differently.
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
	if f, ok := reflect.Zero(rv.Type().Elem()).Interface().(formatted); ok {
		if err := checkDataFormat(data, f); err != nil {
			return fmt.Errorf("audit: strict json: %w", err)
		}
	}
	if err := strictjson.Check(data, rv.Type().Elem(), strictOptions); err != nil {
		return fmt.Errorf("audit: strict json: %w", err)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

// strictOptions check an agent.Message against its wire shape, and each part against the wire
// form of the part its "type" names. A struct's names are strictjson.ExactFields: none required.
var strictOptions = &strictjson.Options{
	Shapes: map[reflect.Type]reflect.Type{reflect.TypeFor[agent.Message](): reflect.TypeFor[messageShape]()},
}

func init() {
	strictOptions.Hooks = map[reflect.Type]func([]byte, string) error{reflect.TypeFor[partShape](): checkPart}
}

// messageShape is the wire form of agent.Message (see its MarshalJSON), which UnmarshalStrict
// checks a message against in place of its loosely matching UnmarshalJSON.
type messageShape struct {
	Role  agent.Role  `json:"role"`
	Parts []partShape `json:"parts,omitempty"`
}

// partShape stands for one part of a message: the fields it may have depend on its "type".
type partShape struct{}

// partShapes maps each part "type" to the wire form of that part: the part's own fields and the
// "type" tag.
var partShapes = map[string]reflect.Type{
	"text": reflect.TypeFor[struct {
		Type string `json:"type"`
		agent.Text
	}](),
	"reasoning": reflect.TypeFor[struct {
		Type string `json:"type"`
		agent.Reasoning
	}](),
	"tool_use": reflect.TypeFor[struct {
		Type string `json:"type"`
		agent.ToolUse
	}](),
	"tool_result": reflect.TypeFor[struct {
		Type string `json:"type"`
		agent.ToolResult
	}](),
	"image": reflect.TypeFor[struct {
		Type string `json:"type"`
		agent.Image
	}](),
}

// checkPart checks one message part (raw, already checked for duplicate names) against the wire
// form of the part its "type" names.
func checkPart(raw []byte, path string) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("%s: a message part must be an object: %w", path, err)
	}
	var kind string
	if err := json.Unmarshal(probe["type"], &kind); err != nil {
		return fmt.Errorf("%s: a message part needs a string \"type\"", path)
	}
	shape, ok := partShapes[kind]
	if !ok {
		return fmt.Errorf("%s: unknown message part type %q", path, kind)
	}
	return strictjson.CheckValue(raw, shape, path, strictOptions)
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
