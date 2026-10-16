package runview

import (
	"slices"
	"strings"
	"testing"
)

const termAssert = "every path to a terminal passes charge"

// A: an assert line that held and now holds only vacuously (nothing it is about is reachable) is a
// weakening: the flow turned cyclic, or lost the path to the node.
func TestDiffAssertionHeldNowVacuous(t *testing.T) {
	// "every path to a terminal passes charge" held; receipt -> validate removes every terminal.
	v := diffOf(t, gapCfg("", "", termAssert), gapCfg("", `, {"edge": ["receipt", "validate"]}`, termAssert), nil, nil)
	if len(v.Weakened) != 1 || !strings.Contains(v.Weakened[0], termAssert) || !strings.Contains(v.Weakened[0], "vacuous") {
		t.Errorf("held -> vacuous (terminal): weakened %q, changes %q", v.Weakened, lines(v.Changes))
	}
	if len(v.weakKinds) != 1 || v.weakKinds[0] != "assertion" {
		t.Errorf("kinds %q", v.weakKinds)
	}
	// The target cut off from the entry: the gate assertion holds because charge is unreachable.
	gate := "every path to charge passes approval >= 2"
	cut := strings.Replace(gapCfg("", "", gate), `{"edge": ["validate", "charge"]}, {"edge": ["charge", "receipt"]}`, `{"edge": ["validate", "receipt"]}`, 1)
	if got := weakenedOf(t, gapCfg("", "", gate), cut); !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "vacuous") }) {
		t.Errorf("held -> vacuous (unreachable target): weakened %q", got)
	}
	// Not reported: vacuous before and after, held non-vacuously after, and vacuous now but violated before.
	cyc := gapCfg("", `, {"edge": ["receipt", "validate"]}`, termAssert)
	if got := weakenedOf(t, cyc, cyc); len(got) != 0 {
		t.Errorf("vacuous before and after: %q", got)
	}
	if got := weakenedOf(t, cyc, gapCfg("", "", termAssert)); len(got) != 0 {
		t.Errorf("vacuous -> held: %q", got)
	}
}

// B: a line written twice is one assertion: one entry, one change line.
func TestDiffDuplicateAssertionLines(t *testing.T) {
	v := diffOf(t, gapCfg("", "", termAssert, termAssert), gapCfg(`, {"name": "audit", "block": "audit", "safety": "readonly"}`, `, {"edge": ["validate", "audit"]}`, termAssert, termAssert), nil, nil)
	if len(v.Weakened) != 1 {
		t.Errorf("a violated line written twice: weakened %q", v.Weakened)
	}
	n := 0
	for _, c := range v.Changes {
		if c.What == "assertion" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d assertion changes; want 1: %q", n, lines(v.Changes))
	}
	// Removed twice, and added twice.
	v = diffOf(t, gapCfg("", "", "charge reachable", "charge reachable"), gapCfg("", ""), nil, nil)
	if len(v.Weakened) != 1 {
		t.Errorf("removed twice: weakened %q", v.Weakened)
	}
	v = diffOf(t, gapCfg("", ""), gapCfg("", "", "charge reachable", "charge reachable"), nil, nil)
	if len(v.Changes) != 1 || v.Changes[0].Change != "added" {
		t.Errorf("added twice: %q", lines(v.Changes))
	}
}

// C: respelling a node name in quotes, or the spacing, changes no assertion, in every form.
func TestDiffAssertionQuoteOnlyRespelling(t *testing.T) {
	for _, c := range [][2]string{
		{"charge reachable", `\"charge\" reachable`},
		{"charge unreachable", `\"charge\" unreachable`},
		{termAssert, `every path to a terminal passes \"charge\"`},
		{"every path to receipt passes charge", `every path to \"receipt\" passes \"charge\"`},
		{"no side effect after validate", `no side effect after \"validate\"`},
		{"every path to charge passes approval >= 2 of 3", `every  path to \"charge\" passes approval >= 2 of 3`},
	} {
		v := diffOf(t, gapCfg("", "", c[0]), gapCfg("", "", c[1]), nil, nil)
		if len(v.Changes) != 0 || len(v.Weakened) != 0 {
			t.Errorf("%q -> %q: changes %q, weakened %q", c[0], c[1], lines(v.Changes), v.Weakened)
		}
	}
	// A different node is still a different assertion.
	v := diffOf(t, gapCfg("", "", "charge reachable"), gapCfg("", "", `\"receipt\" reachable`), nil, nil)
	if len(v.Weakened) != 1 {
		t.Errorf("a different node: weakened %q", v.Weakened)
	}
}

