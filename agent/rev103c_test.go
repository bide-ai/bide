package agent

// Adversarial review of PR #103's F2 (exact decimal comparison of flow inputs).

import (
	"strings"
	"testing"
	"time"
)

// F2: canonicalNumber documents "Two number texts denote the same decimal value if and only if
// their canonical forms are equal." A number whose exponent is beyond +-2^62 keeps its text, so
// spellings of one value compare unequal: 0e<huge> is zero but not 0, and 1E<huge> is not 1e<huge>.
func TestRev103c_F2_HugeExponentSpellingsOfOneValue(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{`0e4611686018427387905`, `0`},
		{`0e99999999999999999999`, `0`},
		{`1E4611686018427387905`, `1e4611686018427387905`},
		{`1e+4611686018427387905`, `1e4611686018427387905`},
		{`1.0e4611686018427387905`, `1e4611686018427387905`},
		{`10e4611686018427387905`, `1e4611686018427387906`},
		{`1e-4611686018427387905`, `0.1e-4611686018427387904`},
	} {
		if !sameCanonicalJSON(tc.a, tc.b) {
			ca, _ := canonicalJSON(tc.a)
			cb, _ := canonicalJSON(tc.b)
			t.Errorf("%s and %s are one decimal value, but compare unequal (%s vs %s)", tc.a, tc.b, ca, cb)
		}
	}
}

// F2: no two different values share a canonical form, across the grammar's corners.
func TestRev103c_F2_DistinctValuesStayDistinct(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{`9007199254740993`, `9007199254740992`},
		{`[1,[2,{"k":9007199254740993}]]`, `[1,[2,{"k":9007199254740992}]]`},
		{`1e4611686018427387904`, `1e4611686018427387903`},
		{`-0.5`, `0.5`},
		{`1` + strings.Repeat("0", 400) + `1`, `1` + strings.Repeat("0", 401)},
		{`1e-4611686018427387904`, `0`},
	} {
		if sameCanonicalJSON(tc.a, tc.b) {
			t.Errorf("%.60s and %.60s are different values but compare equal", tc.a, tc.b)
		}
	}
	// Non-JSON spellings the grammar refuses compare as text, never as the number they suggest.
	for _, bad := range []string{`+1`, `.5`, `5.`, `0x10`, `NaN`, `Infinity`, `-Infinity`, `01`} {
		if sameCanonicalJSON(bad, `1`) || sameCanonicalJSON(bad, `0.5`) || sameCanonicalJSON(bad, `5`) || sameCanonicalJSON(bad, `16`) {
			t.Errorf("%s compared equal to a number", bad)
		}
	}
}

// F2: cost is linear in the text: a 4 MB digit string and a 1e999999999 exponent are cheap.
func TestRev103c_F2_NoBlowup(t *testing.T) {
	long := `1` + strings.Repeat("0", 4<<20)
	start := time.Now()
	if !sameCanonicalJSON(long, `1e`+"4194304") {
		t.Error("1 followed by 4Mi zeros is 1e4194304")
	}
	if !sameCanonicalJSON(`1e999999999`, `10e999999998`) {
		t.Error("1e999999999 is 10e999999998")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v", d)
	}
}

// F2 (beyond numbers): the canonical form decodes strings, so a lone surrogate escape becomes
// U+FFFD, and a duplicate key keeps its last value. Two different JSON texts then compare equal.
// A flow whose input type is json.RawMessage (or that forwards the raw text) sees different bytes.
func TestRev103c_F2_DistinctTextsCollapse(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{`"\ud800"`, `"\udfff"`},
		{`"\ud800"`, `"�"`},
		{`{"a":1,"a":2}`, `{"a":2}`},
	} {
		if sameCanonicalJSON(tc.a, tc.b) {
			t.Errorf("%s and %s are different JSON texts but compare equal", tc.a, tc.b)
		}
	}
}
