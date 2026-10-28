package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/taintwords"
)

// The static taint verdicts (taint_inspect.go) on small flows of int steps. Every block is a Step; the
// options say which declare a source, are read-only or are exempt.

type tn map[string][]NodeOption

func taintRegistryOf(t *testing.T, blocks tn, preds ...string) *Registry {
	t.Helper()
	reg := NewRegistry()
	step := func(_ context.Context, n int) (int, error) { return n, nil }
	for name, opts := range blocks {
		if err := reg.RegisterStep(name, step, opts...); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	for _, p := range preds {
		if err := reg.RegisterPredicate(p, func(n int) bool { return n > 0 }); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

// cfgOf is a flow config of nodes (each its own block) and the wiring elements given as JSON.
func cfgOf(entry string, nodes []string, wiring ...string) string {
	var ns []string
	for _, n := range nodes {
		ns = append(ns, fmt.Sprintf(`{"name":%q,"block":%q}`, n, n))
	}
	return fmt.Sprintf(`{"version":1,"flow":"f","in":"int","out":"int","entry":%q,"nodes":[%s],"wiring":[%s]}`, entry, strings.Join(ns, ","), strings.Join(wiring, ","))
}

func wEdge(a, b string) string { return fmt.Sprintf(`{"edge":[%q,%q]}`, a, b) }

func verdicts(r ConfigReport) map[string]string {
	out := map[string]string{}
	for n, t := range r.Taint {
		out[n] = t.Verdict
	}
	return out
}

func inspectTaint(t *testing.T, cfg string, blocks tn, opts ...InspectOption) ConfigReport {
	t.Helper()
	preds := []string{"again", "big"}
	r := InspectConfig([]byte(cfg), taintRegistryOf(t, blocks, preds...), append([]InspectOption{AllowUnusedBlocks()}, opts...)...)
	if r.Errors() {
		t.Fatalf("config has errors: %+v\n%s", r.Problems, cfg)
	}
	return r
}

var gatePol = agent.TaintPolicy{Gate: agent.SingleApproval()}

func TestTaintInspect_Chain(t *testing.T) {
	cfg := cfgOf("src", []string{"src", "mid", "pay", "look"}, wEdge("src", "mid"), wEdge("mid", "pay"), wEdge("pay", "look"))
	blocks := tn{"src": {Untrusted("inbound-email")}, "mid": {}, "pay": {}, "look": {ReadOnly()}}
	cases := []struct {
		name string
		opts []InspectOption
		want map[string]string
		why  string
	}{
		{"no policy given", nil, map[string]string{"src": taintVerdictNoSource, "mid": taintVerdictReads, "pay": taintVerdictReads, "look": taintVerdictReads}, taintReasonNoPolicy},
		{"known none", []InspectOption{InspectTaintPolicy(agent.TaintPolicy{})}, map[string]string{"src": taintVerdictNoSource, "mid": taintVerdictReads, "pay": taintVerdictReads, "look": taintVerdictReads}, taintReasonNone},
		{"gate", []InspectOption{InspectTaintPolicy(gatePol)}, map[string]string{"src": taintVerdictNoSource, "mid": taintVerdictEscalates, "pay": taintVerdictEscalates, "look": taintVerdictReads}, ""},
		{"refuse", []InspectOption{InspectTaintPolicy(agent.TaintPolicy{Refuse: true})}, map[string]string{"src": taintVerdictNoSource, "mid": taintVerdictRefused, "pay": taintVerdictRefused, "look": taintVerdictReads}, ""},
	}
	for _, c := range cases {
		r := inspectTaint(t, cfg, blocks, c.opts...)
		if got := verdicts(r); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v; want %v", c.name, got, c.want)
		}
		if c.why != "" && r.Taint["pay"].Reason != c.why {
			t.Errorf("%s: reason %q", c.name, r.Taint["pay"].Reason)
		}
	}
	// The two reasons are told apart by the prefixes the diff reads (internal/runview coverOf).
	none := inspectTaint(t, cfg, blocks, InspectTaintPolicy(agent.TaintPolicy{}))
	given := inspectTaint(t, cfg, blocks)
	if !strings.HasPrefix(none.Taint["pay"].Reason, "no taint policy") || strings.HasPrefix(given.Taint["pay"].Reason, "no taint policy") {
		t.Errorf("reasons %q / %q", none.Taint["pay"].Reason, given.Taint["pay"].Reason)
	}
	r := inspectTaint(t, cfg, blocks, InspectTaintPolicy(gatePol))
	if got := r.Taint["pay"]; !reflect.DeepEqual(got.Sources, []string{"inbound-email"}) || !reflect.DeepEqual(got.Path, []string{"src", "mid", "pay"}) {
		t.Errorf("pay: %+v", got)
	}
	if got := r.Taint["src"]; !reflect.DeepEqual(got.Declares, []string{"inbound-email"}) || len(got.Sources) != 0 || got.Reason != taintwords.SelfSource+"; "+taintCaveatNoSource {
		t.Errorf("src: %+v", got)
	}
}

// A node never reads a source that runs after it in the engine's order.
func TestTaintInspect_OrderDecidesNotDeclarationOrNames(t *testing.T) {
	// s fans out to two branches; the engine runs them in declared node order.
	blocks := tn{"s": {}, "src": {Untrusted("web")}, "pay": {}}
	srcFirst := inspectTaint(t, cfgOf("s", []string{"s", "src", "pay"}, wEdge("s", "src"), wEdge("s", "pay")), blocks, InspectTaintPolicy(gatePol))
	if got := srcFirst.Taint["pay"]; got.Verdict != taintVerdictEscalates || len(got.Path) != 0 {
		t.Errorf("source first: %+v (a fan-out sibling that runs later reads it, by order, with no edge path)", got)
	}
	payFirst := inspectTaint(t, cfgOf("s", []string{"s", "pay", "src"}, wEdge("s", "pay"), wEdge("s", "src")), blocks, InspectTaintPolicy(gatePol))
	if got := payFirst.Taint["pay"]; got.Verdict != taintVerdictNoSource || len(got.Sources) != 0 {
		t.Errorf("pay first: %+v", got)
	}
}

func TestTaintInspect_SwitchArmsAreAllLive(t *testing.T) {
	blocks := tn{"s": {}, "left": {Untrusted("l")}, "right": {}}
	cfg := cfgOf("s", []string{"s", "left", "right"},
		`{"switch":"s","when":[{"pred":"big","to":"left"}],"else":"right"}`)
	r := inspectTaint(t, cfg, blocks, InspectTaintPolicy(gatePol))
	// right is after left in the order: an over-approximation, since the arms exclude each other.
	if r.Taint["right"].Verdict != taintVerdictEscalates || r.Taint["left"].Verdict != taintVerdictNoSource {
		t.Errorf("%v", verdicts(r))
	}
}

func TestTaintInspect_Join(t *testing.T) {
	reg := taintRegistryOf(t, tn{"s": {}, "x": {Untrusted("x-src")}, "y": {}, "z": {}})
	if err := reg.RegisterJoin2("m", func(_ context.Context, a, b int) (int, error) { return a + b, nil }, Untrusted("join-src")); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterStep("after", func(_ context.Context, n int) (int, error) { return n, nil }); err != nil {
		t.Fatal(err)
	}
	cfg := `{"version":1,"flow":"f","in":"int","out":"int","entry":"s","nodes":[{"name":"s","block":"s"},{"name":"x","block":"x"},{"name":"y","block":"y"},{"name":"after","block":"after"}],
	"wiring":[{"edge":["s","x"]},{"edge":["s","y"]},{"join":"j","inputs":["x","y"],"merge":"m"},{"edge":["j","after"]}]}`
	r := InspectConfig([]byte(cfg), reg, AllowUnusedBlocks(), InspectTaintPolicy(gatePol))
	if r.Errors() {
		t.Fatalf("%+v", r.Problems)
	}
	if got := r.Taint["j"]; got.Verdict != taintVerdictEscalates || !reflect.DeepEqual(got.Sources, []string{"x-src"}) || !reflect.DeepEqual(got.Declares, []string{"join-src"}) {
		t.Errorf("join: %+v", got)
	}
	if got := r.Taint["after"]; !reflect.DeepEqual(got.Sources, []string{"join-src", "x-src"}) {
		t.Errorf("after: %+v", got)
	}
}

const loopCfg = `{"version":1,"flow":"f","in":"int","out":"int","entry":"seed","nodes":[{"name":"seed","block":"seed"},{"name":"head","block":"head"},{"name":"tail","block":"tail"},{"name":"chk","block":"chk"},{"name":"done","block":"done"}],
"wiring":[{"edge":["seed","head"]},{"edge":["head","tail"]},{"edge":["tail","chk"]},{"switch":"chk","when":[{"pred":"again","to":"head","loop_max":3}],"else":"done"}]}`

func TestTaintInspect_LoopBody(t *testing.T) {
	// The source is the last step of the body: the head reads it from the second iteration on, though it is
	// earlier in the order; a node after the loop reads it by order.
	r := inspectTaint(t, loopCfg, tn{"seed": {}, "head": {}, "tail": {Untrusted("feed")}, "chk": {}, "done": {}}, InspectTaintPolicy(gatePol))
	want := map[string]string{"seed": taintVerdictNoSource, "head": taintVerdictEscalates, "tail": taintVerdictEscalates, "chk": taintVerdictEscalates, "done": taintVerdictEscalates}
	if got := verdicts(r); !reflect.DeepEqual(got, want) {
		t.Errorf("%v; want %v", got, want)
	}
	// The source reads itself in the next iteration, and has no edge path to itself.
	if got := r.Taint["tail"]; !reflect.DeepEqual(got.Sources, []string{"feed"}) || len(got.Path) != 0 {
		t.Errorf("tail: %+v", got)
	}
	// A source before the loop reaches the whole loop; a loop of no source leaves seed alone.
	r = inspectTaint(t, loopCfg, tn{"seed": {Untrusted("in")}, "head": {}, "tail": {}, "chk": {}, "done": {}}, InspectTaintPolicy(gatePol))
	if r.Taint["head"].Verdict != taintVerdictEscalates || !reflect.DeepEqual(r.Taint["head"].Path, []string{"seed", "head"}) {
		t.Errorf("head: %+v", r.Taint["head"])
	}
}

func TestTaintInspect_ExemptAndReadOnly(t *testing.T) {
	cfg := cfgOf("src", []string{"src", "pay", "look"}, wEdge("src", "pay"), wEdge("pay", "look"))
	r := inspectTaint(t, cfg, tn{"src": {Untrusted("a")}, "pay": {TaintExempt("fixed amount")}, "look": {ReadOnly(), TaintExempt("irrelevant")}}, InspectTaintPolicy(gatePol))
	if got := r.Taint["pay"]; got.Verdict != taintVerdictExempt || got.Exempt != "fixed amount" {
		t.Errorf("pay: %+v", got)
	}
	if got := r.Taint["look"]; got.Verdict != taintVerdictReads {
		t.Errorf("a read-only node cannot escalate or be exempt from a gate: %+v", got)
	}
}

// The config's own untrusted key adds a source to a block that declares none, and is the only thing a
// structural check can state.
func TestTaintInspect_ConfigKey(t *testing.T) {
	cfg := `{"version":1,"flow":"f","in":"int","out":"int","entry":"a","nodes":[{"name":"a","block":"a","untrusted":"crm"},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}]}`
	r := inspectTaint(t, cfg, tn{"a": {}, "b": {}}, InspectTaintPolicy(gatePol))
	if got := r.Taint["a"]; !reflect.DeepEqual(got.Declares, []string{"crm"}) || r.Taint["b"].Verdict != taintVerdictEscalates {
		t.Errorf("typed: %+v %+v", got, r.Taint["b"])
	}
	s := InspectConfig([]byte(cfg), nil)
	if s.Taint["a"].Verdict != taintVerdictUnknown || !reflect.DeepEqual(s.Taint["a"].Declares, []string{"crm"}) || s.Taint["b"].Verdict != taintVerdictUnknown || s.Taint["b"].Reason != taintReasonBlocks {
		t.Errorf("structural: %+v", s.Taint)
	}
	if len(s.Taint["b"].Declares) != 0 || len(s.Taint["b"].Sources) != 0 {
		t.Errorf("structural b states a source it cannot know: %+v", s.Taint["b"])
	}
}

func TestTaintInspect_StructuralJoinKeyAndErrors(t *testing.T) {
	cfg := `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"a"},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]},{"join":"j","inputs":["a","b"],"merge":"m","untrusted":"jk"}]}`
	s := InspectConfig([]byte(cfg), nil)
	if s.Taint["j"].Verdict != taintVerdictUnknown || !reflect.DeepEqual(s.Taint["j"].Declares, []string{"jk"}) {
		t.Errorf("%+v", s.Taint)
	}
	// A typed report with errors has no verdicts.
	r := InspectConfig([]byte(cfgOf("nope", []string{"a"})), taintRegistryOf(t, tn{"a": {}}), AllowUnusedBlocks())
	if !r.Errors() || r.Taint != nil {
		t.Errorf("errors %v, taint %v", r.Errors(), r.Taint)
	}
}

func TestTaintInspect_Described(t *testing.T) {
	cfg := cfgOf("src", []string{"src", "pay"}, wEdge("src", "pay"))
	reg := taintRegistryOf(t, tn{"src": {Untrusted("web")}, "pay": {}})
	d := reg.Describe()
	// A description without a policy: pay reads untrusted, whether it escalates is not known.
	r := InspectConfigDescribed([]byte(cfg), d, AllowUnusedBlocks())
	if got := r.Taint["pay"]; got.Verdict != taintVerdictReads || got.Reason != taintReasonNoPolicy {
		t.Errorf("no policy given: %+v", got)
	}
	// The description's policy is used.
	d.TaintPolicy, _ = DescribeTaintPolicy(gatePol)
	r = InspectConfigDescribed([]byte(cfg), d, AllowUnusedBlocks())
	if r.Taint["pay"].Verdict != taintVerdictEscalates {
		t.Errorf("description policy: %+v", r.Taint["pay"])
	}
	// An explicit option wins.
	r = InspectConfigDescribed([]byte(cfg), d, AllowUnusedBlocks(), InspectTaintPolicy(agent.TaintPolicy{Refuse: true}))
	if r.Taint["pay"].Verdict != taintVerdictRefused {
		t.Errorf("option policy: %+v", r.Taint["pay"])
	}
	// A version 1 description says nothing about taint.
	d.Version, d.TaintDeclared, d.TaintPolicy = 1, false, nil
	for i := range d.Blocks {
		d.Blocks[i].Untrusted = nil
	}
	r = InspectConfigDescribed([]byte(cfg), d, AllowUnusedBlocks())
	for n, got := range r.Taint {
		if got.Verdict != taintVerdictUnknown || got.Reason != taintReasonV1 {
			t.Errorf("v1 %s: %+v", n, got)
		}
	}
	if len(r.Taint) != 2 {
		t.Errorf("v1 taint %v", r.Taint)
	}
}

// No verdict, reason or declared word is a claim of safety.
func TestTaintInspect_NoClaimWords(t *testing.T) {
	cfg := cfgOf("src", []string{"src", "pay", "look"}, wEdge("src", "pay"), wEdge("pay", "look"))
	r := inspectTaint(t, cfg, tn{"src": {Untrusted("a")}, "pay": {}, "look": {ReadOnly()}})
	b, _ := json.Marshal(r.Taint)
	for _, w := range []string{"safe", "clean", "no taint", "protected"} {
		if strings.Contains(strings.ToLower(string(b)), w) {
			t.Errorf("the taint report says %q: %s", w, b)
		}
	}
	for _, v := range []string{taintVerdictNoSource, taintCaveatNoSource, taintReasonNoPolicy, taintReasonBlocks, taintReasonV1} {
		for _, w := range []string{"safe", "clean", "no taint", "protected"} {
			if strings.Contains(strings.ToLower(v), w) {
				t.Errorf("%q says %q", v, w)
			}
		}
	}
}

// A node that is a source and can have a side effect says its own read is not gated; a read-only source does not.
func TestTaintInspect_ASourceThatCanHaveASideEffect(t *testing.T) {
	cfg := cfgOf("a", []string{"a", "b"}, wEdge("a", "b"))
	r := inspectTaint(t, cfg, tn{"a": {Untrusted("x")}, "b": {Untrusted("y"), ReadOnly()}}, InspectTaintPolicy(gatePol))
	if got := r.Taint["a"]; got.Verdict != taintVerdictNoSource || !strings.HasPrefix(got.Reason, taintwords.SelfSource) {
		t.Errorf("a: %+v", got)
	}
	r = inspectTaint(t, cfg, tn{"a": {Untrusted("x"), ReadOnly()}, "b": {ReadOnly()}}, InspectTaintPolicy(gatePol))
	if got := r.Taint["a"]; got.Reason != taintCaveatNoSource {
		t.Errorf("a read-only source: %+v", got)
	}
}
