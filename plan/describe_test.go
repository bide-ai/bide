package plan

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

type dsIn struct{ ID int }
type dsMid struct{ ID int }
type dsOut struct{ Status string }

func dsStep(_ context.Context, in dsIn) (dsMid, error)          { return dsMid{in.ID}, nil }
func dsPay(_ context.Context, m dsMid) (dsOut, error)           { return dsOut{"paid"}, nil }
func dsLook(_ context.Context, m dsMid) (dsMid, error)          { return m, nil }
func dsJoin(_ context.Context, a dsMid, b dsOut) (dsOut, error) { return b, nil }

// describedRegistry registers one of every shape Describe reports: each kind, each safety, a
// single-decision and an m-of-n gate with a deadline, a tool's own gate, a predicate and a merge.
func describedRegistry(t *testing.T, reverse bool) *Registry {
	t.Helper()
	reg := NewRegistry()
	regs := []func() error{
		func() error { return reg.RegisterStep("prepare", dsStep) },
		func() error { return reg.RegisterStep("lookup", dsLook, ReadOnly()) },
		func() error {
			return reg.RegisterStep("pay", dsPay, Idempotent(), Approval(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "fin", "risk"}, ExcludePrincipal: true, Timeout: 90 * time.Minute}))
		},
		func() error { return reg.RegisterStep("sign", dsPay, Approval(agent.SingleApproval())) },
		func() error { return reg.RegisterModel[dsMid, dsMid]("draft", "draft {{.}}") },
		func() error {
			tool := agent.MustFunc("refund", "refund", func(context.Context, dsMid) (dsOut, error) { return dsOut{}, nil },
				agent.WithApproval(&agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops", "fin"}}))
			return reg.RegisterTool[dsMid, dsOut]("refund", tool)
		},
		func() error { return reg.RegisterPredicate("big", func(m dsMid) bool { return m.ID > 9 }) },
		func() error { return reg.RegisterPredicate("small", func(m dsMid) bool { return m.ID < 2 }) },
		func() error {
			return reg.RegisterJoin3("all3", func(_ context.Context, a dsMid, b dsMid, c dsOut) (dsMid, error) { return a, nil }, Idempotent())
		},
		func() error { return reg.RegisterJoin2("both", dsJoin, ReadOnly(), Approval(agent.SingleApproval())) },
	}
	if reverse {
		for i := len(regs) - 1; i >= 0; i-- {
			if err := regs[i](); err != nil {
				t.Fatalf("register: %v", err)
			}
		}
		return reg
	}
	for _, f := range regs {
		if err := f(); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	return reg
}

func TestDescribeReportsEveryRegistration(t *testing.T) {
	d := describedRegistry(t, false).Describe()
	if d.Version != DescriptionVersion || len(d.Errors) != 0 {
		t.Fatalf("version %d, errors %v", d.Version, d.Errors)
	}
	pkg := reflect.TypeFor[dsIn]().PkgPath()
	mid, out, in := pkg+".dsMid", pkg+".dsOut", pkg+".dsIn"
	want := []BlockDescription{
		{Name: "draft", Kind: "model", In: mid, Out: mid, Safety: "readonly"},
		{Name: "lookup", Kind: "step", In: mid, Out: mid, Safety: "readonly"},
		{Name: "pay", Kind: "step", In: mid, Out: out, Safety: "idempotent", Gate: &GateDescription{Need: 2, Approvers: []string{"ops", "fin", "risk"}, ExcludePrincipal: true, Timeout: "1h30m0s"}},
		{Name: "prepare", Kind: "step", In: in, Out: mid, Safety: "side_effect"},
		{Name: "refund", Kind: "tool", In: mid, Out: out, Safety: "side_effect", Gate: &GateDescription{Need: 1, Approvers: []string{"ops", "fin"}}},
		{Name: "sign", Kind: "step", In: mid, Out: out, Safety: "side_effect", Gate: &GateDescription{Single: true, Need: 1}},
	}
	if !reflect.DeepEqual(d.Blocks, want) {
		g, _ := json.MarshalIndent(d.Blocks, "", " ")
		w, _ := json.MarshalIndent(want, "", " ")
		t.Errorf("blocks:\n%s\nwant:\n%s", g, w)
	}
	if want := []PredicateDescription{{"big", mid}, {"small", mid}}; !reflect.DeepEqual(d.Predicates, want) {
		t.Errorf("predicates %+v", d.Predicates)
	}
	wantM := []MergeDescription{{Name: "all3", Inputs: []string{mid, mid, out}, Out: mid, Safety: "idempotent"}, {Name: "both", Inputs: []string{mid, out}, Out: out, Safety: "readonly", Gate: &GateDescription{Single: true, Need: 1}}}
	if !reflect.DeepEqual(d.Merges, wantM) {
		t.Errorf("merges %+v", d.Merges)
	}
}

// The same registrations in another order describe the same bytes, and the empty Registry
// describes arrays, not nulls, so a reader never has to tell "none" from "absent".
func TestDescribeIsDeterministicAndEmptyIsArrays(t *testing.T) {
	a, _ := json.Marshal(describedRegistry(t, false).Describe())
	b, _ := json.Marshal(describedRegistry(t, true).Describe())
	if string(a) != string(b) {
		t.Errorf("registration order changed the description:\n%s\n%s", a, b)
	}
	e, _ := json.Marshal(NewRegistry().Describe())
	if string(e) != `{"version":1,"blocks":[],"predicates":[],"merges":[]}` {
		t.Errorf("empty description %s", e)
	}
}

// A duplicate registration never replaces the first; the description keeps the first and reports
// the error, as Load does.
func TestDescribeKeepsTheFirstOfADuplicate(t *testing.T) {
	reg := NewRegistry()
	_ = reg.RegisterStep("x", dsStep, ReadOnly())
	_ = reg.RegisterStep("x", dsPay)
	_ = reg.RegisterPredicate("p", func(m dsMid) bool { return true })
	_ = reg.RegisterPredicate("p", func(m dsIn) bool { return true })
	d := reg.Describe()
	if len(d.Blocks) != 1 || d.Blocks[0].Safety != "readonly" || !strings.HasSuffix(d.Blocks[0].Out, "dsMid") {
		t.Errorf("block %+v", d.Blocks)
	}
	if len(d.Predicates) != 1 || !strings.HasSuffix(d.Predicates[0].Switched, "dsMid") {
		t.Errorf("predicate %+v", d.Predicates)
	}
	if len(d.Errors) != 2 || !strings.Contains(d.Errors[0], `duplicate block registration "x"`) || !strings.Contains(d.Errors[1], `duplicate predicate registration "p"`) {
		t.Errorf("errors %q", d.Errors)
	}
}

// Describe copies: changing what it returned changes nothing the Registry will do.
func TestDescribeReturnsCopies(t *testing.T) {
	reg := describedRegistry(t, false)
	d := reg.Describe()
	for i := range d.Blocks {
		if g := d.Blocks[i].Gate; g != nil && len(g.Approvers) > 0 {
			g.Approvers[0] = "mallory"
			g.Need = 9
		}
	}
	if g := reg.blocks["pay"].approval; g.Approvers[0] != "ops" || g.Need != 2 {
		t.Errorf("the registry's gate changed: %+v", g)
	}
	if again := reg.Describe(); again.Blocks[2].Gate.Approvers[0] != "ops" {
		t.Errorf("a second description saw the first's edit: %+v", again.Blocks[2].Gate)
	}
}

// Describe is checked against Load, the reference: for a config that Load accepts, each node's
// block is described with the types, safety and gate the loaded flow's topology and risk map
// carry. A node's types come from its block, so the two readings must agree.
func TestDescribeMatchesWhatLoadResolves(t *testing.T) {
	reg := describedRegistry(t, false)
	delete(reg.merges, "both") // a join cannot follow a Switch's arms; TestDescribeMatchesLoadForAJoin has it
	cfg := `{"version":1,"flow":"d","in":"plan.dsIn","out":"plan.dsOut","entry":"prepare",
	 "nodes":[{"name":"prepare","block":"prepare"},{"name":"look","block":"lookup"},{"name":"pay","block":"pay"},{"name":"sign","block":"sign"},{"name":"refund","block":"refund"},{"name":"draft","block":"draft"}],
	 "wiring":[{"edge":["prepare","look"]},{"switch":"look","when":[{"pred":"big","to":"draft"},{"pred":"small","to":"refund"}],"else":"sign"},{"edge":["draft","pay"]}]}`
	// Load is the reference. A Model node needs a bound model and an m-of-n gate its approvers'
	// verifiers; the editor's checks bind neither, so the test binds stand-ins.
	flow, err := Load[dsIn, dsOut]([]byte(cfg), reg, WithLoadedModel(&fakeModel{reply: `{}`}),
		WithLoadedApproverVerifiers(func(string) (agent.ApproverVerifier, bool) { return nil, false }))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	topo := flow.Topology()
	safety := map[string]NodeSafety{}
	for _, n := range flow.core.nodes {
		safety[n.name] = safetyClass(n.safety.ReadOnly, n.safety.Idempotent)
	}
	d := reg.Describe()
	blocks := map[string]BlockDescription{}
	for _, b := range d.Blocks {
		blocks[b.Name] = b
	}
	merges := map[string]MergeDescription{}
	for _, m := range d.Merges {
		merges[m.Name] = m
	}
	checked := 0
	for _, n := range topo.Nodes {
		var gate *TopologyGate
		switch n.Kind {
		case NodeKindJoin:
			m, ok := merges[n.Block]
			if !ok {
				t.Fatalf("join %s: merge %q not described", n.Name, n.Block)
			}
			if m.Out != n.OutCanonical {
				t.Errorf("join %s: out %q, Load has %q", n.Name, m.Out, n.OutCanonical)
			}
			for _, j := range topo.Joins {
				if j.Name == n.Name && !reflect.DeepEqual(m.Inputs, j.InCanonical) {
					t.Errorf("join %s: inputs %q, Load has %q", n.Name, m.Inputs, j.InCanonical)
				}
			}
			gate = describedGate(m.Gate)
			if got := safetyOf(m.Safety); got != safety[n.Name] {
				t.Errorf("join %s: safety %s, Load has %s", n.Name, got, safety[n.Name])
			}
		case NodeKindSwitch:
			continue
		default:
			b, ok := blocks[n.Block]
			if !ok {
				t.Fatalf("node %s: block %q not described", n.Name, n.Block)
			}
			if b.In != n.InCanonical || b.Out != n.OutCanonical || b.Kind != string(n.Kind) {
				t.Errorf("node %s: described %s %s -> %s, Load has %s %s -> %s", n.Name, b.Kind, b.In, b.Out, n.Kind, n.InCanonical, n.OutCanonical)
			}
			gate = describedGate(b.Gate)
			if got := safetyOf(b.Safety); got != safety[n.Name] {
				t.Errorf("node %s: safety %s, Load has %s", n.Name, got, safety[n.Name])
			}
		}
		if !reflect.DeepEqual(gate, n.Gate) {
			t.Errorf("node %s: gate %+v, Load has %+v", n.Name, gate, n.Gate)
		}
		checked++
	}
	if checked < 6 {
		t.Errorf("only %d nodes compared", checked)
	}
	// A predicate's switched type is the output type of the node it switches over.
	preds := map[string]string{}
	for _, p := range d.Predicates {
		preds[p.Name] = p.Switched
	}
	for _, br := range topo.Branches {
		for _, arm := range br.Arms {
			if arm.Pred == "" {
				continue
			}
			var out string
			for _, n := range topo.Nodes {
				if n.Name == br.Over {
					out = n.OutCanonical
				}
			}
			if preds[arm.Pred] != out {
				t.Errorf("predicate %s: switched %q, Load switches %q", arm.Pred, preds[arm.Pred], out)
			}
		}
	}
}

func safetyOf(word string) NodeSafety {
	switch word {
	case "readonly":
		return SafetyReadOnly
	case "idempotent":
		return SafetyIdempotent
	}
	return SafetySideEffect
}

// describedGate is the TopologyGate a GateDescription stands for.
func describedGate(g *GateDescription) *TopologyGate {
	if g == nil {
		return nil
	}
	tg := &TopologyGate{Single: g.Single, ExcludePrincipal: g.ExcludePrincipal}
	if !g.Single {
		tg.Need, tg.Approvers = g.Need, g.Approvers
	}
	if g.Timeout != "" {
		d, _ := time.ParseDuration(g.Timeout)
		tg.TimeoutMS = d.Milliseconds()
	}
	return tg
}

// The merge side of the same check, on the fan-in diamond: the described merge has the input
// types, output type and (default) safety of the join Load builds.
func TestDescribeMatchesLoadForAJoin(t *testing.T) {
	reg := diamondRegistry(t)
	flow, err := Load[int, string]([]byte(diamondConfig), reg)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	topo := flow.Topology()
	d := reg.Describe()
	if len(d.Merges) != 1 || d.Merges[0].Name != "mergeBlock" || len(topo.Joins) != 1 {
		t.Fatalf("merges %+v, joins %+v", d.Merges, topo.Joins)
	}
	m, j := d.Merges[0], topo.Joins[0]
	if !reflect.DeepEqual(m.Inputs, j.InCanonical) || m.Out != "string" || m.Safety != "side_effect" || m.Gate != nil {
		t.Errorf("described %+v, Load has inputs %q", m, j.InCanonical)
	}
	for _, n := range flow.core.nodes {
		if n.name == j.Name && (n.safety.ReadOnly || n.safety.Idempotent) {
			t.Errorf("Load's join is %+v, described %q", n.safety, m.Safety)
		}
	}
}
