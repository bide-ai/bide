package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The one rule for comparing JSON values the engine and package plan record: two JSON texts are
// the same value if and only if their canonical forms (canonicalJSON) are equal. It is blind to
// whitespace, object key order, a number's spelling and a string's escaping (so a value json.Marshal
// wrote with HTML escapes is the value marshalJournal wrote without them), and it refuses the texts
// on which "the same value" is ambiguous: invalid UTF-8, a lone surrogate escape, and an object with
// a repeated key.

// errNotCanonical is the condition of a JSON text canonicalJSON refuses.
var errNotCanonical = errors.New("not canonical JSON")

// equalJSON reports whether a and b are the same JSON value (see canonicalJSON). A text
// canonicalJSON refuses is an error wrapping errNotCanonical.
func equalJSON(a, b []byte) (bool, error) {
	ca, err := canonicalJSON(string(a))
	if err != nil {
		return false, err
	}
	cb, err := canonicalJSON(string(b))
	if err != nil {
		return false, err
	}
	return ca == cb, nil
}

// sameJSON reports whether a and b are the same JSON value (see canonicalJSON), and, for a text
// canonicalJSON refuses, whether a and b are the same bytes: such a text equals only itself.
func sameJSON(a, b []byte) bool {
	same, err := equalJSON(a, b)
	if err != nil {
		return bytes.Equal(a, b)
	}
	return same
}

// sameCanonicalJSON is sameJSON for texts held as strings.
func sameCanonicalJSON(a, b string) bool { return sameJSON([]byte(a), []byte(b)) }

// canonicalJSON re-encodes the JSON text s canonically: objects with their keys sorted, no
// insignificant whitespace, strings as the characters they denote (encoded one way), and every
// number as the exact decimal value it denotes (see canonicalNumber), so 1, 1.0 and 1e0 are one
// value while 2^53 and 2^53+1 are two: no number is rounded through a float. It refuses (an error
// wrapping errNotCanonical) text that is not one JSON value, invalid UTF-8, a string escape of a
// lone surrogate, and an object with a repeated key: each would let two different texts decode to
// one value.
//
// Its cost is linear in the text (plus sorting each object's keys): the value is read once into a
// tree whose scalars are spans of one shared buffer, and written out once, so no byte is copied
// once per level of nesting.
func canonicalJSON(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%w: invalid UTF-8", errNotCanonical)
	}
	if err := checkSurrogates(s); err != nil {
		return "", err
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	c := canonicalizer{dec: dec, scalars: make([]byte, 0, len(s))}
	root, err := c.value()
	if err != nil {
		return "", err
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", fmt.Errorf("%w: trailing data after the value", errNotCanonical)
	}
	var b strings.Builder
	b.Grow(len(c.scalars) + len(s))
	c.write(&b, root)
	return b.String(), nil
}

// checkSurrogates refuses a \u escape of a surrogate that is not a high surrogate followed by an
// escaped low one: the decoder turns a lone surrogate into U+FFFD, the same as the character itself.
// A backslash is valid JSON only inside a string, and the text's syntax is checked by the decoder.
func checkSurrogates(s string) error {
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			continue
		}
		if s[i+1] != 'u' {
			i++ // a two-character escape
			continue
		}
		r, ok := hex4(s, i+2)
		if !ok {
			return nil // not a valid escape: the decoder refuses the text
		}
		switch {
		case r >= 0xD800 && r <= 0xDBFF:
			lo, ok := hex4(s, i+8)
			if !ok || s[i+6] != '\\' || s[i+7] != 'u' || lo < 0xDC00 || lo > 0xDFFF {
				return fmt.Errorf("%w: a lone surrogate \\u%04X", errNotCanonical, r)
			}
			i += 11
		case r >= 0xDC00 && r <= 0xDFFF:
			return fmt.Errorf("%w: a lone surrogate \\u%04X", errNotCanonical, r)
		default:
			i += 5
		}
	}
	return nil
}

// hex4 reads the four hex digits at s[i:i+4].
func hex4(s string, i int) (rune, bool) {
	if i+4 > len(s) {
		return 0, false
	}
	v, err := strconv.ParseUint(s[i:i+4], 16, 16)
	return rune(v), err == nil
}

