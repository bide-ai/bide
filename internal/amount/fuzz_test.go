package amount

import (
	"math/big"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// plainDecimal is the whole of what Parse accepts: an optional minus, an integer part with no
// leading zero, and an optional fraction of at least one digit.
var plainDecimal = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`)

// oracle says what Parse must return, by a route that shares no code with it: the scaled digits
// through strconv.ParseInt (the note's C1 statement) and, for the overflow side, math/big.
func oracle(t testing.TB, text string, unit Unit, exp int) (want int64, ok bool) {
	t.Helper()
	if !plainDecimal.MatchString(text) {
		return 0, false
	}
	neg := strings.HasPrefix(text, "-")
	body := strings.TrimPrefix(text, "-")
	intPart, frac, hasFrac := strings.Cut(body, ".")
	allowed := exp
	if unit == Minor {
		allowed = 0
	}
	if hasFrac && len(frac) > allowed {
		return 0, false
	}
	scale := exp
	if unit == Minor {
		scale = 0
	}
	digits := intPart + frac + strings.Repeat("0", scale-len(frac))
	if neg {
		digits = "-" + digits
	}
	n, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		t.Fatalf("oracle: %q is not digits", digits)
	}
	if !n.IsInt64() {
		return 0, false
	}
	v, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || v != n.Int64() {
		t.Fatalf("oracle: ParseInt(%q) = %d, %v; big says %v", digits, v, err, n)
	}
	return v, true
}

func checkAgainstOracle(t testing.TB, text string, unit Unit, exp int) {
	t.Helper()
	got, err := Parse(text, unit, exp)
	want, ok := oracle(t, text, unit, exp)
	switch {
	case ok && err != nil:
		t.Fatalf("Parse(%q, %v, %d) refused: %v; the scaled digits are %d", text, unit, exp, err, want)
	case ok && got != want:
		t.Fatalf("Parse(%q, %v, %d) = %d, want %d", text, unit, exp, got, want)
	case !ok && err == nil:
		t.Fatalf("Parse(%q, %v, %d) = %d; a literal the scaled digits through strconv.ParseInt would not accept", text, unit, exp, got)
	case !ok && got != 0:
		t.Fatalf("Parse(%q, %v, %d) returned %d with an error", text, unit, exp, got)
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"0", "1", "12.50", "-3.25", "1e3", "0x10", "+5", ".5", "5.", "007", "9223372036854775807",
		"92233720368547758.07", "92233720368547758.08", "-92233720368547758.08", "1.234", "1,5",
		"١", "NaN", "", " 1", "1.2.3", strings.Repeat("9", 40),
	} {
		for _, exp := range []uint8{0, 2, 3, 4} {
			f.Add(s, exp, false)
			f.Add(s, exp, true)
		}
	}
	f.Fuzz(func(t *testing.T, text string, exp uint8, minor bool) {
		unit := Major
		if minor {
			unit = Minor
		}
		e := int(exp % 19)
		checkAgainstOracle(t, text, unit, e)
	})
}

// TestParse_Randomized is the bounded form of FuzzParse that every CI run executes: a fixed seed,
// a fixed count, the same oracle. Generated texts are built from the characters a literal is made
// of, so a good share are well-formed and the rest are near-misses.
func TestParse_Randomized(t *testing.T) {
	const runs = 200000
	rng := rand.New(rand.NewPCG(4217, 18))
	alphabet := []string{"0", "1", "5", "9", "9", "9", ".", "-", "+", "e", "x", ",", " ", "_"}
	accepted := 0
	for i := 0; i < runs; i++ {
		var b strings.Builder
		for n := 1 + rng.IntN(24); n > 0; n-- {
			b.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		text := b.String()
		unit, exp := Major, rng.IntN(6)
		if rng.IntN(4) == 0 {
			unit = Minor
		}
		if i%2 == 0 { // well-formed, with a magnitude from 1 to 19 digits and a fraction around the exponent
			text = strconv.FormatInt(rng.Int64N(1<<62)>>rng.UintN(62), 10)
			if rng.IntN(5) == 0 {
				text = "-" + text
			}
			if rng.IntN(2) == 0 {
				text += "." + strings.Repeat("7", rng.IntN(exp+2))
				text = strings.TrimSuffix(text, ".")
			}
		}
		checkAgainstOracle(t, text, unit, exp)
		if _, err := Parse(text, unit, exp); err == nil {
			accepted++
		}
	}
	if accepted < runs/10 {
		t.Fatalf("only %d of %d generated texts were accepted; the generator no longer reaches the accepting paths", accepted, runs)
	}
}
