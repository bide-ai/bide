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

func TestUngatedPath(t *testing.T) {
	cfg := func(entryGate string) []byte {
		return []byte(`{"version": 1, "flow": "f", "entry": "a",
		  "nodes": [{"name": "a", "block": "a"` + entryGate + `}, {"name": "b", "block": "b", "approval": {"need": 1, "approvers": ["ops"]}}, {"name": "c", "block": "c"}],
		  "wiring": [{"edge": ["a", "b"]}, {"edge": ["b", "c"]}, {"edge": ["a", "c"]}]}`)
	}
	r := InspectConfig(cfg(""), nil)
	if p := r.UngatedPath("c"); len(p) != 2 || p[0] != "a" || p[1] != "c" {
		t.Errorf("path to c = %v, want a -> c", p)
	}
	if p := r.UngatedPath("b"); p != nil {
		t.Errorf("path to the gated node b = %v, want none (its own gate is on the way)", p)
	}
	// With the entry gated every path passes a gate.
	r = InspectConfig(cfg(`, "approval": {"need": 1, "approvers": ["ops"]}`), nil)
	if p := r.UngatedPath("c"); p != nil || !r.Gated["c"] {
		t.Errorf("entry gated: path %v, gated %v", p, r.Gated["c"])
	}
}
