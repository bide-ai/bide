package plan

import (
	"strings"
	"testing"
)

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
		// Quoting a node name changes no assertion, in any form.
		{"charge reachable", `"charge" reachable`, "equal"},
		{"every path to a terminal passes charge", `every path to a terminal passes "charge"`, "equal"},
		{"every path to receipt passes charge", `every path to "receipt" passes "charge"`, "equal"},
		{"no side effect after validate", `no side effect after "validate"`, "equal"},
		{"charge reachable", `"charge" unreachable`, ""},
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

func TestRenameAssertion(t *testing.T) {
	ren := func(n string) string {
		if n == "receipt" {
			return "receipt 2"
		}
		return n
	}
	for _, c := range []struct {
		in     string
		rename func(string) string
		want   string
	}{
		{"receipt reachable", ren, `"receipt 2" reachable`},
		{`every path to "receipt" passes charge`, ren, `every path to "receipt 2" passes "charge"`},
		{"every path to charge passes approval >= 2 of 3 excluding the requester", ren, `every path to "charge" passes approval >= 2 of 3 excluding the requester`},
		{"every path to a terminal passes receipt", ren, `every path to a terminal passes "receipt 2"`},
		{"no side  effect after receipt", ren, `no side effect after "receipt 2"`},
		{"receipt reachable", nil, `"receipt" reachable`},
		{"not an  assertion", ren, "not an assertion"},
	} {
		if got := RenameAssertion(c.in, c.rename); got != c.want {
			t.Errorf("RenameAssertion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// An assertion about nothing the entry reaches holds vacuously, and says so; a reachable one does not.
func TestAssertionVacuous(t *testing.T) {
	cfg := func(wiring string, asserts ...string) []byte {
		return []byte(`{"version": 1, "flow": "f", "entry": "a", "nodes": [{"name": "a", "block": "a", "safety": "readonly"}, {"name": "b", "block": "b", "safety": "readonly"}, {"name": "c", "block": "c", "safety": "readonly"}], "wiring": [` + wiring + `], "assert": ["` + strings.Join(asserts, `", "`) + `"]}`)
	}
	tail := `{"edge": ["a", "b"]}, {"edge": ["b", "c"]}`
	r := InspectConfig(cfg(tail, "every path to a terminal passes b", "every path to c passes b", "every path to c passes approval >= 1", "c reachable"), nil)
	for i, a := range r.Assertions {
		if i < 2 && (!a.Holds || a.Vacuous) {
			t.Errorf("%q on a chain: %+v", a.Text, a)
		}
	}
	// A cycle has no terminal; the line holds vacuously.
	r = InspectConfig(cfg(tail+`, {"edge": ["c", "a"]}`, "every path to a terminal passes b"), nil)
	if a := r.Assertions[0]; !a.Holds || !a.Vacuous || !strings.Contains(a.Message, "vacuously") {
		t.Errorf("cycle: %+v", a)
	}
	// "no side effect after X" is vacuous when the entry cannot reach X.
	r = InspectConfig(cfg(`{"edge": ["a", "b"]}`, "no side effect after c", "no side effect after b"), nil)
	if a := r.Assertions[0]; !a.Holds || !a.Vacuous {
		t.Errorf("no side effect after an unreachable node: %+v", a)
	}
	if a := r.Assertions[1]; !a.Holds || a.Vacuous {
		t.Errorf("no side effect after a reachable node: %+v", a)
	}
	// c is cut off from the entry.
	r = InspectConfig(cfg(`{"edge": ["a", "b"]}`, "every path to c passes b", "every path to c passes approval >= 2"), nil)
	for _, a := range r.Assertions {
		if !a.Holds || !a.Vacuous {
			t.Errorf("unreachable target %q: %+v", a.Text, a)
		}
	}
}
