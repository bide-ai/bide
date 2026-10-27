package runview

import (
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/plan"
)

// Reports built by hand: the diff reads plan.ConfigReport, and the engine's guard against declaring taint
// before it is enforced keeps Inspect from making them in this package.

func taintReport(nodes map[string]plan.NodeTaint, safety map[string]plan.NodeSafety, order ...string) plan.ConfigReport {
	r := plan.ConfigReport{Typed: true, Taint: nodes, Safety: safety, Gated: map[string]bool{}}
	for _, n := range order {
		r.Topology.Nodes = append(r.Topology.Nodes, plan.TopologyNode{Name: n})
		r.Gated[n] = true // behind a gate, so the path rules of the diff stay out of what is tested here
	}
	return r
}

func nt(verdict string, declares, sources []string, extra ...string) plan.NodeTaint {
	t := plan.NodeTaint{Verdict: verdict, Declares: declares, Sources: sources}
	if len(extra) > 0 {
		t.Exempt = extra[0]
	}
	if len(extra) > 1 {
		t.Reason = extra[1]
	}
	return t
}

const (
	noSrc  = "no declared source before it"
	reads  = "reads untrusted"
	escal  = "escalates"
	exempt = "exempt"
)

var effect = map[string]plan.NodeSafety{"src": plan.SafetySideEffect, "pay": plan.SafetySideEffect, "look": plan.SafetyReadOnly}

func oldFlow() plan.ConfigReport {
	return taintReport(map[string]plan.NodeTaint{"src": nt(noSrc, nil, nil), "pay": nt(noSrc, nil, nil), "look": nt(noSrc, nil, nil)}, effect, "src", "pay", "look")
}

func finishDiff(o, n plan.ConfigReport) *DiffView {
	v := &DiffView{Typed: true}
	v.finish(o, n, nil, nil)
	return v
}

const (
	noneReason  = "no taint policy: nothing gates its side effect"
	ungivenText = "taint policy not given: whether it escalates is not known"
)

// withPay is the new flow in which src declares a source and pay reads it with the given verdict.
func withPay(pay plan.NodeTaint) plan.ConfigReport {
	pay.Sources = []string{"inbound-email"}
	return taintReport(map[string]plan.NodeTaint{
		"src": nt(noSrc, []string{"inbound-email"}, nil), "pay": pay, "look": nt(reads, nil, []string{"inbound-email"}),
	}, effect, "src", "pay", "look")
}

func taintLines(v *DiffView) string {
	var lines []string
	for _, c := range v.Changes {
		lines = append(lines, c.String())
	}
	return strings.Join(lines, "\n")
}

// A side-effecting node that newly reads a source and is covered (gated, refused or exempt) is a listed change.
func TestDiffTaint_CoveredExposureIsListedNotWeakened(t *testing.T) {
	for name, c := range map[string]struct {
		pay  plan.NodeTaint
		want string
	}{
		"gate":   {plan.NodeTaint{Verdict: escal}, "changed node pay: taint: now reads inbound-email, escalates"},
		"refuse": {plan.NodeTaint{Verdict: "refused"}, "changed node pay: taint: now reads inbound-email, refused"},
		"exempt": {plan.NodeTaint{Verdict: exempt, Exempt: "fixed amount"}, "changed node pay: taint: now reads inbound-email, exempt (fixed amount)"},
	} {
		v := finishDiff(oldFlow(), withPay(c.pay))
		if len(v.Weakened) != 0 || v.Fallback != "" {
			t.Errorf("%s: weakened %v, fallback %q", name, v.Weakened, v.Fallback)
		}
		all := taintLines(v)
		if !strings.Contains(all, c.want) {
			t.Errorf("%s: changes lack %q:\n%s", name, c.want, all)
		}
		if !strings.Contains(all, "changed node src: taint: now declares source inbound-email (strengthened)") {
			t.Errorf("%s: the new source is not listed:\n%s", name, all)
		}
		// A read-only node that reads is no exposure of an effect.
		if strings.Contains(all, "node look") {
			t.Errorf("%s: a read-only node is listed:\n%s", name, all)
		}
		if len(v.Strengthened) != 1 || v.Counts.NodesChanged != 0 {
			t.Errorf("%s: strengthened %v, counts %+v (a taint line is not a changed node)", name, v.Strengthened, v.Counts)
		}
	}
}

// Uncovered: the policy is known to be absent, so nothing gates the node that newly reads a source.
func TestDiffTaint_UncoveredExposureIsWeakened(t *testing.T) {
	v := finishDiff(oldFlow(), withPay(plan.NodeTaint{Verdict: reads, Reason: noneReason}))
	if len(v.Weakened) != 1 || !strings.Contains(v.Weakened[0], "pay: now reads inbound-email and no taint policy: nothing gates its side effect") {
		t.Fatalf("weakened %v", v.Weakened)
	}
	if v.Verdict.Word != "UNTRUSTED EXPOSURE WIDENED" || v.Verdict.Class != "bad" || v.Fallback != "" {
		t.Errorf("verdict %+v, fallback %q", v.Verdict, v.Fallback)
	}
	if !slices.Equal(v.weakKinds, []string{"exposure"}) {
		t.Errorf("kinds %v", v.weakKinds)
	}
}

