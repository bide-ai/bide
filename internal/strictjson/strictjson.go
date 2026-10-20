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
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/bide-ai/bide/internal/jsonfields"
)

// Field is one name a struct accepts: the type its value decodes into, whether a document must
// include it, and whether it may be null.
type Field struct {
	Type     reflect.Type
	Required bool
	NullOK   bool
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
	// AllowUnknown lets an object decoded into a struct carry names the struct does not have, as a
	// document written by a later version does. Such a name must not be a case variant of a name
	// the struct has ("Kind" beside a "kind" field), which encoding/json would read into that
	// field; its value is checked like any value decoded into Go: no duplicate names at any depth
	// and no escaped lone surrogate.
	AllowUnknown bool
}

// MaxDepth is the deepest nesting of arrays and objects the structure of a document may have. The
// deepest document bide reads from its fixtures, examples, golden exports and test data nests 5
// levels (the journal records in agent/testdata), and the deepest any test reads is 7; 128 leaves
// an order of magnitude of headroom, and is the nesting past which a document is an attack, not
// data. JSON kept verbatim (json.RawMessage, as a journal keeps a tool call's input and a tool's
// result, whatever a model or a tool made it) is not structure bide defines, so it is held to
// MaxVerbatimDepth instead: a record carrying a deep tool input must stay readable.
const MaxDepth = 128

// MaxVerbatimDepth is the deepest nesting any document may have, structure and verbatim JSON
// together: about 10000, encoding/json's own limit (a verbatim value inside a journal record counts
// the record's own wrapper, so 9991 levels of one are read and 10000 are not). Check refuses a deeper document before any decoder
// runs, so no reader spends time or memory on it.
const MaxVerbatimDepth = 10000

// ErrTrailingData reports data after the one JSON value.
var ErrTrailingData = errors.New("trailing data after the value")

// Unmarshal checks data against v's type (see Check) and decodes it into v with encoding/json,
// which then rejects any name the check let through that is not a field (unless
// Options.AllowUnknown, when it ignores such names). Invalid UTF-8 anywhere
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
	if opts == nil || !opts.AllowUnknown {
		d.DisallowUnknownFields()
	}
	return d.Decode(v)
}

// Check consumes the one JSON value in data, checking it against t, and returns ErrTrailingData
// if anything but whitespace follows. An object decoded into a struct may use only that struct's
// exact JSON field names, each at most once, and must include each required one, with a value
// other than null unless the field takes null (Field.NullOK); every other
// object (a map, raw JSON, a type with its own UnmarshalJSON, an interface) is checked for
// duplicate names only, at any depth. Strings decoded into Go must have one spelling: no escaped
// lone surrogate, and in a []byte, base64 that is the standard encoding of its bytes. JSON kept
// verbatim (json.RawMessage) is checked for duplicate names only.
func Check(data []byte, t reflect.Type, opts *Options) error {
	if err := checkDepth(data, MaxVerbatimDepth); err != nil {
		return err
	}
	s := newDecoder(data, opts)
	if err := s.value(t, &path{name: "$"}, false); err != nil {
		return err
	}
	if _, err := s.dec.Token(); err != io.EOF {
		return ErrTrailingData
	}
	return nil
}

// CheckValue checks the first JSON value in raw against t, as Check does, naming path in its
// errors. It does not look past that value.
func CheckValue(raw []byte, t reflect.Type, at string, opts *Options) error {
	if err := checkDepth(raw, MaxVerbatimDepth); err != nil {
		return fmt.Errorf("%s: %w", at, err)
	}
	return newDecoder(raw, opts).value(t, &path{name: at}, false)
}

// checkDepth refuses data nested deeper than limit, scanning it once without allocating. A
// bracket inside a string does not count, and an escape inside a string is skipped whole, so a
// quote after a backslash does not end the string. Brackets are not paired here (the decoder
// rejects a malformed document): only the count of those open at once is bounded. The error names
// the limit and the offset of the bracket that crossed it, never the content.
func checkDepth(data []byte, limit int) error {
	depth := 0
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '"':
			for i++; i < len(data) && data[i] != '"'; i++ {
				if data[i] == '\\' {
					i++
				}
			}
		case '[', '{':
			if depth++; depth > limit {
				return errDepth(limit, i)
			}
		case ']', '}':
			depth--
		}
	}
	return nil
}

func errDepth(limit, offset int) error {
	return fmt.Errorf("nesting deeper than %d levels at offset %d", limit, offset)
}

// path is the position of a value in the document, kept as a chain from the value up to the root
// so that a path is built (String) only when an error or a hook needs it, not at every level.
type path struct {
	parent *path
	name   string // the root's text, an object's key, or empty for an array element
	index  int    // an array element's index
	elem   bool
}

func (p *path) String() string {
	var parts []*path
	for ; p != nil; p = p.parent {
		parts = append(parts, p)
	}
	var b strings.Builder
	for i := len(parts) - 1; i >= 0; i-- {
		switch q := parts[i]; {
		case q.parent == nil:
			b.WriteString(q.name)
		case q.elem:
			b.WriteString("[" + strconv.Itoa(q.index) + "]")
		default:
			b.WriteString("." + q.name)
		}
	}
	return b.String()
}

var (
	unmarshalerType = reflect.TypeFor[json.Unmarshaler]()
	rawMessageType  = reflect.TypeFor[json.RawMessage]()
	timeType        = reflect.TypeFor[time.Time]()
)