// D: a node renamed together with its assert line is a rename, not a removed assertion; a line
// moved to another node, or following a rename the check is unsure of, stays reported. A paired
// rename's line is still decided, so one that stops holding is reported under the new name.
func TestDiffAssertionFollowsRename(t *testing.T) {
	// rename receipt -> receipt2 in the node, the edge and the assert lines (the block keeps its name).
	rename := func(s string) string {
		head, tail, _ := strings.Cut(s, `"assert"`)
		head = strings.NewReplacer(`"name": "receipt"`, `"name": "receipt2"`, `["charge", "receipt"]`, `["charge", "receipt2"]`).Replace(head)
		if tail != "" {
			tail = `"assert"` + strings.ReplaceAll(tail, "receipt", "receipt2")
		}
		return head + tail
	}
	for _, a := range []string{
		"receipt reachable",
		"every path to receipt passes charge",
		"every path to receipt passes approval >= 2 of 3",
		"no side effect after receipt",
		"every path to charge passes receipt",
	} {
		oldCfg := gapCfg("", "", a)
		v := diffOf(t, oldCfg, rename(oldCfg), nil, nil)
		if len(v.Weakened) != 0 {
			t.Errorf("%q renamed with its node: weakened %q, changes %q", a, v.Weakened, lines(v.Changes))
		}
		for _, c := range v.Changes {
			if c.What == "assertion" {
				t.Errorf("%q renamed with its node: %q", a, lines(v.Changes))
			}
		}
	}
	// A bypass that lands with the rename is reported (the edges changed, so the rename is not paired and
	// the node is judged as new, its line as removed).
	a := "every path to receipt passes approval >= 2 of 3"
	oldCfg := gapCfg("", "", a)
	bypass := strings.Replace(rename(oldCfg), `"wiring": [`, `"wiring": [{"edge": ["validate", "receipt2"]}, `, 1)
	if v := diffOf(t, oldCfg, bypass, nil, nil); len(v.Weakened) != 2 {
		t.Errorf("renamed and bypassed: weakened %q", v.Weakened)
	}
	// The line dropped while the node is renamed is still a removal.
	v := diffOf(t, gapCfg("", "", "receipt reachable"), rename(gapCfg("", "")), nil, nil)
	if !slices.ContainsFunc(v.Weakened, func(s string) bool { return strings.HasPrefix(s, "assertion removed") }) {
		t.Errorf("assertion dropped with the rename: weakened %q", v.Weakened)
	}
	// A line renamed onto a different node than the node was renamed to is a different assertion.
	other := strings.Replace(rename(gapCfg("", "", "receipt reachable")), "receipt2 reachable", "charge reachable", 1)
	if v := diffOf(t, gapCfg("", "", "receipt reachable"), other, nil, nil); len(v.Weakened) != 1 {
		t.Errorf("the line moved to another node: weakened %q", v.Weakened)
	}
	// An uncertain rename is not assumed: a node renamed with a changed edge is not paired, and its
	// line reads as removed.
	moved := strings.Replace(rename(gapCfg("", "", "receipt reachable")), `{"edge": ["charge", "receipt2"]}`, `{"edge": ["validate", "receipt2"]}`, 1)
	if v := diffOf(t, gapCfg("", "", "receipt reachable"), moved, nil, nil); !slices.ContainsFunc(v.Weakened, func(s string) bool { return strings.HasPrefix(s, "assertion removed") }) {
		t.Errorf("an unpaired rename: weakened %q", v.Weakened)
	}
}

// A paired rename whose assertion stops holding is reported once, under the old line's text.
func TestDiffAssertionRegressionThroughRename(t *testing.T) {
	// receipt is renamed to receipt2 (same edges); the gate on charge is dropped in the same change.
	oldCfg := gapCfg("", "", "every path to receipt passes approval >= 2 of 3")
	renamed := strings.NewReplacer(`"name": "receipt"`, `"name": "receipt2"`, `["charge", "receipt"]`, `["charge", "receipt2"]`, "to receipt passes", "to receipt2 passes").Replace(oldCfg)
	renamed = strings.Replace(renamed, `, "approval": {"need": 2, "approvers": ["ops", "finance", "risk"]}`, "", 1)
	v := diffOf(t, oldCfg, renamed, nil, nil)
	if !slices.ContainsFunc(v.Weakened, func(s string) bool {
		return strings.HasPrefix(s, "assertion no longer holds: every path to receipt passes approval >= 2 of 3")
	}) {
		t.Errorf("weakened %q", v.Weakened)
	}
}

// "no side effect after X" is vacuous too once X is cut off from the entry.
func TestDiffNoSideEffectHeldNowVacuous(t *testing.T) {
	a := "no side effect after receipt"
	cut := strings.Replace(gapCfg("", "", a), `{"edge": ["charge", "receipt"]}`, `{"edge": ["validate", "validate"]}`, 1)
	if got := weakenedOf(t, gapCfg("", "", a), cut); !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "vacuously: "+a) }) {
		t.Errorf("weakened %q", got)
	}
}
