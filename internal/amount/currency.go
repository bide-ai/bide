package amount

import (
	"errors"
	"fmt"
)

// ErrUnknownCurrency: the code is not in the exponent table (and no exponent= was stated for
// it), or is not three upper-case ASCII letters. It is a configuration error, found when a rule
// is registered; it is not ErrNotAssessable.
var ErrUnknownCurrency = errors.New("unknown currency code")

// Exponent returns the ISO 4217 exponent (the number of minor-unit digits) of a currency code. A
// code is exactly three upper-case ASCII letters; "usd" is not "USD". An unknown code, and a code
// the published list gives no minor unit (XAU, XXX and the like), is ErrUnknownCurrency.
func Exponent(code string) (int, error) {
	if n, ok := exponents[code]; ok {
		return n, nil
	}
	return 0, fmt.Errorf("%w %.12q", ErrUnknownCurrency, code)
}

// Resolve is the exponent a rule uses for code: the table's, or the one stated with exponent=
// (override non-nil) for a code the table lacks. An override that contradicts the table is
// ErrExponent, as is one outside 0 to MaxExponent, so a rule cannot quietly change how many
// minor units a dollar has. An override for a code that is not three upper-case letters is
// ErrUnknownCurrency.
func Resolve(code string, override *int) (int, error) {
	if override == nil {
		return Exponent(code)
	}
	if *override < 0 || *override > MaxExponent {
		return 0, fmt.Errorf("%w: exponent=%d", ErrExponent, *override)
	}
	if !isCode(code) {
		return 0, fmt.Errorf("%w %.12q", ErrUnknownCurrency, code)
	}
	if n, ok := exponents[code]; ok && n != *override {
		return 0, fmt.Errorf("%w: exponent=%d, but the table says %d for %s", ErrExponent, *override, n, code)
	}
	return *override, nil
}

// Exponents is a copy of the ISO 4217 exponent table: every code with its number of minor-unit digits. The
// approve page carries the same table to write an amount as a decimal, and a test holds the two equal.
func Exponents() map[string]int {
	out := make(map[string]int, len(exponents))
	for k, v := range exponents {
		out[k] = v
	}
	return out
}

// isCode reports whether s is three upper-case ASCII letters.
func isCode(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}
