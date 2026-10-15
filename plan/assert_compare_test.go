package plan

import "testing"

func TestCompareAssertions(t *testing.T) {
	for _, c := range []struct{ a, b, want string }{
		{"every path to charge passes approval >= 2 of 3", "every path to charge passes approval >= 2 of 3", "equal"},
		{"every path to charge passes approval >= 2 of 3", `every path to "charge"  passes approval >= 2 of 3`, "equal"},
		{"every path to charge passes approval >= 2 of 3", "every path to charge passes approval >= 3 of 3", "stronger"},
		{"every path to charge passes approval >= 2 of 3", "every path to charge passes approval >= 2 of 2", "stronger"},
		{"every path to charge passes approval >= 2", "every path to charge passes approval >= 2 of 5", "stronger"},
		{"every path to charge passes approval >= 2", "every path to charge passes approval >= 2 excluding the requester", "stronger"},
		{"every path to charge passes approval >= 2 of 3", "every path to charge passes approval >= 1 of 3", "weaker"},
		{"every path to charge passes approval >= 2 of 3", "every path to charge passes approval >= 2 of 4", "weaker"},
		{"every path to charge passes approval >= 2 of 3", "every path to charge passes approval >= 2", "weaker"},
		{"every path to charge passes approval >= 2 excluding the requester", "every path to charge passes approval >= 2", "weaker"},
		// More K but a larger set, or a smaller set but a lower K: neither implies the other.
		{"every path to charge passes approval >= 2 of 3", "every path to charge passes approval >= 3 of 4", "weaker"},
		{"every path to charge passes approval >= 2 of 3", "every path to refund passes approval >= 2 of 3", ""},
		{"every path to charge passes review", "every path to charge passes approval_node", ""},
		{"no side effect after validate", "no side effect after validate", "equal"},
		{"no side effect after validate", "no side effect after charge", ""},
	} {
		if got := CompareAssertions(c.a, c.b); got != c.want {
			t.Errorf("CompareAssertions(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}
