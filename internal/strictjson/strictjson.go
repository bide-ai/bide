// Package strictjson decodes JSON so that what a reader sees in the text is exactly what the
// decoded value holds. encoding/json accepts a duplicate key and keeps the last value, matches
// keys case-insensitively, ignores unknown fields, and rewrites invalid UTF-8 and escaped lone
// surrogates to U+FFFD; each lets one text mean a value other than the one it shows. The checks
// here reject all of them, token by token against the target type, before encoding/json decodes.
//
// The audit package reads proof artifacts with it (audit.UnmarshalStrict) and the agent package
// reads tool arguments and typed answers with it, which also requires every field schema.For
// lists as required.
package strictjson

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/bide-ai/bide/internal/jsonfields"
)

// Field is one name a struct accepts: the type its value decodes into, and whether a document
// must include it.
type Field struct {
	Type     reflect.Type
	Required bool
}

// Options configure a check.
type Options struct {
	// Fields returns the names struct type t accepts, each exactly as spelled. Nil means
	// ExactFields.
	Fields func(t reflect.Type) map[string]Field
	// Shapes maps a type to the type its JSON is checked against instead: for a type whose own
	// UnmarshalJSON matches names loosely, its wire shape.
	Shapes map[reflect.Type]reflect.Type
	// Hooks maps a type to a check of its own: the value is consumed (checked for duplicate
	// names only) and its raw text, with the path to it, is handed to the hook.
	Hooks map[reflect.Type]func(raw []byte, path string) error
}

// ErrTrailingData reports data after the one JSON value.
var ErrTrailingData = errors.New("trailing data after the value")

// Unmarshal checks data against v's type (see Check) and decodes it into v with encoding/json,
// which then rejects any name the check let through that is not a field. Invalid UTF-8 anywhere
// in data is an error.
func Unmarshal(data []byte, v any, opts *Options) error {
	if !utf8.Valid(data) {
		return errors.New("invalid UTF-8")
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("decode target must be a non-nil pointer, got %T", v)
	}
	if err := Check(data, rv.Type().Elem(), opts); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

// Check consumes the one JSON value in data, checking it against t, and returns ErrTrailingData
// if anything but whitespace follows. An object decoded into a struct may use only that struct's
// exact JSON field names, each at most once, and must include each required one; every other
// object (a map, raw JSON, a type with its own UnmarshalJSON, an interface) is checked for
// duplicate names only, at any depth. Strings decoded into Go must have one spelling: no escaped
// lone surrogate, and in a []byte, base64 that is the standard encoding of its bytes. JSON kept
// verbatim (json.RawMessage) is checked for duplicate names only.
func Check(data []byte, t reflect.Type, opts *Options) error {
	s := newDecoder(data, opts)
	if err := s.value(t, "$", false); err != nil {
		return err
	}
	if _, err := s.dec.Token(); err != io.EOF {
		return ErrTrailingData
	}
	return nil
}

// CheckValue checks the first JSON value in raw against t, as Check does, naming path in its
// errors. It does not look past that value.
func CheckValue(raw []byte, t reflect.Type, path string, opts *Options) error {
	return newDecoder(raw, opts).value(t, path, false)
}

var (
	unmarshalerType = reflect.TypeFor[json.Unmarshaler]()
	rawMessageType  = reflect.TypeFor[json.RawMessage]()
)

// decoder walks the tokens of data, which it keeps so it can read each token's raw spelling.
type decoder struct {
	dec  *json.Decoder
	data []byte
	opts *Options
}

func newDecoder(data []byte, opts *Options) *decoder {
	if opts == nil {
		opts = &Options{}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return &decoder{dec: dec, data: data, opts: opts}
}

// token reads the next token and returns it with its raw spelling in the input.
func (s *decoder) token() (json.Token, []byte, error) {
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

func (s *decoder) fields(t reflect.Type) map[string]Field {
	if s.opts.Fields != nil {
		return s.opts.Fields(t)
	}
	return ExactFields(t)
}

// value consumes one JSON value, checking it against t (see Check). verbatim is set inside raw
// JSON, which is kept as written.
func (s *decoder) value(t reflect.Type, path string, verbatim bool) error {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if shape, ok := s.opts.Shapes[t]; ok && t != nil {
		t = shape
	}
	if hook, ok := s.opts.Hooks[t]; ok && t != nil {
		start := s.dec.InputOffset()
		if err := s.value(nil, path, true); err != nil {
			return err
		}
		return hook(trimSeparators(s.data[start:s.dec.InputOffset()]), path)
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
		var fields map[string]Field
		var mapElem reflect.Type
		if !custom {
			switch t.Kind() {
			case reflect.Struct:
				fields = s.fields(t)
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
				f, ok := fields[key]
				if !ok {
					return fmt.Errorf("%s: %q is not a field of %s (names must match exactly)", path, key, t)
				}
				next = f.Type
			case mapElem != nil:
				next = mapElem
			}
			if err := s.value(next, path+"."+key, verbatim); err != nil {
				return err
			}
		}
		if _, err := s.dec.Token(); err != nil { // }
			return err
		}
		var missing []string
		for name, f := range fields {
			if f.Required && !seen[name] {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			slices.Sort(missing)
			return fmt.Errorf("%s: missing required field %q", path, missing[0])
		}
		return nil
	}
	return nil
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

// SchemaFields returns the names struct type t accepts as schema.For describes them: the fields
// encoding/json reads (jsonfields.Of), each required exactly when the schema lists it as
// required. A type jsonfields cannot resolve accepts no names.
func SchemaFields(t reflect.Type) map[string]Field {
	fs, err := jsonfields.Of(t)
	if err != nil {
		return map[string]Field{}
	}
	out := make(map[string]Field, len(fs))
	for _, f := range fs {
		out[f.Name] = Field{Type: f.Field.Type, Required: f.Required()}
	}
	return out
}

// ExactFields returns the exact JSON names encoding/json decodes into struct type t, with each
// field's type, by encoding/json's own rules: exported fields by tag name or Go name (a "-" tag is
// skipped), and the fields of an untagged embedded struct promoted into t. When several fields
// claim one name, the shallowest wins, and at equal depth a tagged field wins over an untagged one.
// A name still claimed by several fields is one encoding/json ignores; it is kept here, and the
// decode that follows (DisallowUnknownFields) rejects it. No field is required.
func ExactFields(t reflect.Type) map[string]Field {
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
	out := map[string]Field{}
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
			out[name] = Field{Type: top[0].t}
		}
	}
	return out
}
