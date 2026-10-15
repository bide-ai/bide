package runview

import (
	"context"
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
			renamed = append(renamed, c.Node+" from "+c.Details[0])
		}
	}
	if len(v.Weakened) != 0 || len(renamed) != 1 || !strings.HasPrefix(renamed[0], "receipt2 from receipt -> receipt2") {
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
	if got := weakenedOf(t, base, nogate); len(got) < 2 {
		t.Errorf("renamed and ungated: weakened %q; want the gate and the effect behind it", got)
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
	if len(got) != 1 || !strings.HasPrefix(got[0], "validate: arm conditions changed (removed large>charge:false:false:0; when tiny -> charge) while another arm reaches an ungated effect (small)") {
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
	reg := plan.NewRegistry()
	pass := func(_ context.Context, o order) (order, error) { return o, nil }
	for _, err := range []error{reg.RegisterStep("a", pass), reg.RegisterStep("b", pass), reg.RegisterStep("c", pass)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if v := diffOf(t, cfg("b", ""), cfg("c", ""), reg, reg); !v.Typed || len(v.Weakened) != 0 {
		t.Errorf("with a Registry: typed %v, weakened %q", v.Typed, v.Weakened)
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
