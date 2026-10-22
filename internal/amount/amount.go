// Package amount reads money out of tool arguments for the amount caps of the untrusted-content
// and amount-caps note (docs/design/untrusted-content-and-amount-caps.md, sections 6.2 and 6.3):
// the text of an amount to an int64 count of minor units, the ISO 4217 exponent of a currency,
// the strict reading of a tool's arguments that precedes it, and the one-line grammar of a cap
// rule. It is pure: no engine, no journal, no clock, no I/O.
//
// Nothing here uses a floating-point type. An amount is the decimal text of a JSON number or
// string, scaled by the currency's exponent with checked int64 arithmetic; an overflow is an
// error, never a wrap. Anything the package cannot read with certainty is "not assessable"
// (ErrNotAssessable), and the engine gives a not-assessable amount the rule's own action, so a
// value the package refuses never silently passes a cap.
//
// Package location: internal/amount, in the core module, beside internal/strictjson, which it
// builds on. The agent package imports it without a cycle because amount imports only the
// standard library and internal/strictjson (and strictjson imports nothing of agent's). It is
// internal/ rather than agent/internal/ so that the cmd modules (a later `bide audit caps`,
// `bide doctor`) can parse a rule too: Go's internal rule is by import path, and every module of
// this repository lives under github.com/bide-ai/bide.
package amount

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
)

// ErrNotAssessable is wrapped by every error that means "this amount cannot be read with
// certainty": a text that is not a plain decimal, too many fraction digits, an overflow, a
// negative value, a value that is not a number or string, arguments that are not strict, or an
// argument that is absent.
var ErrNotAssessable = errors.New("amount not assessable")

// The specific reasons. Each wraps ErrNotAssessable and none echoes the input, which a model
// wrote.
var (
	// ErrSyntax: the text is not a plain decimal: optional minus, digits with no leading zero, and
	// at most one point with digits after it. An exponent, a plus sign, hex, NaN, a thousands
	// separator, whitespace and any non-ASCII digit are all refused.
	ErrSyntax = notAssessable("amount is not a plain decimal (optional minus, digits, one point; no exponent, plus sign, hex or leading zero)")
	// ErrFraction: more fraction digits than the currency's exponent (any, for minor units).
	ErrFraction = notAssessable("amount has more fraction digits than the currency's exponent")
	// ErrOverflow: the amount, scaled to minor units, or a sum, does not fit an int64.
	ErrOverflow = notAssessable("amount overflows int64")
	// ErrNegative: the amount is negative (or written with a minus sign).
	ErrNegative = notAssessable("amount is negative")
	// ErrType: the value is not a JSON number or string.
	ErrType = notAssessable("amount is not a JSON number or string")
	// ErrExponent: an exponent outside 0 to 18, or one that contradicts the ISO 4217 table.
	ErrExponent = notAssessable("currency exponent must be 0 to 18 and agree with the ISO 4217 table")
)

func notAssessable(msg string) error { return &reason{msg} }

type reason struct{ msg string }

func (r *reason) Error() string { return r.msg }
func (r *reason) Unwrap() error { return ErrNotAssessable }

// Unit says how a tool's argument is written.
type Unit uint8

const (
	// Minor: an integer count of the currency's minor units (cents).
	Minor Unit = iota + 1
	// Major: a decimal in major units (dollars), with at most the currency's exponent of
	// fractional digits.
	Major
)

func (u Unit) String() string {
	switch u {
	case Minor:
		return "minor"
	case Major:
		return "major"
	}
	return "unit(?)"
}

const (
	// MaxExponent is the largest exponent accepted, from the table or from an exponent=
	// override: 10^18 is the largest power of ten an int64 holds.
	MaxExponent = 18
	// maxText bounds the text Parse reads. A valid amount is at most 39 bytes (a sign, 19 digits,
	// a point, 18 fraction digits), so nothing legitimate is refused, and a hostile one is never
	// scanned in full.
	maxText = 256
)

// Parse reads the decimal text of an amount to an int64 count of minor units. For Major, a
// fraction of at most exp digits is allowed and the result is scaled by 10^exp; for Minor the
// text is an integer and exp is not used. The result is signed: a minus sign is parsed, not
// refused, so the caller (Assess does) decides what a negative amount means.
//
// The accepted text is exactly: an optional '-', then "0" or a digit 1 to 9 followed by digits,
// then optionally '.' and one or more digits. Parse never uses a float and never accepts a
// literal that strconv.ParseInt over the scaled digits would not (the note's C1).
func Parse(text string, unit Unit, exp int) (int64, error) {
	if exp < 0 || exp > MaxExponent {
		return 0, ErrExponent
	}
	if unit != Minor && unit != Major {
		return 0, ErrSyntax
	}
	if len(text) > maxText {
		return 0, ErrSyntax
	}
	s := text
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	n := digitsAt(s)
	intPart := s[:n]
	if n == 0 || (n > 1 && intPart[0] == '0') {
		return 0, ErrSyntax
	}
	s = s[n:]
	frac := ""
	if strings.HasPrefix(s, ".") {
		s = s[1:]
		n = digitsAt(s)
		if n == 0 {
			return 0, ErrSyntax
		}
		frac, s = s[:n], s[n:]
	}
	if s != "" {
		return 0, ErrSyntax
	}
	scale := 0
	if unit == Major {
		scale = exp
	}
	if len(frac) > scale {
		return 0, ErrFraction
	}
	// The value is intPart, frac, then zeros up to the scale; accumulate it as a magnitude
	// bounded by what the sign allows, so that math.MinInt64 is reachable and nothing wraps.
	limit := uint64(math.MaxInt64)
	if neg {
		limit++
	}
	var acc uint64
	push := func(d byte) bool {
		if acc > (limit-uint64(d))/10 {
			return false
		}
		acc = acc*10 + uint64(d)
		return true
	}
	for i := 0; i < len(intPart); i++ {
		if !push(intPart[i] - '0') {
			return 0, ErrOverflow
		}
	}
	for i := 0; i < len(frac); i++ {
		if !push(frac[i] - '0') {
			return 0, ErrOverflow
		}
	}
	for i := len(frac); i < scale; i++ {
		if !push(0) {
			return 0, ErrOverflow
		}
	}
	if neg {
		return int64(-acc), nil // acc <= 1<<63; int64(-acc) is exact, MinInt64 included
	}
	return int64(acc), nil
}

// digitsAt is the length of the run of ASCII digits at the start of s.
func digitsAt(s string) int {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return n
}

// Assess reads one JSON value, a number or a string as it stands in the arguments (Lookup returns
// it verbatim), to a non-negative count of minor units. The number is read from its text, never
// converted through a float, so 12.50 is "12.50". A negative amount (including -0) is
// ErrNegative, an exponent form, a value that is neither a number nor a string, and anything
// else Parse refuses, is an error: the caller gives the rule's action to each.
func Assess(raw []byte, unit Unit, exp int) (int64, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return 0, ErrType
	}
	var text string
	switch c := raw[0]; {
	case c == '"':
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0, ErrType
		}
	case c == '-' || (c >= '0' && c <= '9'):
		text = string(raw)
	default:
		return 0, ErrType
	}
	v, err := Parse(text, unit, exp)
	if err != nil {
		return 0, err
	}
	if strings.HasPrefix(text, "-") {
		return 0, ErrNegative
	}
	return v, nil
}

// Add is a + b in int64 with an overflow check. A sum of reservations that does not fit is
// ErrOverflow, a refusal, never a wrap.
func Add(a, b int64) (int64, error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, ErrOverflow
	}
	return a + b, nil
}
