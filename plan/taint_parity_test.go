package plan

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// Parity of the static view with the runtime (docs/design/flow-taint.md, section 8): the analysis in
// ConfigReport.Taint is the may-version of the rule the engine enforces, so on a flow that runs
//   - every node the engine holds at its taint instance is one the analysis calls "escalates" (it never says
//     "no declared source before it" of a node the engine gates, except for the run-input case it names);
//   - every node that ran after a source ran, and that the analysis calls "escalates", was held.

// parityFlow is a flow config, the options of its blocks, and the integer the run starts with.
type parityFlow struct {
	name   string
	cfg    string
	blocks tn
	preds  []string
	join   bool // register the merge "m" for a node "j"
	// none: the engine holds nothing, because no source runs before a node that can have a side effect.
	none bool
	// want are the nodes the engine holds, sorted, when the test pins them.
	want string
	in   int
	opts []agent.RunOption
}

// heldNode is the node a taint instance key is about: "node:pay:taint" and "node:iter:2:pay:taint" are pay's.
func heldNode(key string) (string, bool) {
	k, ok := strings.CutPrefix(key, "node:")
	if !ok {
		return "", false
	}
	k, ok = strings.CutSuffix(k, ":taint")
	if !ok {
		return "", false
	}
	if rest, ok := strings.CutPrefix(k, "iter:"); ok {
		_, name, ok := strings.Cut(rest, ":")
		if !ok {
			return "", false
		}
		k = name
	}
	return k, true
}

