package runview

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// gapCfg is validate -> charge (gated) -> receipt, with extra nodes, wiring and assertions spliced in.
func gapCfg(extraNodes, extraWiring string, asserts ...string) string {
	as := ""
	if len(asserts) > 0 {
		as = `, "assert": ["` + strings.Join(asserts, `", "`) + `"]`
	}
	return `{"version": 1, "flow": "f", "entry": "validate",
	  "nodes": [{"name": "validate", "block": "validate", "safety": "readonly"},
	            {"name": "charge", "block": "charge", "safety": "side_effect", "approval": {"need": 2, "approvers": ["ops", "finance", "risk"]}},
	            {"name": "receipt", "block": "receipt", "safety": "side_effect"}` + extraNodes + `],
	  "wiring": [{"edge": ["validate", "charge"]}, {"edge": ["charge", "receipt"]}` + extraWiring + `]` + as + `}`
}

func weakenedOf(t *testing.T, from, to string) []string {
	t.Helper()
	return diffOf(t, from, to, nil, nil).Weakened
}

// G3: a new route to a side effect that passes no approval gate is a weakening, with the path.
func TestDiffNewUngatedPathIsWeakened(t *testing.T) {
	base := gapCfg("", "")
	got := weakenedOf(t, base, gapCfg("", `, {"edge": ["validate", "receipt"]}`))
	want := "receipt: reachable without an approval gate (validate -> receipt), not before"
	if !slices.Equal(got, []string{want}) {
		t.Fatalf("weakened = %q; want %q", got, []string{want})
	}
	// The same through an intermediate node, and the gate itself removed: reported for the node
	// whose gate went (as a gate change) and for each effect that became reachable ungated.
	v := diffOf(t, base, gapCfg(`, {"name": "skip", "block": "skip", "safety": "readonly"}`, `, {"edge": ["validate", "skip"]}, {"edge": ["skip", "receipt"]}`), nil, nil)
	if !slices.Equal(v.Weakened, []string{"receipt: reachable without an approval gate (validate -> skip -> receipt), not before"}) {
		t.Errorf("bypass through skip: %q", v.Weakened)
	}
	// Dropping the gate on charge is the gate's own change (not also a path change for charge), and
	// receipt, which sat behind it, is now ungated too.
	nogate := strings.Replace(base, `, "approval": {"need": 2, "approvers": ["ops", "finance", "risk"]}`, "", 1)
	v = diffOf(t, base, nogate, nil, nil)
	if len(v.Weakened) != 2 || !strings.HasPrefix(v.Weakened[0], "charge: approval 2 of 3: ops, finance, risk -> none") || !strings.HasPrefix(v.Weakened[1], "receipt: reachable without an approval gate") {
		t.Errorf("gate dropped: %q", v.Weakened)
	}
}

// Not flagged: a bypass to a node that is not an effect, a new route that is gated, an effect that
// was already ungated, a new ungated node (nothing was gated before), and a change that gates more.
func TestDiffUngatedPathNotFalselyFlagged(t *testing.T) {
	base := gapCfg("", "")
	for name, to := range map[string]string{
		"read-only node reachable ungated": gapCfg(`, {"name": "log", "block": "log", "safety": "readonly"}`, `, {"edge": ["validate", "log"]}`),
		"new effect node ungated":          gapCfg(`, {"name": "notify", "block": "notify", "safety": "side_effect"}`, `, {"edge": ["validate", "notify"]}`),
		"new route that is gated":          gapCfg(`, {"name": "review", "block": "review", "safety": "readonly", "approval": {"need": 1, "approvers": ["ops"]}}`, `, {"edge": ["validate", "review"]}, {"edge": ["review", "receipt"]}`),
		"idempotent node ungated":          gapCfg(`, {"name": "stamp", "block": "stamp", "safety": "idempotent"}`, `, {"edge": ["validate", "stamp"]}`),
	} {
		if got := weakenedOf(t, base, to); len(got) != 0 {
			t.Errorf("%s: weakened %q", name, got)
		}
	}
	// receipt was ungated before and after: not newly ungated.
	open := strings.Replace(gapCfg("", ""), `, "approval": {"need": 2, "approvers": ["ops", "finance", "risk"]}`, "", 1)
	if got := weakenedOf(t, open, strings.Replace(open, `"wiring": [`, `"wiring": [{"edge": ["validate", "receipt"]}, `, 1)); len(got) != 0 {
		t.Errorf("already ungated: weakened %q", got)
	}
	// Gating the node more is no weakening.
	gated := strings.Replace(open, `"name": "receipt", "block": "receipt", "safety": "side_effect"`, `"name": "receipt", "block": "receipt", "safety": "side_effect", "approval": {"need": 1, "approvers": ["ops"]}`, 1)
	if got := weakenedOf(t, open, gated); len(got) != 0 {
		t.Errorf("gate added: weakened %q", got)
	}
}

