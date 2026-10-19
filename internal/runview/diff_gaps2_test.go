package runview

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/plan"
)

// A node that is the same node under another name (same block, safety, gate and edges) is renamed,
// not removed and added: nothing got weaker. A rename that also changes the block, the gate or the
// edges is judged on that change.
func TestDiffRename(t *testing.T) {
	base := gapCfg("", "")
	rename := func(base, from, to string) string {
		return strings.NewReplacer(`"name": "`+from+`"`, `"name": "`+to+`"`, `"`+from+`"]`, `"`+to+`"]`, `["`+from+`"`, `["`+to+`"`).Replace(base)
	}
	v := diffOf(t, base, rename(base, "receipt", "receipt2"), nil, nil)
	var renamed []string
	for _, c := range v.Changes {
		if c.Change == "renamed" {
			renamed = append(renamed, c.Node+": "+c.Details[0])
		}
	}
	if len(v.Weakened) != 0 || len(renamed) != 1 || renamed[0] != "receipt2: was receipt, with the same block, safety, gate and edges" {
		t.Errorf("rename of a gated effect: weakened %q, renamed %q, changes %q", v.Weakened, renamed, lines(v.Changes))
	}
	// An ungated effect renamed: it was ungated before.
	open := gapCfg(`, {"name": "notify", "block": "notify", "safety": "side_effect"}`, `, {"edge": ["validate", "notify"]}`)
	if got := weakenedOf(t, open, rename(open, "notify", "notify2")); len(got) != 0 {
		t.Errorf("rename of an ungated effect: weakened %q", got)
	}
	// ... and the same rename that also swaps the block is a new node, judged as one.
	swapped := strings.Replace(rename(open, "notify", "notify2"), `"block": "notify"`, `"block": "sms"`, 1)
	if got := weakenedOf(t, open, swapped); len(got) != 1 || !strings.HasPrefix(got[0], "notify2: new node, reachable without an approval gate") {
		t.Errorf("rename with a new block: weakened %q", got)
	}
	// ... and one that also takes another route: edges differ, so it is not a rename.
	moved := strings.Replace(rename(open, "notify", "notify2"), `["validate", "notify2"]`, `["receipt", "notify2"]`, 1)
	if got := weakenedOf(t, open, moved); len(got) != 0 {
		// receipt is behind the gate, so a node moved behind it is gated: not a weakening.
		t.Errorf("rename moved behind the gate: weakened %q", got)
	}
	// A gated node renamed with its gate kept is no weakening; the same rename dropping the gate is.
	pay := rename(base, "charge", "pay")
	if got := weakenedOf(t, base, pay); len(got) != 0 {
		t.Errorf("gated node renamed: weakened %q", got)
	}
	nogate := strings.Replace(pay, `, "approval": {"need": 2, "approvers": ["ops", "finance", "risk"]}`, "", 1)
	v = diffOf(t, base, nogate, nil, nil)
	if len(v.Weakened) != 2 || !strings.HasPrefix(v.Weakened[0], "pay: approval 2 of 3: ops, finance, risk -> none") || !strings.HasPrefix(v.Weakened[1], "receipt: reachable without") {
		t.Errorf("renamed and ungated: weakened %q; want the renamed node's gate and the effect behind it", v.Weakened)
	}
	var line string
	for _, c := range v.Changes {
		if c.Change == "renamed" {
			line = c.String()
		}
	}
	if line != "renamed node pay: was charge; approval 2 of 3: ops, finance, risk -> none (weakened)" {
		t.Errorf("rename line = %q", line)
	}
	// Loosened with the rename: a renamed node, one change.
	loose := strings.Replace(pay, `"need": 2`, `"need": 1`, 1)
	if got := weakenedOf(t, base, loose); len(got) != 1 || got[0] != "pay: approval 2 of 3 -> 1 of 3" {
		t.Errorf("renamed and loosened: %q", got)
	}
	// Renamed and tightened: a strengthening.
	tight := strings.Replace(pay, `"need": 2`, `"need": 3`, 1)
	if v := diffOf(t, base, tight, nil, nil); len(v.Weakened) != 0 || len(v.Strengthened) != 1 {
		t.Errorf("renamed and tightened: %q, %q", v.Weakened, v.Strengthened)
	}
	// Two neighbours renamed at once pair up through each other.
	both := rename(rename(base, "charge", "pay"), "receipt", "receipt2")
	if got := weakenedOf(t, base, both); len(got) != 0 {
		t.Errorf("two renames: weakened %q", got)
	}
}