// The policy is not given on the new side (and on the old one, or not): the exposure cannot be judged.
func TestDiffTaint_PolicyNotGivenIsNotCertified(t *testing.T) {
	ungiven := plan.NodeTaint{Verdict: reads, Reason: ungivenText}
	oldGiven := withPay(plan.NodeTaint{Verdict: escal})
	oldGiven.Taint["pay"] = nt(escal, nil, []string{"inbound-email"})
	oldGiven.Taint["src"] = nt(noSrc, []string{"inbound-email"}, nil)
	cases := map[string]plan.ConfigReport{
		"not given on both sides, a new exposure": oldFlow(),
		"no exposure before":                      oldFlow(),
		"given on the old side, covered then":     oldGiven,
	}
	for name, old := range cases {
		v := finishDiff(old, withPay(ungiven))
		if !strings.Contains(v.Fallback, "taint could not be compared") || !strings.Contains(v.Fallback, "pay") || v.Verdict.Word != "NOT CERTIFIED" {
			t.Errorf("%s: fallback %q, verdict %q", name, v.Fallback, v.Verdict.Word)
		}
		if len(v.Weakened) != 0 {
			t.Errorf("%s: weakened %v", name, v.Weakened)
		}
	}
	// Given on the new side and not on the old one: the new side judges, and it covers the node.
	notGivenOld := oldFlow()
	v := finishDiff(notGivenOld, withPay(plan.NodeTaint{Verdict: escal}))
	if v.Fallback != "" || len(v.Weakened) != 0 {
		t.Errorf("given on the new side only: fallback %q, weakened %v", v.Fallback, v.Weakened)
	}
	// Nothing new is exposed and the policy is not given on either side: nothing to compare.
	same := withPay(ungiven)
	if v := finishDiff(same, withPay(ungiven)); v.Fallback != "" || len(v.Changes) != 0 {
		t.Errorf("an unchanged flow: fallback %q, changes %v", v.Fallback, v.Changes)
	}
}

// A node that read a source covered, and is no longer covered by the policy.
func TestDiffTaint_APolicyThatNoLongerCoversANode(t *testing.T) {
	covered := withPay(plan.NodeTaint{Verdict: escal})
	gone := withPay(plan.NodeTaint{Verdict: reads, Reason: noneReason})
	v := finishDiff(covered, gone)
	if len(v.Weakened) != 1 || !strings.Contains(v.Weakened[0], "pay: reads inbound-email and no taint policy: nothing gates its side effect, was escalates") {
		t.Fatalf("weakened %v", v.Weakened)
	}
	ungiven := withPay(plan.NodeTaint{Verdict: reads, Reason: ungivenText})
	v = finishDiff(covered, ungiven)
	if !strings.Contains(v.Fallback, "taint could not be compared") || len(v.Weakened) != 0 {
		t.Errorf("not given now: fallback %q, weakened %v", v.Fallback, v.Weakened)
	}
	// Known none before and now: not a change. Refuse to a gate: still covered.
	if v := finishDiff(gone, withPay(plan.NodeTaint{Verdict: reads, Reason: noneReason})); len(v.Changes) != 0 {
		t.Errorf("none to none: %v", v.Changes)
	}
	if v := finishDiff(withPay(plan.NodeTaint{Verdict: "refused"}), covered); len(v.Changes) != 0 || v.Fallback != "" {
		t.Errorf("refused to escalates: %v", v.Changes)
	}
}

func TestDiffTaint_ARemovedSourceIsWeakened(t *testing.T) {
	o := taintReport(map[string]plan.NodeTaint{
		"src": nt(noSrc, []string{"inbound-email"}, nil), "pay": nt(escal, nil, []string{"inbound-email"}), "look": nt(noSrc, nil, nil),
	}, effect, "src", "pay", "look")
	v := finishDiff(o, oldFlow())
	if len(v.Weakened) != 1 || !strings.Contains(v.Weakened[0], "src: no longer declares source inbound-email") {
		t.Fatalf("weakened %v", v.Weakened)
	}
	if v.Verdict.Word != "UNTRUSTED EXPOSURE WIDENED" || v.Fallback != "" {
		t.Errorf("verdict %+v, fallback %q (a node that reads nothing now has no cover to compare)", v.Verdict, v.Fallback)
	}
}

// An unchanged exposure, a node that read the source before, and a node that was already exempt are not new.
func TestDiffTaint_UnchangedExposureIsNotAChange(t *testing.T) {
	r := func() plan.ConfigReport {
		return taintReport(map[string]plan.NodeTaint{
			"src": nt(noSrc, []string{"a"}, nil), "pay": nt(exempt, nil, []string{"a"}, "why"), "look": nt(reads, nil, []string{"a"}),
		}, effect, "src", "pay", "look")
	}
	if v := finishDiff(r(), r()); len(v.Changes) != 0 {
		t.Errorf("changes %v", v.Changes)
	}
}

