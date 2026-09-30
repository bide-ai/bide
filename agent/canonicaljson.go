package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"slices"
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
func canonicalJSON(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%w: invalid UTF-8", errNotCanonical)
	}
	if err := checkSurrogates(s); err != nil {
		return "", err
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var b strings.Builder
	if err := writeCanonical(dec, &b); err != nil {
		return "", err
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", fmt.Errorf("%w: trailing data after the value", errNotCanonical)
	}
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

// writeCanonical writes the next JSON value dec yields to b, canonically (see canonicalJSON).
func writeCanonical(dec *json.Decoder, b *strings.Builder) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %w", errNotCanonical, err)
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			members := map[string]string{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return fmt.Errorf("%w: %w", errNotCanonical, err)
				}
				k := kt.(string) // the decoder yields a string key inside an object
				if _, dup := members[k]; dup {
					return fmt.Errorf("%w: the key %q is repeated", errNotCanonical, k)
				}
				var vb strings.Builder
				if err := writeCanonical(dec, &vb); err != nil {
					return err
				}
				members[k] = vb.String()
			}
			if _, err := dec.Token(); err != nil { // '}'
				return fmt.Errorf("%w: %w", errNotCanonical, err)
			}
			keys := slices.Sorted(func(yield func(string) bool) {
				for k := range members {
					if !yield(k) {
						return
					}
				}
			})
			b.WriteByte('{')
			for i, k := range keys {
				if i > 0 {
					b.WriteByte(',')
				}
				writeCanonicalString(b, k)
				b.WriteByte(':')
				b.WriteString(members[k])
			}
			b.WriteByte('}')
		case '[':
			b.WriteByte('[')
			for i := 0; dec.More(); i++ {
				if i > 0 {
					b.WriteByte(',')
				}
				if err := writeCanonical(dec, b); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // ']'
				return fmt.Errorf("%w: %w", errNotCanonical, err)
			}
			b.WriteByte(']')
		default:
			return fmt.Errorf("%w: unexpected %v", errNotCanonical, t)
		}
	case string:
		writeCanonicalString(b, t)
	case json.Number:
		b.WriteString(canonicalNumber(string(t)))
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case nil:
		b.WriteString("null")
	default:
		return fmt.Errorf("%w: unexpected token %T", errNotCanonical, tok)
	}
	return nil
}

// writeCanonicalString writes s as a JSON string, escaped one way: marshalJournal's.
func writeCanonicalString(b *strings.Builder, s string) {
	eb, _ := marshalJournal(s) // a valid UTF-8 string always encodes
	b.Write(eb)
}

// canonicalNumber writes the JSON number text t (valid JSON number grammar) as its exact decimal
// value: "0" for zero, and otherwise an optional "-", the significant digits with no leading or
// trailing zero, "e", and the exponent that makes them the value. Two number texts denote the
// same decimal value if and only if their canonical forms are equal. The exponent is computed
// exactly whatever its size, and the cost is linear in the text.
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
	exp := new(big.Int)
	if hasExp {
		if _, ok := exp.SetString(expText, 10); !ok { // SetString takes a leading "+" or "-"
			return t // not a JSON number: the decoder refuses the text before this
		}
	}
	exp.Sub(exp, big.NewInt(int64(len(frac))))
	exp.Add(exp, big.NewInt(int64(len(digits)-len(trimmed))))
	sign := ""
	if neg {
		sign = "-"
	}
	return sign + trimmed + "e" + exp.String()
}