// A node whose safety the config does not state (no Registry) may be an effect, so a new ungated
// route to it is flagged, and says the safety is unknown.
func TestDiffUngatedPathUnknownSafety(t *testing.T) {
	mk := func(wiring string) string {
		return `{"version": 1, "flow": "f", "entry": "a", "nodes": [{"name": "a", "block": "a"}, {"name": "pay", "block": "pay", "approval": {"need": 1, "approvers": ["ops"]}}, {"name": "send", "block": "send"}],
		  "wiring": [{"edge": ["a", "pay"]}, {"edge": ["pay", "send"]}` + wiring + `]}`
	}
	got := weakenedOf(t, mk(""), mk(`, {"edge": ["a", "send"]}`))
	if !slices.Equal(got, []string{"send: reachable without an approval gate (a -> send), not before; its safety is not known"}) {
		t.Fatalf("weakened = %q", got)
	}
}

// G1: removing an assert line, or replacing an approval assertion with a weaker one, is a weakening;
// keeping it (however spelled), tightening it or adding one is not.
func TestDiffAssertions(t *testing.T) {
	two := "every path to receipt passes approval >= 2 of 3"
	for _, c := range []struct {
		name     string
		from, to []string
		weak     []string
		strong   bool
	}{
		{"removed", []string{two}, nil, []string{"assertion removed: " + two}, false},
		{"one of two removed", []string{two, "no side effect after validate"}, []string{two}, []string{"assertion removed: no side effect after validate"}, false},
		{"lowered need", []string{two}, []string{"every path to receipt passes approval >= 1 of 3"}, []string{"assertion loosened: " + two + " -> every path to receipt passes approval >= 1 of 3"}, false},
		{"larger set", []string{two}, []string{"every path to receipt passes approval >= 2 of 5"}, []string{"assertion loosened: " + two + " -> every path to receipt passes approval >= 2 of 5"}, false},
		{"requester no longer excluded", []string{two + " excluding the requester"}, []string{two}, []string{"assertion loosened: " + two + " excluding the requester -> " + two}, false},
		{"replaced by an unrelated one", []string{two}, []string{"receipt reachable"}, []string{"assertion removed: " + two}, false},
		{"unchanged", []string{two}, []string{two}, nil, false},
		{"respelled", []string{two}, []string{"every  path to receipt passes approval >= 2 of 3"}, nil, false},
		{"tightened", []string{two}, []string{"every path to receipt passes approval >= 3 of 3"}, nil, true},
		{"added", nil, []string{two}, nil, false},
		{"reordered", []string{two, "receipt reachable"}, []string{"receipt reachable", two}, nil, false},
	} {
		v := diffOf(t, gapCfg("", "", c.from...), gapCfg("", "", c.to...), nil, nil)
		if !slices.Equal(v.Weakened, c.weak) {
			t.Errorf("%s: weakened %q; want %q", c.name, v.Weakened, c.weak)
		}
		if (len(v.Strengthened) > 0) != c.strong {
			t.Errorf("%s: strengthened %q", c.name, v.Strengthened)
		}
		// An assertion change is a change: a diff with no other change still lists it.
		if len(c.weak) > 0 && len(v.Changes) == 0 {
			t.Errorf("%s: no change listed", c.name)
		}
	}
}

// The text output names what was weakened, and -fail-on-weaken exits 1 on each new kind.
func TestDiffGapsText(t *testing.T) {
	var out bytes.Buffer
	weak, err := CheckDiff(&out, Diff{Old: Config{Name: "old", Data: []byte(gapCfg("", "", "receipt reachable"))}, New: Config{Name: "new", Data: []byte(gapCfg("", `, {"edge": ["validate", "receipt"]}`))}})
	if err != nil || !weak {
		t.Fatalf("weak %v, err %v", weak, err)
	}
	for _, want := range []string{
		"new: removed assertion: receipt reachable (weakened)",
		"new: weakened: assertion removed: receipt reachable",
		"new: weakened: receipt: reachable without an approval gate (validate -> receipt), not before",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	v := diffOf(t, gapCfg("", ""), gapCfg("", `, {"edge": ["validate", "receipt"]}`), nil, nil)
	if got := v.Verdict.Word; got != "SAFETY CHECK WEAKENED" {
		t.Errorf("verdict word %q", got)
	}
}