// parityRun runs the flow under a gate policy, approving each hold, and returns the nodes held and the nodes whose
// body ran.
func parityRun(t *testing.T, f parityFlow, policy agent.TaintPolicy) (held, ran map[string]bool) {
	t.Helper()
	ran = map[string]bool{}
	reg := NewRegistry()
	for name, opts := range f.blocks {
		name := name
		if err := reg.RegisterStep(name, func(_ context.Context, n int) (int, error) { ran[name] = true; return n + 1, nil }, opts...); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range f.preds {
		limit := 6
		if p == "big" {
			limit = 100
		}
		if err := reg.RegisterPredicate(p, func(n int) bool { return n < limit }); err != nil {
			t.Fatal(err)
		}
	}
	if f.join {
		if err := reg.RegisterJoin2("m", func(_ context.Context, a, b int) (int, error) { ran["j"] = true; return a + b, nil }); err != nil {
			t.Fatal(err)
		}
	}
	flow, err := Load[int, int]([]byte(f.cfg), reg, WithLoadedTaintPolicy(policy))
	if err != nil {
		t.Fatalf("%s: %v", f.name, err)
	}
	ctx, j := context.Background(), agenttest.MemJournal()
	held = map[string]bool{}
	for i := 0; i < 40; i++ {
		_, err := flow.Run(ctx, j, "r1", f.in, f.opts...)
		if err == nil {
			return held, ran
		}
		var p *agent.ApprovalPending
		if !errors.As(err, &p) {
			t.Fatalf("%s: run: %v", f.name, err)
		}
		name, ok := heldNode(p.ToolUseID)
		if !ok {
			t.Fatalf("%s: paused at %q, which is not a taint instance", f.name, p.ToolUseID)
		}
		held[name] = true
		if err := agent.Approve(ctx, j, "r1", p.ToolUseID, true); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatalf("%s: the run did not finish", f.name)
	return nil, nil
}

func TestTaintParityWithTheRuntime(t *testing.T) {
	loop := `{"version":1,"flow":"f","in":"int","out":"int","entry":"seed","nodes":[{"name":"seed","block":"seed"},{"name":"head","block":"head"},{"name":"tail","block":"tail"},{"name":"chk","block":"chk"},{"name":"done","block":"done"}],
"wiring":[{"edge":["seed","head"]},{"edge":["head","tail"]},{"edge":["tail","chk"]},{"switch":"chk","when":[{"pred":"again","to":"head","loop_max":5}],"else":"done"}]}`
	join := `{"version":1,"flow":"f","in":"int","out":"int","entry":"s","nodes":[{"name":"s","block":"s"},{"name":"x","block":"x"},{"name":"y","block":"y"},{"name":"after","block":"after"}],
"wiring":[{"edge":["s","x"]},{"edge":["s","y"]},{"join":"j","inputs":["x","y"],"merge":"m"},{"edge":["j","after"]}]}`
	sw := func(pred string) string {
		return `{"version":1,"flow":"f","in":"int","out":"int","entry":"s","nodes":[{"name":"s","block":"s"},{"name":"a","block":"a"},{"name":"b","block":"b"},{"name":"c","block":"c"}],
"wiring":[{"switch":"s","when":[{"pred":"` + pred + `","to":"a"}],"else":"b"},{"edge":["b","c"]}]}`
	}
	src := Untrusted("inbound-email")
	flows := []parityFlow{
		{name: "chain", want: "mid,pay", cfg: cfgOf("src", []string{"src", "mid", "pay", "look"}, wEdge("src", "mid"), wEdge("mid", "pay"), wEdge("pay", "look")),
			blocks: tn{"src": {src}, "mid": {}, "pay": {}, "look": {ReadOnly()}}},
		{name: "source in the middle", cfg: cfgOf("a", []string{"a", "src", "pay"}, wEdge("a", "src"), wEdge("src", "pay")),
			blocks: tn{"a": {}, "src": {src}, "pay": {}}},
		{name: "fan-out sibling declared after the source", cfg: cfgOf("s", []string{"s", "src", "pay"}, wEdge("s", "src"), wEdge("s", "pay")),
			blocks: tn{"s": {}, "src": {src}, "pay": {}}},
		{name: "fan-out sibling declared before the source", none: true, cfg: cfgOf("s", []string{"s", "pay", "src"}, wEdge("s", "pay"), wEdge("s", "src")),
			blocks: tn{"s": {}, "src": {src}, "pay": {}}},
		{name: "join", want: "after,j,y", cfg: join, join: true, blocks: tn{"s": {}, "x": {src}, "y": {}, "after": {}}},
		{name: "loop: the head reads the tail's source from the second iteration", want: "chk,done,head,tail", cfg: loop, preds: []string{"again"},
			blocks: tn{"seed": {}, "head": {}, "tail": {src}, "chk": {}, "done": {}}},
		{name: "loop: a source before it", cfg: loop, preds: []string{"again"},
			blocks: tn{"seed": {src}, "head": {}, "tail": {}, "chk": {}, "done": {}}},
		{name: "switch: the arm with the source is taken", none: true, cfg: sw("again"), preds: []string{"again"}, in: 0,
			blocks: tn{"s": {}, "a": {src}, "b": {}, "c": {}}},
		{name: "switch: the other arm is taken", none: true, cfg: sw("big"), preds: []string{"big"}, in: 500,
			blocks: tn{"s": {}, "a": {src}, "b": {}, "c": {}}},
		{name: "two sources", want: "pay,two", cfg: cfgOf("one", []string{"one", "two", "pay"}, wEdge("one", "two"), wEdge("two", "pay")),
			blocks: tn{"one": {Untrusted("a")}, "two": {Untrusted("b")}, "pay": {}}},
		{name: "exempt and read-only nodes", cfg: cfgOf("src", []string{"src", "ex", "ro", "pay"}, wEdge("src", "ex"), wEdge("ex", "ro"), wEdge("ro", "pay")),
			blocks: tn{"src": {src}, "ex": {TaintExempt("fixed")}, "ro": {ReadOnly()}, "pay": {}}},
	}
	policy := agent.TaintPolicy{Gate: agent.SingleApproval()}
	for _, f := range flows {
		t.Run(f.name, func(t *testing.T) {
			reg := taintRegistryOf(t, f.blocks, f.preds...)
			if f.join {
				if err := reg.RegisterJoin2("m", func(_ context.Context, a, b int) (int, error) { return a + b, nil }); err != nil {
					t.Fatal(err)
				}
			}
			rep := InspectConfig([]byte(f.cfg), reg, InspectTaintPolicy(policy))
			if rep.Errors() {
				t.Fatalf("the config has errors: %+v", rep.Problems)
			}
			held, ran := parityRun(t, f, policy)
			var names []string
			for n := range rep.Taint {
				names = append(names, n)
			}
			slices.Sort(names)
			for _, n := range names {
				v := rep.Taint[n].Verdict
				switch {
				case held[n] && v != "escalates":
					t.Errorf("%s is held at its taint instance and the analysis says %q (%s)", n, v, fmt.Sprint(rep.Taint[n]))
				case ran[n] && v == "escalates" && !held[n] && !f.skipsSource(n, rep, ran):
					t.Errorf("%s ran, the analysis says it escalates, and the engine never held it", n)
				}
			}
			if f.want != "" {
				var got []string
				for n := range held {
					got = append(got, n)
				}
				slices.Sort(got)
				if strings.Join(got, ",") != f.want {
					t.Errorf("held %v, want %s", got, f.want)
				}
			}
			if f.none && len(held) > 0 {
				t.Errorf("held %v, want nothing", held)
			}
			if !f.none && len(held) == 0 {
				t.Errorf("nothing was held: the test proves nothing about %s", f.name)
			}
		})
	}
}

// skipsSource reports whether every source that may precede n in the analysis is on a path that did not run (an
// untaken switch arm): then the engine, which reads the journal, has nothing to hold n for.
func (f parityFlow) skipsSource(n string, rep ConfigReport, ran map[string]bool) bool {
	for name, opts := range f.blocks {
		var nd nodeTaint
		o := node{}
		for _, opt := range opts {
			opt(&o)
		}
		nd = o.taint
		if len(nd.untrusted) > 0 && ran[name] {
			return false
		}
	}
	return true
}

// The run's input is declared untrusted by the caller, which no config carries: the engine holds the first
// node that can have a side effect, and the analysis names the case in the reason of a node with no declared source.
func TestTaintParityRunInputCaveat(t *testing.T) {
	f := parityFlow{name: "input", cfg: cfgOf("first", []string{"first", "pay"}, wEdge("first", "pay")), blocks: tn{"first": {}, "pay": {}},
		opts: []agent.RunOption{agent.WithUntrustedInput("webhook")}}
	policy := agent.TaintPolicy{Gate: agent.SingleApproval()}
	rep := InspectConfig([]byte(f.cfg), taintRegistryOf(t, f.blocks), InspectTaintPolicy(policy))
	held, _ := parityRun(t, f, policy)
	if !held["first"] || !held["pay"] {
		t.Fatalf("held %v: the untrusted input should gate both side-effecting nodes", held)
	}
	for _, n := range []string{"first", "pay"} {
		got := rep.Taint[n]
		if got.Verdict != "no declared source before it" || !strings.Contains(got.Reason, "input untrusted") {
			t.Errorf("%s: %+v: a node the engine gates for the run's input must say the input is outside what the view knows", n, got)
		}
	}
}

// A refusing policy refuses the nodes the analysis calls refused and no others.
func TestTaintParityRefuse(t *testing.T) {
	cfg := cfgOf("src", []string{"src", "mid", "pay"}, wEdge("src", "mid"), wEdge("mid", "pay"))
	blocks := tn{"src": {Untrusted("x")}, "mid": {ReadOnly()}, "pay": {}}
	policy := agent.TaintPolicy{Refuse: true}
	rep := InspectConfig([]byte(cfg), taintRegistryOf(t, blocks), InspectTaintPolicy(policy))
	ran := map[string]bool{}
	reg := NewRegistry()
	for name, opts := range blocks {
		name := name
		_ = reg.RegisterStep(name, func(_ context.Context, n int) (int, error) { ran[name] = true; return n, nil }, opts...)
	}
	flow, err := Load[int, int]([]byte(cfg), reg, WithLoadedTaintPolicy(policy))
	if err != nil {
		t.Fatal(err)
	}
	_, err = flow.Run(context.Background(), agenttest.MemJournal(), "r1", 1)
	if !errors.Is(err, ErrTaintRefused) || !strings.Contains(err.Error(), `"pay"`) {
		t.Fatalf("run: %v", err)
	}
	if rep.Taint["pay"].Verdict != "refused" || rep.Taint["mid"].Verdict == "refused" || ran["pay"] || !ran["mid"] {
		t.Errorf("pay %q mid %q, ran %v", rep.Taint["pay"].Verdict, rep.Taint["mid"].Verdict, ran)
	}
}