// canonicalizer reads one JSON value into a tree: a scalar is the span of its canonical text in
// scalars, an array its elements, and an object its members, sorted by key once the object ends.
type canonicalizer struct {
	dec     *json.Decoder
	scalars []byte
	depth   int // arrays and objects open around the value being read
}

// maxCanonicalDepth bounds the nesting canonicalJSON reads, as encoding/json's own decoder does:
// deeper text is refused rather than read by a recursion as deep as the text asks.
const maxCanonicalDepth = 10000

type canonicalNode struct {
	kind       byte // 's' a scalar, '[' an array, '{' an object
	start, end int  // a scalar's canonical text: scalars[start:end]
	kids       []canonicalNode
	keys       []string // an object's keys, in the order of kids
}

// value reads the next JSON value.
func (c *canonicalizer) value() (canonicalNode, error) {
	tok, err := c.dec.Token()
	if err != nil {
		return canonicalNode{}, fmt.Errorf("%w: %w", errNotCanonical, err)
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '{' || t == '[' {
			if c.depth++; c.depth > maxCanonicalDepth {
				return canonicalNode{}, fmt.Errorf("%w: exceeded max depth %d", errNotCanonical, maxCanonicalDepth)
			}
			defer func() { c.depth-- }()
		}
		switch t {
		case '{':
			n := canonicalNode{kind: '{'}
			seen := map[string]bool{}
			for c.dec.More() {
				kt, err := c.dec.Token()
				if err != nil {
					return canonicalNode{}, fmt.Errorf("%w: %w", errNotCanonical, err)
				}
				k := kt.(string) // the decoder yields a string key inside an object
				if seen[k] {
					return canonicalNode{}, fmt.Errorf("%w: the key %q is repeated", errNotCanonical, k)
				}
				seen[k] = true
				v, err := c.value()
				if err != nil {
					return canonicalNode{}, err
				}
				n.keys = append(n.keys, k)
				n.kids = append(n.kids, v)
			}
			if _, err := c.dec.Token(); err != nil { // '}'
				return canonicalNode{}, fmt.Errorf("%w: %w", errNotCanonical, err)
			}
			sort.Sort(byKey{&n})
			return n, nil
		case '[':
			n := canonicalNode{kind: '['}
			for c.dec.More() {
				v, err := c.value()
				if err != nil {
					return canonicalNode{}, err
				}
				n.kids = append(n.kids, v)
			}
			if _, err := c.dec.Token(); err != nil { // ']'
				return canonicalNode{}, fmt.Errorf("%w: %w", errNotCanonical, err)
			}
			return n, nil
		default:
			return canonicalNode{}, fmt.Errorf("%w: unexpected %v", errNotCanonical, t)
		}
	case string:
		return c.scalar(func(b []byte) []byte { return appendCanonicalString(b, t) }), nil
	case json.Number:
		return c.scalar(func(b []byte) []byte { return append(b, canonicalNumber(string(t))...) }), nil
	case bool:
		return c.scalar(func(b []byte) []byte { return strconv.AppendBool(b, t) }), nil
	case nil:
		return c.scalar(func(b []byte) []byte { return append(b, "null"...) }), nil
	default:
		return canonicalNode{}, fmt.Errorf("%w: unexpected token %T", errNotCanonical, tok)
	}
}

// scalar appends a scalar's canonical text to the shared buffer and returns its span.
func (c *canonicalizer) scalar(appendText func([]byte) []byte) canonicalNode {
	start := len(c.scalars)
	c.scalars = appendText(c.scalars)
	return canonicalNode{kind: 's', start: start, end: len(c.scalars)}
}

// write writes n's canonical text to b.
func (c *canonicalizer) write(b *strings.Builder, n canonicalNode) {
	switch n.kind {
	case 's':
		b.Write(c.scalars[n.start:n.end])
	case '[':
		b.WriteByte('[')
		for i, k := range n.kids {
			if i > 0 {
				b.WriteByte(',')
			}
			c.write(b, k)
		}
		b.WriteByte(']')
	case '{':
		b.WriteByte('{')
		for i, k := range n.kids {
			if i > 0 {
				b.WriteByte(',')
			}
			b.Write(appendCanonicalString(nil, n.keys[i]))
			b.WriteByte(':')
			c.write(b, k)
		}
		b.WriteByte('}')
	}
}

