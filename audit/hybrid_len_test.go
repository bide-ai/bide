package audit

import (
	"math"
	"testing"
)

// A hybrid signature's length prefix is a uint32. Converted to a 32-bit int, a prefix of 2^31 or
// more is negative, so it would pass a comparison against the remaining length and the split
// sig[4:4+n] would panic. The check must hold at every int width, which this test exercises by
// instantiating it at int32, the width of int on 386 and arm.
func TestHybridLenFits_HoldsAtEveryIntWidth(t *testing.T) {
	for _, tc := range []struct {
		n    uint32
		rest int64
		want bool
	}{
		{0, 0, true},
		{0, -1, false},
		{32, 32, true},
		{33, 32, false},
		{math.MaxInt32, 100, false},
		{math.MaxInt32 + 1, 100, false},
		{math.MaxUint32, 100, false},
		{math.MaxUint32, 0, false},
	} {
		if got := hybridLenFits(tc.n, int32(tc.rest)); got != tc.want {
			t.Errorf("hybridLenFits[int32](%d, %d) = %v, want %v", tc.n, tc.rest, got, tc.want)
		}
		if got := hybridLenFits(tc.n, tc.rest); got != tc.want {
			t.Errorf("hybridLenFits[int64](%d, %d) = %v, want %v", tc.n, tc.rest, got, tc.want)
		}
		if got := hybridLenFits(tc.n, int(tc.rest)); got != tc.want {
			t.Errorf("hybridLenFits[int](%d, %d) = %v, want %v", tc.n, tc.rest, got, tc.want)
		}
	}
}

// A signature whose length prefix is 0xFFFFFFFF does not verify, and does not panic.
func TestHybridVerifier_HugeLengthPrefix(t *testing.T) {
	if (HybridVerifier{}).Verify([]byte("m"), []byte{0xff, 0xff, 0xff, 0xff, 1, 2, 3}) {
		t.Fatal("a hybrid signature with an impossible length prefix verified")
	}
}