// A node that becomes able to have a side effect while reading a source is newly exposed; a second source is too.
func TestDiffTaint_BecomingAnEffectOrReadingAnotherSource(t *testing.T) {
	o := taintReport(map[string]plan.NodeTaint{
		"src": nt(noSrc, []string{"a"}, nil), "pay": nt(reads, nil, []string{"a"}), "look": nt(reads, nil, []string{"a"}),
	}, map[string]plan.NodeSafety{"src": plan.SafetySideEffect, "pay": plan.SafetyReadOnly, "look": plan.SafetyReadOnly}, "src", "pay", "look")
	n := taintReport(map[string]plan.NodeTaint{
		"src": nt(noSrc, []string{"a", "b"}, nil), "pay": nt(reads, nil, []string{"a", "b"}, "", noneReason), "look": nt(exempt, nil, []string{"a", "b"}, "y"),
	}, effect, "src", "pay", "look")
	n.Safety["look"] = plan.SafetyReadOnly
	v := finishDiff(o, n)
	if len(v.Weakened) != 1 || !strings.Contains(v.Weakened[0], "pay: now reads a, b and no taint policy") {
		t.Fatalf("weakened %v", v.Weakened)
	}
	// Reading a new source with the old one already read.
	o2 := taintReport(map[string]plan.NodeTaint{"src": nt(noSrc, []string{"a"}, nil), "pay": nt(reads, nil, []string{"a"}, "", noneReason), "look": nt(noSrc, nil, nil)}, effect, "src", "pay", "look")
	n2 := taintReport(map[string]plan.NodeTaint{"src": nt(noSrc, []string{"a", "b"}, nil), "pay": nt(reads, nil, []string{"a", "b"}, "", noneReason), "look": nt(noSrc, nil, nil)}, effect, "src", "pay", "look")
	v = finishDiff(o2, n2)
	if len(v.Weakened) != 1 || !strings.Contains(v.Weakened[0], "now reads b ") {
		t.Fatalf("second source: %v", v.Weakened)
	}
}

// A renamed node is compared with its old self, and an unknown side compares nothing but the declared sources.
func TestDiffTaint_RenamesAndUnknown(t *testing.T) {
	unk := func(declares ...string) plan.NodeTaint {
		return plan.NodeTaint{Verdict: "unknown", Declares: declares, Reason: "blocks not known: check with -blocks or a Registry"}
	}
	o := taintReport(map[string]plan.NodeTaint{"src": unk("a"), "pay": unk()}, map[string]plan.NodeSafety{}, "src", "pay")
	n := taintReport(map[string]plan.NodeTaint{"src": unk(), "pay": unk()}, map[string]plan.NodeSafety{}, "src", "pay")
	v := finishDiff(o, n)
	if len(v.Weakened) != 1 || !strings.Contains(v.Weakened[0], "src: no longer declares source a") || v.Fallback != "" {
		t.Errorf("structural key removed: %v (%q)", v.Weakened, v.Fallback)
	}
	// Nothing known on the new side: no exposure verdict is made.
	only := taintReport(nil, nil, "src", "pay")
	if v := finishDiff(o, only); len(v.Weakened) != 0 {
		t.Errorf("a side with no taint: %v", v.Weakened)
	}
}

// Nothing is compared with a side that has no taint, or whose taint is unknown.
func TestDiffTaint_NothingIsComparedWithASideThatDoesNotKnow(t *testing.T) {
	uncoveredNew := withPay(plan.NodeTaint{Verdict: reads, Reason: noneReason})
	// The old side has no taint at all (a report with errors).
	noTaint := oldFlow()
	noTaint.Taint = nil
	if v := finishDiff(noTaint, uncoveredNew); len(v.Changes) != 0 || len(v.Weakened) != 0 || v.Fallback != "" {
		t.Errorf("old side with no taint: %v %v %q", v.Changes, v.Weakened, v.Fallback)
	}
	// The old side is structural: every node unknown.
	unk := plan.NodeTaint{Verdict: "unknown", Reason: "blocks not known: check with -blocks or a Registry"}
	structural := taintReport(map[string]plan.NodeTaint{"src": unk, "pay": unk, "look": unk}, effect, "src", "pay", "look")
	if v := finishDiff(structural, uncoveredNew); len(v.Weakened) != 0 || v.Fallback != "" {
		t.Errorf("old side unknown: %v %q", v.Weakened, v.Fallback)
	}
}

// A node added with a source is a new source, listed as such.
func TestDiffTaint_ANewNodeThatDeclaresASource(t *testing.T) {
	n := oldFlow()
	n.Topology.Nodes = append(n.Topology.Nodes, plan.TopologyNode{Name: "feed"})
	n.Taint["feed"] = nt(noSrc, []string{"rss"}, nil)
	n.Safety["feed"] = plan.SafetyReadOnly
	n.Gated["feed"] = true
	v := finishDiff(oldFlow(), n)
	if all := taintLines(v); !strings.Contains(all, "changed node feed: taint: now declares source rss (strengthened)") {
		t.Errorf("changes:\n%s", all)
	}
}
