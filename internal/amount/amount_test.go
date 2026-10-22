package amount

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestParse_Accepts(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		unit Unit
		exp  int
		want int64
	}{
		{"usd whole", "12", Major, 2, 1200},
		{"usd one fraction digit", "12.5", Major, 2, 1250},
		{"usd two fraction digits", "12.50", Major, 2, 1250},
		{"usd zero", "0", Major, 2, 0},
		{"usd one cent", "0.01", Major, 2, 1},
		{"usd zero point zero zero", "0.00", Major, 2, 0},
		{"negative", "-3.25", Major, 2, -325},
		{"negative zero is zero", "-0", Major, 2, 0},
		{"max int64 in cents", "92233720368547758.07", Major, 2, math.MaxInt64},
		{"min int64 in cents", "-92233720368547758.08", Major, 2, math.MinInt64},
		{"JPY has exponent 0", "500", Major, 0, 500},
		{"JPY large", "9223372036854775807", Major, 0, math.MaxInt64},
		{"KWD has exponent 3", "1.234", Major, 3, 1234},
		{"KWD short fraction", "1.5", Major, 3, 1500},
		{"KWD whole", "7", Major, 3, 7000},
		{"CLF has exponent 4", "0.0001", Major, 4, 1},
		{"scaled value just fits", "9223372036854775", Major, 3, 9223372036854775000},
		{"minor integer", "1250", Minor, 2, 1250},
		{"minor zero", "0", Minor, 3, 0},
		{"minor negative", "-7", Minor, 2, -7},
		{"minor max", "9223372036854775807", Minor, 0, math.MaxInt64},
		{"minor ignores the exponent", "1250", Minor, 0, 1250},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.text, tc.unit, tc.exp)
			if err != nil {
				t.Fatalf("Parse(%q, %v, %d): %v", tc.text, tc.unit, tc.exp, err)
			}
			if got != tc.want {
				t.Fatalf("Parse(%q, %v, %d) = %d, want %d", tc.text, tc.unit, tc.exp, got, tc.want)
			}
		})
	}
}