// byKey sorts an object's members by key.
type byKey struct{ n *canonicalNode }

func (s byKey) Len() int           { return len(s.n.keys) }
func (s byKey) Less(i, j int) bool { return s.n.keys[i] < s.n.keys[j] }
func (s byKey) Swap(i, j int) {
	s.n.keys[i], s.n.keys[j] = s.n.keys[j], s.n.keys[i]
	s.n.kids[i], s.n.kids[j] = s.n.kids[j], s.n.kids[i]
}

// appendCanonicalString appends s as a JSON string, escaped one way: marshalJournal's.
func appendCanonicalString(b []byte, s string) []byte {
	eb, _ := marshalJournal(s) // a valid UTF-8 string always encodes
	return append(b, eb...)
}

// canonicalNumber writes the JSON number text t (valid JSON number grammar) as its exact decimal
// value: "0" for zero, and otherwise an optional "-", the significant digits with no leading or
// trailing zero, "e", and the exponent that makes them the value. Two number texts denote the
// same decimal value if and only if their canonical forms are equal. The exponent is computed
// exactly whatever its size, in time linear in the text: the offset added to it (the fraction's
// length and the trailing zeros dropped) is at most the text's length, so it is added digit by
// digit to the exponent's decimal text.
func canonicalNumber(t string) string {
	neg := strings.HasPrefix(t, "-")
	mant, expText, hasExp := strings.Cut(strings.TrimPrefix(t, "-"), "e")
	if !hasExp {
		mant, expText, hasExp = strings.Cut(mant, "E")
	}
	intPart, frac, _ := strings.Cut(mant, ".")
	digits := strings.TrimLeft(intPart+frac, "0")
	if digits == "" {
		return "0" // every zero, whatever its sign or exponent
	}
	trimmed := strings.TrimRight(digits, "0")
	offset := int64(len(digits)-len(trimmed)) - int64(len(frac))
	if !hasExp {
		expText = "0"
	}
	sign := ""
	if neg {
		sign = "-"
	}
	return sign + trimmed + "e" + addExponent(expText, offset)
}

// addExponent returns the decimal text of e+d, where e is a JSON exponent's text (optional sign,
// then digits, maybe with leading zeros) and |d| is small (it fits an int64). It costs time linear
// in e's text: e is parsed as an int64 when it fits, and otherwise d is added to (or subtracted
// from) e's digits from the right, since |e| >= 10^18 > |d| fixes the result's sign as e's.
func addExponent(e string, d int64) string {
	neg := false
	switch {
	case strings.HasPrefix(e, "-"):
		neg, e = true, e[1:]
	case strings.HasPrefix(e, "+"):
		e = e[1:]
	}
	e = strings.TrimLeft(e, "0")
	if len(e) <= 18 {
		v, _ := strconv.ParseInt("0"+e, 10, 64) // at most 18 digits: always fits
		if neg {
			v = -v
		}
		return strconv.FormatInt(v+d, 10)
	}
	// |e| >= 10^18 > |d|: the result has e's sign, and its magnitude is |e| + d (e positive) or
	// |e| - d (e negative).
	add := (d >= 0) != neg
	m := d
	if m < 0 {
		m = -m
	}
	digits := []byte(e)
	out := ""
	if add {
		carry := m
		for i := len(digits) - 1; i >= 0 && carry > 0; i-- {
			v := int64(digits[i]-'0') + carry%10
			carry /= 10
			if v > 9 {
				v -= 10
				carry++
			}
			digits[i] = byte('0' + v)
		}
		if carry > 0 { // a carry out of the top digit
			out = strconv.FormatInt(carry, 10)
		}
		out += string(digits)
	} else {
		borrow := m
		for i := len(digits) - 1; i >= 0 && borrow > 0; i-- {
			v := int64(digits[i]-'0') - borrow%10
			borrow /= 10
			if v < 0 {
				v += 10
				borrow++
			}
			digits[i] = byte('0' + v)
		}
		out = strings.TrimLeft(string(digits), "0") // |e| > m: nothing is left to borrow
	}
	if neg {
		out = "-" + out
	}
	return out
}
