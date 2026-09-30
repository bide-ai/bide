package agent

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// canonicalNumber's doc: "The exponent is computed exactly whatever its size, and the cost is
// linear in the text." Parsing and printing the exponent through math/big is quadratic in its
// digits: 4x the digits costs about 16x the time. Linear cost would be about 4x; 8x is the bar.
func TestRev103d_HugeExponentIsLinear(t *testing.T) {
	cost := func(n int) time.Duration {
		s := "1e" + strings.Repeat("7", n)
		best := time.Duration(1 << 62)
		for range 3 {
			st := time.Now()
			if _, err := canonicalJSON(s); err != nil {
				t.Fatal(err)
			}
			best = min(best, time.Since(st))
		}
		return best
	}
	small, large := cost(250_000), cost(1_000_000)
	t.Logf("exponent of 250k digits: %v; of 1M digits: %v (x%.1f)", small, large, float64(large)/float64(small))
	if large > 8*small {
		t.Fatalf("4x the exponent digits cost x%.1f the time: not linear", float64(large)/float64(small))
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