func TestParse_Refuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		unit Unit
		exp  int
		want error
	}{
		// Too many fraction digits, either side of the exponent boundary.
		{"usd three fraction digits", "12.505", Major, 2, ErrFraction},
		{"usd three zero fraction digits", "12.000", Major, 2, ErrFraction},
		{"JPY any fraction", "500.0", Major, 0, ErrFraction},
		{"JPY fraction digit", "1.5", Major, 0, ErrFraction},
		{"KWD four fraction digits", "1.2345", Major, 3, ErrFraction},
		{"CLF five fraction digits", "0.00001", Major, 4, ErrFraction},
		{"minor with a fraction", "12.50", Minor, 2, ErrFraction},
		{"minor with a zero fraction", "12.0", Minor, 2, ErrFraction},
		// Exponent form, hex, signs, and everything that is not a plain decimal.
		{"exponent", "1e3", Major, 2, ErrSyntax},
		{"upper exponent", "1E3", Major, 2, ErrSyntax},
		{"exponent with fraction", "1.5e1", Major, 2, ErrSyntax},
		{"negative exponent", "1e-2", Major, 2, ErrSyntax},
		{"minor exponent", "1e3", Minor, 2, ErrSyntax},
		{"hex", "0x10", Major, 2, ErrSyntax},
		{"hex digits", "0xff", Minor, 2, ErrSyntax},
		{"bare hex digits", "ff", Major, 2, ErrSyntax},
		{"binary", "0b11", Major, 2, ErrSyntax},
		{"octal", "0o7", Major, 2, ErrSyntax},
		{"leading plus", "+5", Major, 2, ErrSyntax},
		{"plus then minus", "+-5", Major, 2, ErrSyntax},
		{"double minus", "--5", Major, 2, ErrSyntax},
		{"minus alone", "-", Major, 2, ErrSyntax},
		{"NaN", "NaN", Major, 2, ErrSyntax},
		{"Infinity", "Infinity", Major, 2, ErrSyntax},
		{"inf", "-inf", Major, 2, ErrSyntax},
		{"empty", "", Major, 2, ErrSyntax},
		{"point alone", ".", Major, 2, ErrSyntax},
		{"no integer digits", ".5", Major, 2, ErrSyntax},
		{"negative no integer digits", "-.5", Major, 2, ErrSyntax},
		{"no fraction digits", "5.", Major, 2, ErrSyntax},
		{"two points", "1.2.3", Major, 2, ErrSyntax},
		{"leading zero", "007", Major, 2, ErrSyntax},
		{"double zero", "00", Major, 2, ErrSyntax},
		{"leading zero then fraction", "01.5", Major, 2, ErrSyntax},
		{"minor leading zero", "010", Minor, 2, ErrSyntax},
		{"leading space", " 5", Major, 2, ErrSyntax},
		{"trailing space", "5 ", Major, 2, ErrSyntax},
		{"trailing newline", "5\n", Major, 2, ErrSyntax},
		{"thousands comma", "1,000", Major, 2, ErrSyntax},
		{"decimal comma", "1,50", Major, 2, ErrSyntax},
		{"underscore", "1_000", Major, 2, ErrSyntax},
		{"arabic-indic digits", "١٢", Major, 2, ErrSyntax},
		{"fullwidth digit", "５", Major, 2, ErrSyntax},
		{"superscript digit", "²", Major, 2, ErrSyntax},
		{"currency symbol", "$5", Major, 2, ErrSyntax},
		{"unit suffix", "5USD", Major, 2, ErrSyntax},
		{"NUL", "5\x00", Major, 2, ErrSyntax},
		{"invalid utf-8", "5\xff", Major, 2, ErrSyntax},
		// Overflow, in the digits and in the scaling.
		{"max plus one", "92233720368547758.08", Major, 2, ErrOverflow},
		{"min minus one", "-92233720368547758.09", Major, 2, ErrOverflow},
		{"JPY max plus one", "9223372036854775808", Major, 0, ErrOverflow},
		{"minor max plus one", "9223372036854775808", Minor, 2, ErrOverflow},
		{"scaling overflows", "9223372036854776", Major, 3, ErrOverflow},
		{"huge", strings.Repeat("9", 100), Major, 2, ErrOverflow},
		{"huge with fraction", strings.Repeat("9", 100) + ".99", Major, 2, ErrOverflow},
		{"huge negative", "-" + strings.Repeat("9", 100), Minor, 2, ErrOverflow},
		{"very long is refused, not scanned", strings.Repeat("1", 1<<16), Major, 2, ErrSyntax},
		// An exponent the caller should never pass.
		{"negative exponent argument", "5", Major, -1, ErrExponent},
		{"exponent over 18", "5", Major, 19, ErrExponent},
		{"unknown unit", "5", Unit(0), 2, ErrSyntax},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.text, tc.unit, tc.exp)
			if err == nil {
				t.Fatalf("Parse(%.40q, %v, %d) = %d, want an error wrapping %v", tc.text, tc.unit, tc.exp, got, tc.want)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Parse(%.40q, %v, %d): %v, want an error wrapping %v", tc.text, tc.unit, tc.exp, err, tc.want)
			}
			if !errors.Is(err, ErrNotAssessable) {
				t.Fatalf("Parse(%.40q): %v does not wrap ErrNotAssessable", tc.text, err)
			}
			if got != 0 {
				t.Fatalf("Parse(%.40q) returned %d with an error; want 0", tc.text, got)
			}
			if len(err.Error()) > 200 {
				t.Fatalf("Parse(%.40q): the error is %d bytes; it must not echo a long input", tc.text, len(err.Error()))
			}
		})
	}
}

func TestParse_ExponentBoundary(t *testing.T) {
	// A fraction of exactly the exponent's length is accepted; one more digit is refused, for
	// every exponent a currency can have.
	for exp := 0; exp <= 18; exp++ {
		frac := strings.Repeat("1", exp)
		text := "1"
		if exp > 0 {
			text += "." + frac
		}
		if _, err := Parse(text, Major, exp); err != nil {
			t.Errorf("exp %d: Parse(%q) refused: %v", exp, text, err)
		}
		over := "1." + frac + "1"
		if _, err := Parse(over, Major, exp); !errors.Is(err, ErrFraction) {
			t.Errorf("exp %d: Parse(%q) = %v, want ErrFraction", exp, over, err)
		}
	}
}

