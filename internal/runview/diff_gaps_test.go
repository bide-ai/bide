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
	want := "receipt: reachable without an approval gate (validate -> receipt), not before; its safety is side effect"
	if !slices.Equal(got, []string{want}) {
		t.Fatalf("weakened = %q; want %q", got, []string{want})
	}
	// The same through an intermediate node, and the gate itself removed: reported for the node
	// whose gate went (as a gate change) and for each effect that became reachable ungated.
	v := diffOf(t, base, gapCfg(`, {"name": "skip", "block": "skip", "safety": "readonly"}`, `, {"edge": ["validate", "skip"]}, {"edge": ["skip", "receipt"]}`), nil, nil)
	if !slices.Equal(v.Weakened, []string{"receipt: reachable without an approval gate (validate -> skip -> receipt), not before; its safety is side effect"}) {
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

// A new node that may have an effect and that no approval gate guards is a weakening: nothing stood
// in front of it before. A new node behind a gate, or one that is read-only, is not.
func TestDiffNewUngatedEffectNodeIsWeakened(t *testing.T) {
	base := gapCfg("", "")
	got := weakenedOf(t, base, gapCfg(`, {"name": "notify", "block": "notify", "safety": "side_effect"}`, `, {"edge": ["validate", "notify"]}`))
	if want := []string{"notify: new node, reachable without an approval gate (validate -> notify), not before; its safety is side effect"}; !slices.Equal(got, want) {
		t.Errorf("new ungated effect: weakened %q; want %q", got, want)
	}
	// Unknown safety counts, and says so; the node is also listed as added.
	v := diffOf(t, base, gapCfg(`, {"name": "x", "block": "x"}`, `, {"edge": ["validate", "x"]}`), nil, nil)
	if len(v.Weakened) != 1 || !strings.HasSuffix(v.Weakened[0], "; its safety is not known") || v.Changes[0].Change != "added" || v.Changes[0].Weakening != "path" {
		t.Errorf("unknown safety: %q, %q", v.Weakened, lines(v.Changes))
	}
	for name, to := range map[string]string{
		"behind a gate":         gapCfg(`, {"name": "notify", "block": "notify", "safety": "side_effect"}`, `, {"edge": ["charge", "notify"]}`),
		"read-only, ungated":    gapCfg(`, {"name": "log", "block": "log", "safety": "readonly"}`, `, {"edge": ["validate", "log"]}`),
		"effect, gated itself":  gapCfg(`, {"name": "notify", "block": "notify", "safety": "side_effect", "approval": {"need": 1, "approvers": ["ops"]}}`, `, {"edge": ["validate", "notify"]}`),
		"unreachable (no edge)": gapCfg(`, {"name": "orphan", "block": "orphan", "safety": "side_effect"}`, ``),
	} {
		if got := weakenedOf(t, base, to); len(got) != 0 {
			t.Errorf("%s: weakened %q", name, got)
		}
	}
}

// Not flagged: a new node that is not an effect, a new route that is gated, an effect that
// was already ungated, and a change that gates more.
func TestDiffUngatedPathNotFalselyFlagged(t *testing.T) {
	base := gapCfg("", "")
	for name, to := range map[string]string{
		"read-only node reachable ungated": gapCfg(`, {"name": "log", "block": "log", "safety": "readonly"}`, `, {"edge": ["validate", "log"]}`),
		"new effect node behind a gate":    gapCfg(`, {"name": "notify", "block": "notify", "safety": "side_effect"}`, `, {"edge": ["charge", "notify"]}`),
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

// G3 is broad: a node every path reached through a gate before, and a path now reaches without one, is
// weakened whatever its safety, and the report names the node, its safety and the path.
func TestDiffBroadBypassIsWeakened(t *testing.T) {
	withAudit := func(safety, wiring string) string {
		return gapCfg(`, {"name": "audit", "block": "audit", "safety": "`+safety+`"}`, `, {"edge": ["charge", "audit"]}`+wiring)
	}
	for _, safety := range []string{"readonly", "idempotent", "side_effect"} {
		got := weakenedOf(t, withAudit(safety, ""), withAudit(safety, `, {"edge": ["validate", "audit"]}`))
		name := map[string]string{"readonly": "read-only", "idempotent": "idempotent", "side_effect": "side effect"}[safety]
		want := "audit: reachable without an approval gate (validate -> audit), not before; its safety is " + name
		if !slices.Equal(got, []string{want}) {
			t.Errorf("%s: weakened %q; want %q", safety, got, want)
		}
	}
}

// A relabel to read-only cannot hide the bypass: the old label counts, and the lowered safety is a
// weakening on its own.
func TestDiffSafetyLoweredIsWeakened(t *testing.T) {
	base := gapCfg("", "")
	relabeled := strings.Replace(base, `"name": "receipt", "block": "receipt", "safety": "side_effect"`, `"name": "receipt", "block": "receipt", "safety": "readonly"`, 1)
	got := weakenedOf(t, base, relabeled)
	if want := []string{"receipt: safety lowered side effect -> read-only"}; !slices.Equal(got, want) {
		t.Errorf("relabel alone: weakened %q; want %q", got, want)
	}
	bypass := strings.Replace(relabeled, `"wiring": [`, `"wiring": [{"edge": ["validate", "receipt"]}, `, 1)
	got = weakenedOf(t, base, bypass)
	if want := []string{"receipt: reachable without an approval gate (validate -> receipt), not before; its safety is read-only; safety lowered side effect -> read-only"}; !slices.Equal(got, want) {
		t.Errorf("relabel and bypass: weakened %q; want %q", got, want)
	}
	idem := strings.Replace(base, `"name": "receipt", "block": "receipt", "safety": "side_effect"`, `"name": "receipt", "block": "receipt", "safety": "idempotent"`, 1)
	if got := weakenedOf(t, base, idem); len(got) != 1 {
		t.Errorf("side effect -> idempotent: weakened %q", got)
	}
	if got := weakenedOf(t, idem, base); len(got) != 0 {
		t.Errorf("idempotent -> side effect: weakened %q", got)
	}
	// Renamed and relabeled together is still the same node.
	moved := strings.NewReplacer(`"name": "receipt", "block": "receipt", "safety": "side_effect"`, `"name": "receipt2", "block": "receipt", "safety": "readonly"`, `["charge", "receipt"]`, `["charge", "receipt2"]`).Replace(base)
	got = weakenedOf(t, base, moved)
	if want := []string{"receipt2: safety lowered side effect -> read-only"}; !slices.Equal(got, want) {
		t.Errorf("renamed and relabeled: weakened %q; want %q", got, want)
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
		{"stronger and weaker variants added", []string{two}, []string{"every path to receipt passes approval >= 1 of 3", "every path to receipt passes approval >= 3 of 3"}, nil, true},
		{"reordered", []string{two, "receipt reachable"}, []string{"receipt reachable", two}, nil, false},
	} {
		v := diffOf(t, gapCfg("", "", c.from...), gapCfg("", "", c.to...), nil, nil)
		if !slices.Equal(v.Weakened, c.weak) {
			t.Errorf("%s: weakened %q; want %q", c.name, v.Weakened, c.weak)
		}
		if (len(v.Strengthened) > 0) != c.strong {
			t.Errorf("%s: strengthened %q", c.name, v.Strengthened)
		}
		var shown []string
		for _, ch := range v.Changes {
			if ch.What == "assertion" {
				shown = append(shown, ch.Change)
			}
		}
		if c.name == "added" && !slices.Equal(shown, []string{"added"}) || c.name == "unchanged" && len(shown) != 0 || c.name == "stronger and weaker variants added" && !slices.Equal(shown, []string{"changed", "added"}) {
			t.Errorf("%s: assertion changes %q", c.name, shown)
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

// G1, the other half: an assert line kept as it was is no protection when it stops holding. A line
// that held and is now violated, cannot be decided or no longer reads is a weakening, with what
// changed; a line that did not hold before, or still holds, is not.
func TestDiffAssertionNoLongerHolds(t *testing.T) {
	term := "every path to a terminal passes charge"
	for _, c := range []struct {
		name       string
		oldN, oldW string
		newN, newW string
		asserts    []string
		weak       string // the one expected weakened entry, "" for none
	}{
		{"held, now violated by a read-only route", "", "", `, {"name": "audit", "block": "audit", "safety": "readonly"}`, `, {"edge": ["validate", "audit"]}`,
			[]string{term}, "assertion no longer holds: " + term + " (violated, counterexample: validate -> audit)"},
		{"reachable, now unreachable", `, {"name": "audit", "block": "audit", "safety": "readonly"}`, `, {"edge": ["validate", "audit"]}`, `, {"name": "audit", "block": "audit", "safety": "readonly"}`, "",
			[]string{"audit reachable"}, "assertion no longer holds: audit reachable (violated"},
		{"names a node the new config dropped", `, {"name": "audit", "block": "audit", "safety": "readonly"}`, `, {"edge": ["validate", "audit"]}`, "", "",
			[]string{"audit reachable"}, "assertion no longer holds: audit reachable (invalid"},
		{"held, now undecided", "", "", `, {"name": "audit", "block": "audit"}`, `, {"edge": ["receipt", "audit"]}`,
			[]string{"no side effect after receipt"}, "assertion no longer holds: no side effect after receipt (undecided"},
		{"unchanged and still holds", "", "", "", "", []string{term}, ""},
		{"violated before and after", `, {"name": "audit", "block": "audit", "safety": "readonly"}`, `, {"edge": ["validate", "audit"]}`,
			`, {"name": "audit", "block": "audit", "safety": "readonly"}, {"name": "audit2", "block": "audit", "safety": "readonly"}`, `, {"edge": ["validate", "audit"]}, {"edge": ["validate", "audit2"]}`,
			[]string{term}, ""},
	} {
		oldCfg := gapCfg(c.oldN, c.oldW, c.asserts...)
		newCfg := gapCfg(c.newN, c.newW, c.asserts...)
		var got []string
		for _, w := range weakenedOf(t, oldCfg, newCfg) {
			if strings.HasPrefix(w, "assertion ") {
				got = append(got, w)
			}
		}
		switch {
		case c.weak == "" && len(got) > 0, c.weak != "" && (len(got) != 1 || !strings.HasPrefix(got[0], c.weak)):
			t.Errorf("%s: weakened %q; want %q", c.name, got, c.weak)
		}
	}
}

// A line that held and now violates is a weakening even when nothing else in the diff is: the text
// output names it, the verdict says the safety check was weakened, and -fail-on-weaken would exit 1.
func TestDiffAssertionNoLongerHoldsReported(t *testing.T) {
	term := "every path to a terminal passes charge"
	oldCfg := gapCfg("", "", term)
	newCfg := gapCfg(`, {"name": "audit", "block": "audit", "safety": "readonly"}`, `, {"edge": ["validate", "audit"]}`, term)
	var out bytes.Buffer
	weak, err := CheckDiff(&out, Diff{Old: Config{Name: "old", Data: []byte(oldCfg)}, New: Config{Name: "new", Data: []byte(newCfg)}})
	if err != nil || !weak {
		t.Fatalf("weak %v, err %v\n%s", weak, err, out.String())
	}
	for _, want := range []string{
		"new: weakened: assertion no longer holds: " + term + " (violated, counterexample: validate -> audit)",
		"changed assertion: " + term + ": held before, now violated (weakened)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if w := diffOf(t, oldCfg, newCfg, nil, nil).Verdict.Word; w != "SAFETY CHECK WEAKENED" {
		t.Errorf("verdict word %q", w)
	}
}