// decoder walks the tokens of data, which it keeps so it can read each token's raw spelling.
type decoder struct {
	dec  *json.Decoder
	data []byte
	opts *Options
	deep int // arrays and objects open around the value being read, verbatim ones not counted
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
func (s *decoder) value(t reflect.Type, at *path, verbatim bool) error {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if shape, ok := s.opts.Shapes[t]; ok && t != nil {
		t = shape
	}
	if hook, ok := s.opts.Hooks[t]; ok && t != nil {
		start := s.dec.InputOffset()
		if err := s.value(nil, at, true); err != nil {
			return err
		}
		return hook(trimSeparators(s.data[start:s.dec.InputOffset()]), at.String())
	}
	verbatim = verbatim || t == rawMessageType
	tok, raw, err := s.token()
	if err != nil {
		return fmt.Errorf("%s: %w", at, err)
	}
	delim, isDelim := tok.(json.Delim)
	if !isDelim {
		str, isString := tok.(string)
		if !isString || verbatim {
			return nil // another scalar: encoding/json checks its type when decoding
		}
		if err := oneSpelling(raw); err != nil {
			return fmt.Errorf("%s: %w", at, err)
		}
		if t != nil && t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			if b, err := base64.StdEncoding.DecodeString(str); err == nil && base64.StdEncoding.EncodeToString(b) != str {
				return fmt.Errorf("%s: base64 %s is not the standard encoding of its bytes", at, quoted(str))
			}
		}
		return nil
	}
	if !verbatim {
		if s.deep++; s.deep > MaxDepth {
			return errDepth(MaxDepth, int(s.dec.InputOffset())-1)
		}
		defer func() { s.deep-- }()
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
			if err := s.value(elem, &path{parent: at, index: i, elem: true}, verbatim); err != nil {
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
				return fmt.Errorf("%s: %w", at, err)
			}
			key := kt.(string)
			if seen[key] {
				return fmt.Errorf("%s: duplicate name %s", at, quoted(key))
			}
			seen[key] = true
			if !verbatim {
				if err := oneSpelling(raw); err != nil {
					return fmt.Errorf("%s: name %w", at, err)
				}
			}
			var next reflect.Type
			nullOK := true
			switch {
			case fields != nil:
				f, ok := fields[key]
				switch {
				case ok:
					next, nullOK = f.Type, f.NullOK
				case !s.opts.AllowUnknown:
					return fmt.Errorf("%s: %s is not a field of %s (names must match exactly)", at, quoted(key), t)
				default:
					for name := range fields {
						if strings.EqualFold(name, key) {
							return fmt.Errorf("%s: %s is a case variant of field %s of %s (names must match exactly)", at, quoted(key), quoted(name), t)
						}
					}
				}
			case mapElem != nil:
				next = mapElem
			}
			start := s.dec.InputOffset()
			if err := s.value(next, &path{parent: at, name: key}, verbatim); err != nil {
				return err
			}
			if !nullOK && bytes.Equal(trimSeparators(s.data[start:s.dec.InputOffset()]), []byte("null")) {
				return fmt.Errorf("%s: null for required field %s", at, quoted(key))
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
			return fmt.Errorf("%s: missing required field %s", at, quoted(missing[0]))
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
// required. A required field takes null only when its schema admits any value (For leaves an
// interface, json.RawMessage, or a json.Unmarshaler other than time.Time unconstrained);
// otherwise encoding/json would read null as the zero value, one the document never gave. An
// optional field takes null (OpenAI strict mode sends it). A type jsonfields cannot resolve
// accepts no names.
func SchemaFields(t reflect.Type) map[string]Field {
	fs, err := jsonfields.Of(t)
	if err != nil {
		return map[string]Field{}
	}
	out := make(map[string]Field, len(fs))
	for _, f := range fs {
		out[f.Name] = Field{Type: f.Field.Type, Required: f.Required(), NullOK: !f.Required() || admitsAny(f.Field.Type)}
	}
	return out
}

// admitsAny reports whether schema.For describes t as unconstrained ({}), a schema null meets.
func admitsAny(t reflect.Type) bool {
	switch {
	case t == timeType:
		return false
	case t == rawMessageType, t.Kind() == reflect.Interface:
		return true
	}
	return t.Implements(unmarshalerType) || reflect.PointerTo(t).Implements(unmarshalerType)
}

// ExactFields returns the exact JSON names encoding/json decodes into struct type t, with each
// field's type, by encoding/json's own rules: exported fields by tag name or Go name (a "-" tag is
// skipped), and the fields of an untagged embedded struct promoted into t. When several fields
// claim one name, the shallowest wins, and at equal depth a tagged field wins over an untagged one.
// A name still claimed by several fields is one encoding/json ignores; it is kept here, and the
// decode that follows (DisallowUnknownFields) rejects it. No field is required, and every field
// takes null.
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
			out[name] = Field{Type: top[0].t, NullOK: true}
		}
	}
	return out
}

// maxQuoted is the most of a document's text an error repeats: enough to name a field, and no more
// than a line. It bounds the size of a message; a short name is still quoted whole, which is what
// helps to find the mistake.
const maxQuoted = 64

// quoted is s as Go quotes it (so a control character or an escape sequence is written out, not sent
// to a terminal), cut to maxQuoted characters with "..." when longer. Errors name text from the
// document through it, never raw.
func quoted(s string) string {
	if utf8.RuneCountInString(s) > maxQuoted {
		cut := 0
		for i := range s {
			if cut == maxQuoted {
				return strconv.Quote(s[:i]) + "..."
			}
			cut++
		}
	}
	return strconv.Quote(s)
}