func TestAssess(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		unit Unit
		exp  int
		want int64
		err  error
	}{
		{"number", `12.50`, Major, 2, 1250, nil},
		{"string", `"12.50"`, Major, 2, 1250, nil},
		{"integer number minor", `1250`, Minor, 2, 1250, nil},
		{"string minor", `"1250"`, Minor, 2, 1250, nil},
		{"zero", `0`, Major, 2, 0, nil},
		{"string with escaped digits", `"12"`, Major, 0, 12, nil},
		{"JPY", `500`, Major, 0, 500, nil},
		{"KWD", `"1.234"`, Major, 3, 1234, nil},
		{"negative number", `-5`, Major, 2, 0, ErrNegative},
		{"negative string", `"-5.00"`, Major, 2, 0, ErrNegative},
		{"negative zero number", `-0`, Major, 2, 0, ErrNegative},
		{"negative zero string", `"-0"`, Minor, 2, 0, ErrNegative},
		{"exponent number", `1e3`, Major, 2, 0, ErrSyntax},
		{"exponent string", `"1e3"`, Major, 2, 0, ErrSyntax},
		{"exponent with plus", `1E+2`, Major, 2, 0, ErrSyntax},
		{"too many fraction digits", `12.505`, Major, 2, 0, ErrFraction},
		{"minor with a fraction", `12.5`, Minor, 2, 0, ErrFraction},
		{"overflow number", `99999999999999999999`, Minor, 2, 0, ErrOverflow},
		{"overflow string", `"92233720368547758.08"`, Major, 2, 0, ErrOverflow},
		{"null", `null`, Major, 2, 0, ErrType},
		{"true", `true`, Major, 2, 0, ErrType},
		{"array", `[1]`, Major, 2, 0, ErrType},
		{"object", `{"amount":1}`, Major, 2, 0, ErrType},
		{"empty", ``, Major, 2, 0, ErrType},
		{"empty string", `""`, Major, 2, 0, ErrSyntax},
		{"word", `"abc"`, Major, 2, 0, ErrSyntax},
		{"padded string", `" 5"`, Major, 2, 0, ErrSyntax},
		{"padded number", ` 5`, Major, 2, 0, ErrType},
		{"lone surrogate", `"\ud800"`, Major, 2, 0, ErrSyntax},
		{"unterminated string", `"5`, Major, 2, 0, ErrType},
		{"hex string", `"0x10"`, Major, 2, 0, ErrSyntax},
		{"trailing data", `5 6`, Major, 2, 0, ErrType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Assess([]byte(tc.raw), tc.unit, tc.exp)
			if tc.err == nil {
				if err != nil || got != tc.want {
					t.Fatalf("Assess(%s) = %d, %v; want %d", tc.raw, got, err, tc.want)
				}
				return
			}
			if !errors.Is(err, tc.err) || !errors.Is(err, ErrNotAssessable) || got != 0 {
				t.Fatalf("Assess(%s) = %d, %v; want 0 and an error wrapping %v", tc.raw, got, err, tc.err)
			}
		})
	}
}

func TestAdd(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b int64
		want int64
		err  bool
	}{
		{"small", 1, 2, 3, false},
		{"zero", 0, 0, 0, false},
		{"to max", math.MaxInt64 - 1, 1, math.MaxInt64, false},
		{"over max", math.MaxInt64, 1, 0, true},
		{"over max both large", math.MaxInt64, math.MaxInt64, 0, true},
		{"half and half plus one", math.MaxInt64/2 + 1, math.MaxInt64/2 + 1, 0, true},
		{"to min", math.MinInt64 + 1, -1, math.MinInt64, false},
		{"under min", math.MinInt64, -1, 0, true},
		{"mixed signs never overflow", math.MaxInt64, math.MinInt64, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Add(tc.a, tc.b)
			if tc.err {
				if !errors.Is(err, ErrOverflow) || !errors.Is(err, ErrNotAssessable) || got != 0 {
					t.Fatalf("Add(%d, %d) = %d, %v; want an ErrOverflow", tc.a, tc.b, got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("Add(%d, %d) = %d, %v; want %d", tc.a, tc.b, got, err, tc.want)
			}
		})
	}
}