// A changed or added Switch arm condition, with another arm that leads to an effect no gate guards,
// is flagged for review: the predicates cannot be read, so the check cannot tell who now takes the
// ungated arm. Without such an arm, or without a change, it is not.
func TestDiffSwitchConditions(t *testing.T) {
	sw := func(when, els string) string {
		return `{"version": 1, "flow": "f", "entry": "validate",
	  "nodes": [{"name": "validate", "block": "validate", "safety": "readonly"},
	            {"name": "charge", "block": "charge", "safety": "side_effect", "approval": {"need": 2, "approvers": ["ops", "finance", "risk"]}},
	            {"name": "small", "block": "charge", "safety": "side_effect"},
	            {"name": "note", "block": "note", "safety": "readonly"}],
	  "wiring": [{"switch": "validate", "when": [` + when + `], "else": "` + els + `"}]}`
	}
	arm := func(p, to string) string { return `{"pred": "` + p + `", "to": "` + to + `"}` }
	got := weakenedOf(t, sw(arm("large", "charge"), "small"), sw(arm("tiny", "charge"), "small"))
	if len(got) != 1 || !strings.HasPrefix(got[0], "validate: arm conditions changed (removed when large -> charge; when tiny -> charge) while another arm reaches an ungated effect (small)") {
		t.Errorf("swapped condition: %q", got)
	}
	v := diffOf(t, sw(arm("large", "charge"), "small"), sw(arm("tiny", "charge"), "small"), nil, nil)
	if len(v.weakKinds) != 1 || v.weakKinds[0] != "condition" || v.Verdict.Word != "SAFETY CHECK WEAKENED" {
		t.Errorf("kinds %q, verdict %q", v.weakKinds, v.Verdict.Word)
	}
	// An added arm.
	if got := weakenedOf(t, sw(arm("large", "charge"), "small"), sw(arm("large", "charge")+", "+arm("huge", "note"), "small")); len(got) != 1 {
		t.Errorf("an added arm beside an ungated one: %q", got)
	}
	// The other arm's effect relabeled read-only in the same edit still counts as the effect it was.
	relabeled := strings.Replace(sw(arm("tiny", "charge"), "small"), `"name": "small", "block": "charge", "safety": "side_effect"`, `"name": "small", "block": "charge", "safety": "readonly"`, 1)
	v = diffOf(t, sw(arm("large", "charge"), "small"), relabeled, nil, nil)
	if !slices.Contains(v.weakKinds, "condition") || !slices.Contains(v.weakKinds, "safety") {
		t.Errorf("relabeled other arm: kinds %q, weakened %q", v.weakKinds, v.Weakened)
	}
	for name, c := range map[string][2]string{
		"unchanged":                      {sw(arm("large", "charge"), "small"), sw(arm("large", "charge"), "small")},
		"else arm gated, no ungated":     {sw(arm("large", "note"), "charge"), sw(arm("tiny", "note"), "charge")},
		"the other arm is read-only":     {sw(arm("large", "charge"), "note"), sw(arm("tiny", "charge"), "note")},
		"changed arm is the ungated one": {sw(arm("large", "small"), "charge"), sw(arm("tiny", "small"), "charge")},
	} {
		if got := weakenedOf(t, c[0], c[1]); len(got) != 0 {
			t.Errorf("%s: weakened %q", name, got)
		}
	}
}

