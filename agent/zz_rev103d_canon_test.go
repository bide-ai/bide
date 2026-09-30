package agent

import (
	"strings"
	"testing"
	"time"
)

// Distinct values must have distinct canonical forms; equal values one form.
func TestRev103d_CanonicalDistinctness(t *testing.T) {
	distinct := [][2]string{
		{`"\u00e9"`, `"e\u0301"`}, // NFC vs NFD: different characters
		{`1`, `"1"`},
		{`null`, `"null"`},
		{`[]`, `{}`},
		{`[1,2]`, `[2,1]`},
		{`{"a":1}`, `{"a":"1"}`},
		{`{"a\u0000":1}`, `{"a":1}`},
		{`"\u0000"`, `""`},
		{`9007199254740993`, `9007199254740992`},
		{`1e400`, `1e401`},
		{`0.1`, `0.10000000000000001`},
		{`{"a":{"b":1}}`, `{"a":{"b":1},"":null}`},
		{`["a,b"]`, `["a","b"]`},
		{`{"a":"b","c":"d"}`, `{"a":"b\",\"c\":\"d"}`},
		{`true`, `"true"`},
	}
	for _, p := range distinct {
		ca, ea := canonicalJSON(p[0])
		cb, eb := canonicalJSON(p[1])
		if ea != nil || eb != nil {
			t.Errorf("%s / %s: errors %v %v", p[0], p[1], ea, eb)
			continue
		}
		if ca == cb {
			t.Errorf("distinct values %s and %s share canonical form %s", p[0], p[1], ca)
		}
	}
	same := [][2]string{
		{`"\/"`, `"/"`},
		{`"\u002F"`, `"/"`},
		{`"\u003c"`, `"<"`},
		{`"\u0000"`, "\"\\u0000\""},
		{`"\uD83D\uDE00"`, "\"\U0001F600\""},
		{`1E+2`, `100`},
		{`-0`, `0`},
		{`0.000`, `0e99999`},
		{`{"b":1,"a":2}`, `{ "a" : 2 , "b" : 1 }`},
		{`12.3400e-2`, `0.1234`},
	}
	for _, p := range same {
		ca, ea := canonicalJSON(p[0])
		cb, eb := canonicalJSON(p[1])
		if ea != nil || eb != nil || ca != cb {
			t.Errorf("same value %s / %s: %q %v / %q %v", p[0], p[1], ca, ea, cb, eb)
		}
	}
}

// Every malformed text is refused.
func TestRev103d_CanonicalRefusesMalformed(t *testing.T) {
	bad := []string{
		``, ` `, `{`, `[1,]`, `[,1]`, `{"a":1,}`, `{,"a":1}`, `[1 2]`, `{"a" 1}`, `{"a",1}`,
		`{1:2}`, `[1:2]`, `01`, `1.`, `.5`, `+1`, `-`, `1e`, `1e+`, `"\x"`, `"\u12"`, `1 2`, `[1]]`,
		`{"a":1}}`, `nul`, `"a` + "\x00" + `"`, `{"a":1 "b":2}`, `[{]`, `[}`, `{]`, `"\uD800"`,
		`"\uDC00\uD800"`, `"\uD800\u0041"`, `{"a":1,"\u0061":2}`, "\"\xff\"", `NaN`, `Infinity`,
	}
	for _, s := range bad {
		if c, err := canonicalJSON(s); err == nil {
			t.Errorf("canonicalJSON(%q) = %q, nil; want refused", s, c)
		}
	}
}

// Deep nesting does not overflow the stack and is refused cleanly past the decoder's limit.
func TestRev103d_CanonicalDeepNesting(t *testing.T) {
	for _, depth := range []int{9999, 10001, 1_000_000} {
		s := strings.Repeat(`{"a":`, depth) + "1" + strings.Repeat("}", depth)
		start := time.Now()
		_, err := canonicalJSON(s)
		t.Logf("object depth %d: err=%v in %v", depth, err, time.Since(start))
		s = strings.Repeat(`[`, depth) + "1" + strings.Repeat("]", depth)
		start = time.Now()
		_, err = canonicalJSON(s)
		t.Logf("array depth %d: err=%v in %v", depth, err, time.Since(start))
	}
}

// The claim: the check is linear time, and huge exponents are exact.
func TestRev103d_CanonicalCostLinear(t *testing.T) {
	measure := func(name string, mk func(n int) string) {
		var prev time.Duration
		for _, n := range []int{50_000, 100_000, 200_000, 400_000} {
			s := mk(n)
			start := time.Now()
			if _, err := canonicalJSON(s); err != nil {
				t.Fatalf("%s n=%d: %v", name, n, err)
			}
			d := time.Since(start)
			ratio := 0.0
			if prev > 0 {
				ratio = float64(d) / float64(prev)
			}
			t.Logf("%s: %d bytes in %v (x%.1f for 2x input)", name, len(s), d, ratio)
			prev = d
		}
	}
	measure("exponent digits", func(n int) string { return "1e" + strings.Repeat("7", n) })
	measure("mantissa digits", func(n int) string { return strings.Repeat("7", n) })
	measure("nested objects", func(n int) string {
		depth := n / 6
		if depth > 9000 {
			depth = 9000
		}
		one := strings.Repeat(`{"a":`, depth) + "1" + strings.Repeat("}", depth)
		copies := n / len(one)
		if copies < 1 {
			copies = 1
		}
		return "[" + strings.Repeat(one+",", copies) + "1]"
	})
}
