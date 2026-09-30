package agent

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// canonicalNumber's doc: "The exponent is computed exactly whatever its size, in time linear in the
// text." Parsing and printing the exponent through math/big was quadratic in its digits. A timing
// ratio at these sizes (a few milliseconds) flakes, so the assertion is deterministic: bytes
// allocated per input byte stay constant as the exponent grows 4x (math/big's quadratic
// conversion allocated a growing multiple).
func TestRev103d_HugeExponentIsLinear(t *testing.T) {
	perByte := func(n int) float64 {
		s := "1e" + strings.Repeat("7", n)
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		start := time.Now()
		if _, err := canonicalJSON(s); err != nil {
			t.Fatal(err)
		}
		d := time.Since(start)
		runtime.ReadMemStats(&m1)
		t.Logf("exponent of %d digits: %v", n, d)
		return float64(m1.TotalAlloc-m0.TotalAlloc) / float64(len(s))
	}
	small, large := perByte(250_000), perByte(1_000_000)
	t.Logf("bytes allocated per input byte: 250k digits: %.1f; 1M digits: %.1f", small, large)
	if large > 2*small {
		t.Fatalf("allocation per input byte grows with the exponent's length (%.1f -> %.1f): not linear", small, large)
	}
}

// canonicalJSON's claimed linear cost: each nested object's canonical text is built in its own
// builder and copied into its parent's, so a depth-d nesting copies O(d^2) bytes. Bytes allocated
// per input byte should be a constant; here it grows with depth.
func TestRev103d_NestedObjectsAllocateLinearly(t *testing.T) {
	perByte := func(depth int) float64 {
		s := strings.Repeat(`{"a":`, depth) + "1" + strings.Repeat("}", depth)
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		if _, err := canonicalJSON(s); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&m1)
		return float64(m1.TotalAlloc-m0.TotalAlloc) / float64(len(s))
	}
	shallow, deep := perByte(1000), perByte(8000)
	t.Logf("bytes allocated per input byte: depth 1000: %.0f; depth 8000: %.0f", shallow, deep)
	if deep > 2*shallow {
		t.Fatalf("allocation per input byte grows with depth (%.0f -> %.0f): quadratic, not linear", shallow, deep)
	}
}