// Without the plan.Registry a node's own approval gate (ToolSpec.Approval) is not visible, so a new
// block on an effect node no declared gate guards is flagged; with it, the gate is seen and the
// change is judged by what it shows. A gated or read-only node is not flagged either way.
func TestDiffBlockChangeStructural(t *testing.T) {
	cfg := func(block string, extra string) string {
		return `{"version": 1, "flow": "f", "entry": "a", "nodes": [{"name": "a", "block": "a"}, {"name": "b", "block": "` + block + `"` + extra + `}], "wiring": [{"edge": ["a", "b"]}]}`
	}
	if got := weakenedOf(t, cfg("b", ""), cfg("c", "")); len(got) != 1 || !strings.Contains(got[0], "the new block's own approval gate, if any, is not visible without the plan.Registry") {
		t.Errorf("block swapped on an effect node: %q", got)
	}
	gated := `, "approval": {"need": 1, "approvers": ["ops"]}`
	if got := weakenedOf(t, cfg("b", gated), cfg("c", gated)); len(got) != 0 {
		t.Errorf("gated node: %q", got)
	}
	if got := weakenedOf(t, cfg("b", `, "safety": "readonly"`), cfg("c", `, "safety": "readonly"`)); len(got) != 0 {
		t.Errorf("read-only node: %q", got)
	}
	// With a Registry per version (each holding what its config uses, as Load needs) the gate is seen
	// and the change is judged by what it shows.
	mk := func(names ...string) *plan.Registry {
		reg := plan.NewRegistry()
		pass := func(_ context.Context, o order) (order, error) { return o, nil }
		for _, n := range names {
			if err := reg.RegisterStep(n, pass); err != nil {
				t.Fatal(err)
			}
		}
		return reg
	}
	if v := diffOf(t, cfg("b", ""), cfg("c", ""), mk("a", "b"), mk("a", "c")); !v.Typed || len(v.Weakened) != 0 || v.Fallback != "" {
		t.Errorf("with a Registry per version: typed %v, weakened %q, fallback %q", v.Typed, v.Weakened, v.Fallback)
	}
	// One Registry for both holds a block each config does not use, which is an error on each side: a
	// Registry must never make the comparison less strict than none, so the diff is made structurally
	// and is not certified.
	all := mk("a", "b", "c")
	if v := diffOf(t, cfg("b", ""), cfg("c", ""), all, all); v.Typed || v.Fallback == "" || len(v.Weakened) != 1 {
		t.Errorf("one Registry for both: typed %v, fallback %q, weakened %q", v.Typed, v.Fallback, v.Weakened)
	}
}

// Merges and loops: ConfigReport.Gated, which the path rule reads, is true only where every path
// passes a gate.
func TestDiffGatedCoverageJoinsAndLoops(t *testing.T) {
	join := func(ins string) string {
		return `{"version": 1, "flow": "f", "entry": "a",
	  "nodes": [{"name": "a", "block": "a", "safety": "readonly"}, {"name": "x", "block": "x", "safety": "readonly", "approval": {"need": 1, "approvers": ["ops"]}},
	            {"name": "y", "block": "y", "safety": "readonly"}, {"name": "e", "block": "e", "safety": "side_effect"}],
	  "wiring": [{"edge": ["a", "x"]}, {"edge": ["a", "y"]}, {"join": "j", "inputs": [` + ins + `], "merge": "m"}, {"edge": ["j", "e"]}]}`
	}
	// A fan-in of two gated paths is gated; one ungated input makes the effect after it reachable ungated.
	gatedBoth := strings.Replace(join(`"x", "x"`), `{"edge": ["a", "y"]}, `, "", 1)
	if r := plan.InspectConfig([]byte(gatedBoth), nil); !r.Gated["e"] {
		t.Fatalf("a join fed only through the gate: e not gated (problems %v)", r.Problems)
	}
	if got := weakenedOf(t, join(`"x", "x"`), join(`"x", "y"`)); len(got) == 0 {
		t.Errorf("a join that gains an ungated input: no weakening reported")
	}
	// A loop: the gate is inside it, so the effect after the loop stays gated; a back-edge that skips
	// the gate does not exist when the loop head is the entry.
	loop := func(back string) string {
		return `{"version": 1, "flow": "f", "entry": "a",
	  "nodes": [{"name": "a", "block": "a", "safety": "readonly"}, {"name": "g", "block": "g", "safety": "readonly", "approval": {"need": 1, "approvers": ["ops"]}},
	            {"name": "s", "block": "s", "safety": "readonly"}, {"name": "e", "block": "e", "safety": "side_effect"}],
	  "wiring": [{"edge": ["a", "g"]}, {"edge": ["g", "s"]}, {"switch": "s", "when": [{"pred": "again", "to": "` + back + `", "loop_max": 3}], "else": "e"}]}`
	}
	if r := plan.InspectConfig([]byte(loop("a")), nil); !r.Gated["e"] || !r.Gated["s"] {
		t.Errorf("effect after a gated loop: gated e=%v s=%v", r.Gated["e"], r.Gated["s"])
	}
	// The loop's back-edge goes to the effect itself: reached from the loop switch, still after the gate.
	if got := weakenedOf(t, loop("a"), loop("e")); len(got) != 0 {
		t.Errorf("a back-edge to a node behind the gate: weakened %q", got)
	}
	// An edge from the entry straight to the effect, beside the loop, is a bypass.
	bypass := strings.Replace(loop("a"), `{"edge": ["a", "g"]}`, `{"edge": ["a", "g"]}, {"edge": ["a", "e"]}`, 1)
	if got := weakenedOf(t, loop("a"), bypass); len(got) != 1 || !strings.Contains(got[0], "(a -> e)") {
		t.Errorf("bypass of the loop: weakened %q", got)
	}
}

