package audit

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/bide-ai/bide/agent"
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
	s := newStrictDecoder(data)
	if err := s.value(rv.Type().Elem(), "$", false); err != nil {
		return fmt.Errorf("audit: strict json: %w", err)
	}
	if _, err := s.dec.Token(); err != io.EOF {
		return errors.New("audit: strict json: trailing data after the value")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

var (
	unmarshalerType = reflect.TypeFor[json.Unmarshaler]()
	messageType     = reflect.TypeFor[agent.Message]()
	partShapeType   = reflect.TypeFor[partShape]()
)

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

// strictDecoder walks the tokens of data, which it keeps so it can read each token's raw spelling.
type strictDecoder struct {
	dec  *json.Decoder
	data []byte
}

func newStrictDecoder(data []byte) *strictDecoder {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return &strictDecoder{dec: dec, data: data}
}

// token reads the next token and returns it with its raw spelling in the input.
func (s *strictDecoder) token() (json.Token, []byte, error) {
	start := s.dec.InputOffset()
	tok, err := s.dec.Token()
	if err != nil {
		return nil, nil, err
	}
	return tok, trimSeparators(s.data[start:s.dec.InputOffset()]), nil
}

// trimSeparators drops the whitespace, name separator, and value separator that precede a token.
func trimSeparators(raw []byte) []byte {
	return bytes.TrimLeft(raw, " \t\r\n:,")
}

// value consumes one JSON value, checking it against t: an object decoded into a struct may use
// only that struct's exact JSON field names, each at most once; every other object (a map, raw
// JSON, a type with its own UnmarshalJSON, an interface) is checked for duplicate names only, at
// any depth. Strings decoded into Go must have one spelling (see UnmarshalStrict) unless verbatim
// is set: the value is raw JSON, kept as written.
func (s *strictDecoder) value(t reflect.Type, path string, verbatim bool) error {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == messageType {
		t = reflect.TypeFor[messageShape]()
	}
	if t == partShapeType {
		return s.part(path)
	}
	verbatim = verbatim || t == rawMessageType
	tok, raw, err := s.token()
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	delim, isDelim := tok.(json.Delim)
	if !isDelim {
		str, isString := tok.(string)
		if !isString || verbatim {
			return nil // another scalar: encoding/json checks its type when decoding
		}
		if err := oneSpelling(raw); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if t != nil && t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			if b, err := base64.StdEncoding.DecodeString(str); err == nil && base64.StdEncoding.EncodeToString(b) != str {
				return fmt.Errorf("%s: base64 %q is not the standard encoding of its bytes", path, str)
			}
		}
		return nil
	}
	custom := t == nil || t.Kind() == reflect.Interface ||
		reflect.PointerTo(t).Implements(unmarshalerType) || t.Implements(unmarshalerType)
	switch delim {
	case '[':
		var elem reflect.Type
		if !custom && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
			elem = t.Elem()
		}
		for i := 0; s.dec.More(); i++ {
			if err := s.value(elem, fmt.Sprintf("%s[%d]", path, i), verbatim); err != nil {
				return err
			}
		}
		_, err := s.dec.Token() // ]
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
		for s.dec.More() {
			kt, raw, err := s.token()
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			key := kt.(string)
			if seen[key] {
				return fmt.Errorf("%s: duplicate name %q", path, key)
			}
			seen[key] = true
			if !verbatim {
				if err := oneSpelling(raw); err != nil {
					return fmt.Errorf("%s: name %w", path, err)
				}
			}
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
			if err := s.value(next, path+"."+key, verbatim); err != nil {
				return err
			}
		}
		_, err := s.dec.Token() // }
		return err
	}
	return nil
}

// part consumes one message part, then checks it against the wire form of the part its "type"
// names.
func (s *strictDecoder) part(path string) error {
	start := s.dec.InputOffset()
	if err := s.value(nil, path, true); err != nil {
		return err
	}
	raw := trimSeparators(s.data[start:s.dec.InputOffset()])
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
	return newStrictDecoder(raw).value(shape, path, false)
}

// oneSpelling rejects a string literal (raw, with its quotes) that escapes a lone surrogate:
// encoding/json decodes every such escape to U+FFFD, so the literal would share its decoded value
// with other spellings. A surrogate pair, escaped high then low, is one character and allowed.
func oneSpelling(raw []byte) error {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) || raw[i] != 'u' {
			continue // a two-character escape
		}
		r := escapedUnit(raw, i-1)
		i += 4
		switch {
		case utf16.IsSurrogate(r) && r < 0xdc00: // high: a low surrogate must follow
			if low := escapedUnit(raw, i+1); low >= 0xdc00 && low <= 0xdfff {
				i += 6
				continue
			}
			return fmt.Errorf("escapes a lone surrogate (%U) in %s", r, raw)
		case utf16.IsSurrogate(r):
			return fmt.Errorf("escapes a lone surrogate (%U) in %s", r, raw)
		}
	}
	return nil
}

// escapedUnit returns the UTF-16 code unit escaped as \uXXXX at raw[at:], or -1.
func escapedUnit(raw []byte, at int) rune {
	if at+6 > len(raw) || raw[at] != '\\' || raw[at+1] != 'u' {
		return -1
	}
	u, err := strconv.ParseUint(string(raw[at+2:at+6]), 16, 16)
	if err != nil {
		return -1
	}
	return rune(u)
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
