package runview

import (
	"context"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/plan"
)

// The diff and the check over real reports: a Registry whose block fetch declares a source, and configs that
// order the nodes so refund does, or does not, run after it.

func realRegistry(t *testing.T) *plan.Registry {
	t.Helper()
	reg := plan.NewRegistry()
	step := func(_ context.Context, n int) (int, error) { return n, nil }
	for _, err := range []error{
		reg.RegisterStep("fetch", step, plan.Untrusted("inbound-email")),
		reg.RegisterStep("plain", step),
		reg.RegisterStep("refund", step),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

// readsBefore: fetch runs first, so refund reads inbound-email. readsAfter: fetch runs last.
const (
	readsBefore = `{"version":1,"flow":"f","in":"int","out":"int","entry":"fetch","nodes":[{"name":"fetch","block":"fetch"},{"name":"refund","block":"refund"}],"wiring":[{"edge":["fetch","refund"]}]}`
	readsAfter  = `{"version":1,"flow":"f","in":"int","out":"int","entry":"refund","nodes":[{"name":"refund","block":"refund"},{"name":"fetch","block":"fetch"}],"wiring":[{"edge":["refund","fetch"]}]}`
)

func realDiff(t *testing.T, old, new Config) *DiffView {
	t.Helper()
	old.Name, new.Name = "old.json", "new.json"
	v, err := BuildDiff(Diff{Old: old, New: new})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Config.TaintPolicy reaches the analysis: the report, the check text and the diff all follow it.
func TestRealTaint_ThePolicyOfTheConfigIsUsed(t *testing.T) {
	reg := realRegistry(t)
	gate, none := agent.TaintPolicy{Gate: agent.SingleApproval()}, agent.TaintPolicy{}
	for name, c := range map[string]struct {
		policy *agent.TaintPolicy
		want   string
	}{"gate": {&gate, "escalates"}, "refuse": {&agent.TaintPolicy{Refuse: true}, "refused"}, "none": {&none, "reads untrusted"}, "not given": {nil, "reads untrusted"}} {
		cfg := Config{Name: "f.json", Data: []byte(readsBefore), Registry: reg, TaintPolicy: c.policy, AllowUnusedBlocks: true}
		r := cfg.Inspect()
		if got := r.Taint["refund"].Verdict; got != c.want {
			t.Errorf("%s: refund %q, want %q (%+v)", name, got, c.want, r.Taint["refund"])
		}
		v := BuildConfig(cfg)
		if v.Taint == nil || !v.Taint.Known || len(v.Taint.Nodes) != 2 {
			t.Errorf("%s: view %+v", name, v.Taint)
		}
		var out strings.Builder
		Check(&out, cfg)
		if !strings.Contains(out.String(), "refund: "+c.want) {
			t.Errorf("%s: check printed\n%s", name, out.String())
		}
	}
	// A description's own policy is used, and the config's wins over it.
	d := reg.Describe()
	d.TaintPolicy, _ = plan.DescribeTaintPolicy(gate)
	if got := (Config{Name: "f.json", Data: []byte(readsBefore), Description: &d, AllowUnusedBlocks: true}).Inspect().Taint["refund"].Verdict; got != "escalates" {
		t.Errorf("description policy: %q", got)
	}
	if got := (Config{Name: "f.json", Data: []byte(readsBefore), Description: &d, AllowUnusedBlocks: true, TaintPolicy: &none}).Inspect().Taint["refund"].Verdict; got != "reads untrusted" {
		t.Errorf("the config's policy over the description's: %q", got)
	}
}

// A real diff: refund newly reads a source. Covered by a gate: listed. With the policy known to be absent:
// weakened. Policy not given: not certified.
func TestRealTaint_TheDiffFollowsThePolicy(t *testing.T) {
	reg := realRegistry(t)
	gate, none := agent.TaintPolicy{Gate: agent.SingleApproval()}, agent.TaintPolicy{}
	cfg := func(text string, p *agent.TaintPolicy) Config {
		return Config{Data: []byte(text), Registry: reg, TaintPolicy: p, AllowUnusedBlocks: true}
	}
	v := realDiff(t, cfg(readsAfter, &gate), cfg(readsBefore, &gate))
	if len(v.Weakened) != 0 || v.Fallback != "" || !strings.Contains(taintLines(v), "refund: taint: now reads inbound-email, escalates") {
		t.Errorf("gate: weakened %v fallback %q\n%s", v.Weakened, v.Fallback, taintLines(v))
	}
	v = realDiff(t, cfg(readsAfter, &none), cfg(readsBefore, &none))
	if len(v.Weakened) != 1 || v.Verdict.Word != "UNTRUSTED EXPOSURE WIDENED" {
		t.Errorf("none: weakened %v verdict %q", v.Weakened, v.Verdict.Word)
	}
	v = realDiff(t, cfg(readsAfter, nil), cfg(readsBefore, nil))
	if !strings.Contains(v.Fallback, "taint could not be compared") || v.Verdict.Word != "NOT CERTIFIED" || len(v.Weakened) != 0 {
		t.Errorf("not given: fallback %q verdict %q weakened %v", v.Fallback, v.Verdict.Word, v.Weakened)
	}
	// A Registry for the new side only: the pair is compared structurally, and a source it states is a loss.
	v = realDiff(t, Config{Data: []byte(readsAfter)}, cfg(readsBefore, &gate))
	if !strings.Contains(v.Fallback, "taint could not be compared: the blocks are known on only one side") {
		t.Errorf("one side only: fallback %q note %q", v.Fallback, v.TaintNote)
	}
	// ... and when the side that knows states no source there is only the informational line.
	quiet := `{"version":1,"flow":"f","in":"int","out":"int","entry":"plain","nodes":[{"name":"plain","block":"plain"},{"name":"refund","block":"refund"}],"wiring":[{"edge":["plain","refund"]}]}`
	v = realDiff(t, Config{Data: []byte(quiet)}, cfg(quiet, &gate))
	if v.Fallback != "" || !strings.Contains(v.TaintNote, "one side") {
		t.Errorf("a quiet side: fallback %q note %q", v.Fallback, v.TaintNote)
	}
}

// Descriptions of version 1 on both sides say why the taint is not compared; a version 1 description beside a
// side with no blocks is the one-sided case.
func TestRealTaint_Version1Descriptions(t *testing.T) {
	d := realRegistry(t).Describe()
	d.Version, d.TaintDeclared = 1, false
	for i := range d.Blocks {
		d.Blocks[i].Untrusted = nil
	}
	both := func() *DiffView {
		return realDiff(t, Config{Data: []byte(readsAfter), Description: &d, AllowUnusedBlocks: true}, Config{Data: []byte(readsBefore), Description: &d, AllowUnusedBlocks: true})
	}
	if v := both(); v.Fallback != "" || v.TaintNote != "taint not compared (the description does not carry taint declarations)" {
		t.Errorf("both v1: fallback %q note %q", v.Fallback, v.TaintNote)
	}
	v := realDiff(t, Config{Data: []byte(readsAfter), Description: &d, AllowUnusedBlocks: true}, Config{Data: []byte(readsBefore)})
	if v.Fallback != "" || !strings.Contains(v.TaintNote, "one side") {
		t.Errorf("v1 beside structural: fallback %q note %q", v.Fallback, v.TaintNote)
	}
	// Any node says it, not the first one the map gives.
	o := unknownFlow(nil)
	n := unknownFlow(nil)
	for k, tt := range n.Taint {
		if k == "look" {
			tt.Reason = "the description does not carry taint declarations (version 1)"
			n.Taint[k] = tt
		}
	}
	if note, _ := taintBasis(o, n, false, false); !strings.Contains(note, "does not carry taint declarations") {
		t.Errorf("one node with the reason: %q", note)
	}
}