// A rename does not hide a change next to it: a gate upstream gone leaves the renamed effect
// reachable ungated; a renamed node whose safety changed is not the same node; one whose entry role,
// edges or arm predicate differ is not either.
func TestDiffRenameDoesNotHideChanges(t *testing.T) {
	base := gapCfg("", "")
	moved := strings.NewReplacer(`"name": "receipt"`, `"name": "receipt2"`, `["charge", "receipt"]`, `["charge", "receipt2"]`).Replace(base)
	ungated := strings.Replace(moved, `, "approval": {"need": 2, "approvers": ["ops", "finance", "risk"]}`, "", 1)
	got := weakenedOf(t, base, ungated)
	if len(got) != 2 || !strings.HasPrefix(got[0], "charge: approval") || !strings.HasPrefix(got[1], "receipt2: reachable without an approval gate (validate -> charge -> receipt2)") {
		t.Errorf("renamed effect behind a removed gate: %q", got)
	}
	// Renamed, relabeled read-only and reached around the gate: still a bypass of a node that sat behind it.
	around := strings.Replace(strings.NewReplacer(`"name": "receipt", "block": "receipt", "safety": "side_effect"`, `"name": "receipt2", "block": "receipt", "safety": "readonly"`, `["charge", "receipt"]`, `["charge", "receipt2"]`).Replace(base), `"wiring": [`, `"wiring": [{"edge": ["validate", "receipt2"]}, `, 1)
	got = weakenedOf(t, base, around)
	if len(got) != 1 || !strings.HasPrefix(got[0], "receipt2: new node, reachable without an approval gate (validate -> receipt2)") || !strings.Contains(got[0], "safety lowered side effect -> read-only") {
		t.Errorf("renamed, relabeled, bypassed: %q", got)
	}
	// readonly -> side_effect while renamed.
	log := func(name, safety string) string {
		return gapCfg(`, {"name": "`+name+`", "block": "log", "safety": "`+safety+`"}`, `, {"edge": ["validate", "`+name+`"]}`)
	}
	v := diffOf(t, log("log", "readonly"), log("log2", "side_effect"), nil, nil)
	if len(v.Weakened) != 1 {
		t.Errorf("renamed and made an effect: %q, changes %q", v.Weakened, lines(v.Changes))
	}
	// renamedNodes directly: the pairing's own conditions.
	pairs := func(oldCfg, newCfg string) map[string]string {
		return renamedNodes(plan.InspectConfig([]byte(oldCfg), nil), plan.InspectConfig([]byte(newCfg), nil))
	}
	two := func(entry, a, b, edge string) string {
		return `{"version": 1, "flow": "f", "entry": "` + entry + `", "nodes": [{"name": "` + a + `", "block": "w", "safety": "readonly"}, {"name": "` + b + `", "block": "w", "safety": "readonly"}], "wiring": [{"edge": ` + edge + `}]}`
	}
	// n1 -> n2 against m2 -> m1 (the same shape, the edge reversed): the blurred match pairs n1 with m1
	// and n2 with m2, which the exact check rejects.
	if m := pairs(two("n1", "n1", "n2", `["n1", "n2"]`), two("m2", "m1", "m2", `["m2", "m1"]`)); len(m) != 2 || m["n1"] != "m2" || m["n2"] != "m1" {
		t.Errorf("chain renamed in the other order: %v", m)
	}
	// Two like nodes whose only edge runs the other way round look alike when names are blurred, and
	// are not paired once the edge is checked under the pairing.
	pair := func(n1, n2, from, to string) string {
		return `{"version": 1, "flow": "f", "entry": "z", "nodes": [{"name": "z", "block": "z", "safety": "readonly"}, {"name": "q", "block": "q", "safety": "readonly"}, {"name": "` + n1 + `", "block": "w", "safety": "readonly"}, {"name": "` + n2 + `", "block": "w", "safety": "readonly"}], "wiring": [{"edge": ["z", "q"]}, {"edge": ["` + from + `", "` + to + `"]}]}`
	}
	if m := pairs(pair("r1", "r2", "r1", "r2"), pair("a1", "a2", "a2", "a1")); len(m) != 0 {
		t.Errorf("an edge the other way round: paired %v", m)
	}
	if m := pairs(pair("r1", "r2", "r1", "r2"), pair("a1", "a2", "a1", "a2")); len(m) != 2 {
		t.Errorf("an edge the same way round: paired %v", m)
	}
	// The entry moved: the node that was the entry is not the node that is not.
	one := func(entry, name string) string {
		return `{"version": 1, "flow": "f", "entry": "` + entry + `", "nodes": [{"name": "` + name + `", "block": "w", "safety": "readonly"}, {"name": "z", "block": "q", "safety": "readonly"}], "wiring": [{"edge": ["` + name + `", "z"]}]}`
	}
	if m := pairs(one("r1", "r1"), one("z", "a1")); len(m) != 0 {
		t.Errorf("entry moved: paired %v", m)
	}
	if m := pairs(one("r1", "r1"), one("a1", "a1")); len(m) != 1 || m["r1"] != "a1" {
		t.Errorf("entry renamed with the node: %v", m)
	}
	// An arm's predicate changed at the same time.
	arm := func(pred, to string) string {
		return `{"version": 1, "flow": "f", "entry": "s", "nodes": [{"name": "s", "block": "s", "safety": "readonly"}, {"name": "` + to + `", "block": "w", "safety": "readonly"}, {"name": "z", "block": "z", "safety": "readonly"}], "wiring": [{"switch": "s", "when": [{"pred": "` + pred + `", "to": "` + to + `"}], "else": "z"}]}`
	}
	if m := pairs(arm("large", "r1"), arm("tiny", "a1")); len(m) != 0 {
		t.Errorf("arm predicate changed: paired %v", m)
	}
	if m := pairs(arm("large", "r1"), arm("large", "a1")); len(m) != 1 {
		t.Errorf("arm renamed target: %v", m)
	}
}

// An unchanged-arm walk stops at a gate: an arm that leads through a gate to an effect another route
// also reaches ungated is not an ungated arm.
func TestDiffSwitchOtherArmThroughGate(t *testing.T) {
	cfg := func(p string) string {
		return `{"version": 1, "flow": "f", "entry": "validate",
	  "nodes": [{"name": "validate", "block": "validate", "safety": "readonly"},
	            {"name": "note", "block": "note", "safety": "readonly"},
	            {"name": "charge", "block": "charge", "safety": "side_effect", "approval": {"need": 1, "approvers": ["ops"]}},
	            {"name": "receipt", "block": "receipt", "safety": "side_effect"}],
	  "wiring": [{"switch": "validate", "when": [{"pred": "` + p + `", "to": "note"}], "else": "charge"}, {"edge": ["charge", "receipt"]}, {"edge": ["validate", "receipt"]}]}`
	}
	if got := weakenedOf(t, cfg("large"), cfg("tiny")); len(got) != 0 {
		t.Errorf("weakened %q; want none: the unchanged arm passes the gate", got)
	}
}
