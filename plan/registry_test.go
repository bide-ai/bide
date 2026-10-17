package plan

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/toolhook"
)

type deadSafetyDecorator struct{ agent.Tool }

func (deadSafetyDecorator) Safety() agent.Safety { return agent.Safety{} }

// A flow refuses a decorator whose old Safety override is dead, as agent.New does: the agent
// reads only Spec, so a resume would re-run a node the decorator declared a side effect.
func TestRegisterTool_RefusesAnOldMethodThatDisagreesWithSpec(t *testing.T) {
	inner := agent.MustFunc("lookup", "", func(context.Context, int) (int, error) { return 1, nil },
		agent.WithSafety(agent.Safety{Idempotent: true}))
	reg := NewRegistry()
	err := reg.RegisterTool[int, int]("lookup", deadSafetyDecorator{inner})
	t.Logf("RegisterTool err = %v", err)
	if err == nil {
		t.Fatalf("plan accepted a decorator whose Safety() (%+v) disagrees with its Spec (%+v); agent.New refuses it", deadSafetyDecorator{inner}.Safety(), deadSafetyDecorator{inner}.Spec().Safety)
	}
}

// toolhook.CallGuard is documented as "asked before every tool call reaches its tool" (audit sets
// it to refuse calls under an expired delegated grant). An agent's base handler asks it; a plan
// Tool node's callTool does not, so a flow run under a delegation's context (a tool in the
// sub-run that drives a flow) calls its tools past the grant's expiry.
func TestRev117e_PlanToolNodeSkipsCallGuard(t *testing.T) {
	prev := toolhook.CallGuard
	defer func() { toolhook.CallGuard = prev }()
	toolhook.CallGuard = func(context.Context) error { return errors.New("grant expired") }
	var ran atomic.Int32
	tool := agent.MustFunc("send", "", func(context.Context, int) (int, error) { ran.Add(1); return 1, nil })
	args, _ := json.Marshal(1)
	_, err := callTool(context.Background(), tool, 0, args)
	if ran.Load() != 0 {
		t.Fatalf("the guard refused every call, yet the plan Tool node ran the tool (err %v)", err)
	}
}

// A RegisterTool the agent's wrapper check refuses is not recorded on the Registry, unlike a
// duplicate registration, so Load (for a caller that did not check the return) reports an
// unknown block rather than the refusal.
func TestRev117e_RegisterToolRefusalSurfacesAtLoad(t *testing.T) {
	sub := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("x")), agenttest.MemJournal())
	reg := NewRegistry()
	_ = reg.RegisterTool[int, int]("delegate", timedSubWrap{agent.MustSubAgent("delegate", "", sub)})
	cfg := `{"version":1,"flow":"f","in":"int","out":"int","entry":"delegate",
	  "nodes":[{"name":"delegate","block":"delegate"}],"wiring":[]}`
	_, err := Load[int, int]([]byte(cfg), reg)
	if err == nil || !strings.Contains(err.Error(), "Timeout") {
		t.Fatalf("Load = %v; want the RegisterTool refusal (a Timeout over a sub-agent), as a duplicate registration surfaces", err)
	}
}

// The plan timeout rule matches the agent's: a result after the deadline is returned; an error
// after the flow's own context is done (cancelled, or its earlier deadline passed) is the flow's
// cancellation, not an unknown outcome; an error after the tool's own deadline is unknown.
func TestRev117e_TimeoutRuleParity(t *testing.T) {
	mk := func(f func(ctx context.Context) (int, error)) agent.Tool {
		return agent.MustFunc("t", "", func(ctx context.Context, _ int) (int, error) { return f(ctx) }, agent.WithTimeout(20*time.Millisecond))
	}
	args, _ := json.Marshal(1)
	// result after the tool's deadline
	raw, err := callTool(context.Background(), mk(func(ctx context.Context) (int, error) { <-ctx.Done(); return 7, nil }), 20*time.Millisecond, args)
	if err != nil || string(raw) != "7" {
		t.Errorf("late result: %s, %v", raw, err)
	}
	// error after parent cancellation, before the tool's deadline
	pctx, cancel := context.WithCancel(context.Background())
	_, err = callTool(pctx, mk(func(ctx context.Context) (int, error) { cancel(); return 0, errors.New("boom") }), 20*time.Millisecond, args)
	if errors.Is(err, agent.ErrToolOutcomeUnknown) {
		t.Errorf("error after parent cancel: %v; want the plain error", err)
	}
	// parent deadline earlier than the tool's
	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer dcancel()
	_, err = callTool(dctx, mk(func(ctx context.Context) (int, error) { <-ctx.Done(); return 0, ctx.Err() }), time.Second, args)
	if errors.Is(err, agent.ErrToolOutcomeUnknown) {
		t.Errorf("error after the parent's deadline: %v; want the plain error", err)
	}
	// error after the tool's deadline, judged by the deadline before the timer fires
	_, err = callTool(context.Background(), mk(func(ctx context.Context) (int, error) {
		dl, _ := ctx.Deadline()
		time.Sleep(time.Until(dl))
		return 0, errors.New("late")
	}), 20*time.Millisecond, args)
	if !errors.Is(err, agent.ErrToolOutcomeUnknown) {
		t.Errorf("error at the tool's deadline: %v; want ErrToolOutcomeUnknown", err)
	}
}

type timedSubWrap struct{ agent.Tool }

func (w timedSubWrap) Spec() agent.ToolSpec {
	s := w.Tool.Spec()
	s.Timeout = time.Millisecond
	return s
}
func (w timedSubWrap) Unwrap() agent.Tool { return w.Tool }

// ADV117b-5 ((b)/(c)). agent.New refuses a wrapper that gives a sub-agent a Timeout, since the
// timeout would cut the sub-run off mid-call. plan now applies ToolSpec.Timeout to Tool nodes,
// but Build does not run the same check, so the same wrapper is accepted as a flow node.
func TestAdv117b_PlanAcceptsATimedWrapperOverASubAgent(t *testing.T) {
	sub := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("x")), agenttest.MemJournal())
	b := New[string, string]("f")
	b.Tool[string, string]("delegate", timedSubWrap{agent.MustSubAgent("delegate", "", sub)})
	_, err := b.Build()
	if !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Build = %v; want ErrConfig, as agent.New gives for the same tool", err)
	}
}

// RegisterTool refuses the same wrapper, and a Compensator nested in an Unwrap chain.
func TestAdv117b_RegisterToolRefusesUnsafeWrappers(t *testing.T) {
	sub := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("x")), agenttest.MemJournal())
	reg := NewRegistry()
	if err := reg.RegisterTool[string, string]("delegate", timedSubWrap{agent.MustSubAgent("delegate", "", sub)}); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("RegisterTool = %v, want ErrConfig", err)
	}
}

// Suspicion (c): a flow's Tool node applies the wrapped tool's ToolSpec.Timeout, as the agent
// does, and an error after the deadline has an unknown outcome.
func TestR117_ToolNodeAppliesTheToolsTimeout(t *testing.T) {
	var sawDeadline bool
	slow := agent.MustFunc("slow", "", func(ctx context.Context, _ int) (int, error) {
		dl, ok := ctx.Deadline()
		sawDeadline = ok && time.Until(dl) < time.Second
		<-ctx.Done()
		return 0, ctx.Err()
	}, agent.WithTimeout(time.Millisecond))
	for name, run := range map[string]func(context.Context, any) (any, error){
		"Builder.Tool": func() func(context.Context, any) (any, error) {
			b := New[int, int]("f")
			b.Tool[int, int]("slow", slow)
			return b.core.nodes[0].run
		}(),
		"RegisterTool": func() func(context.Context, any) (any, error) {
			reg := NewRegistry()
			if err := reg.RegisterTool[int, int]("slow", slow); err != nil {
				t.Fatal(err)
			}
			return reg.blocks["slow"].run
		}(),
	} {
		sawDeadline = false
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // a backstop, far past the tool's
		start := time.Now()
		_, err := run(ctx, 1)
		cancel()
		if time.Since(start) > 4*time.Second {
			t.Errorf("%s: the call ran to the backstop: the tool's timeout was not applied", name)
		}
		if !sawDeadline || !errors.Is(err, agent.ErrToolOutcomeUnknown) {
			t.Errorf("%s: deadline seen %v, err %v; want the tool's deadline applied and ErrToolOutcomeUnknown", name, sawDeadline, err)
		}
	}
}

// A loop's back-edge is drawn with its bound, apart from the forward arms.
func TestRenderMermaid_LoopBackEdgeShowsItsBound(t *testing.T) {
	flow, err := buildCountdownLoop(7, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := flow.RenderMermaid()
	if !strings.Contains(out, "|loop &le;7|") {
		t.Errorf("the back-edge is not labelled with its bound 7:\n%s", out)
	}
	if !strings.Contains(out, "|Else|") {
		t.Errorf("the exit arm lost its Else label:\n%s", out)
	}
	if strings.Count(out, "|loop") != 1 {
		t.Errorf("want exactly one loop-labelled edge:\n%s", out)
	}
}

// A Switch with no Else and no matching arm fails the run with an error that names the switch,
// and journals no completion for it.
func TestRun_SwitchMatchingNothingFails(t *testing.T) {
	b := New[int, string]("nomatch")
	entry := b.Step("entry", func(_ context.Context, n int) (int, error) { return n, nil })
	big := b.Step("big", func(context.Context, int) (string, error) { return "big", nil })
	b.Switch(entry, When(func(n int) bool { return n > 100 }, big))
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	j := agenttest.MemJournal()
	_, err = flow.Run(context.Background(), j, "r", 1)
	if err == nil || !strings.Contains(err.Error(), `Switch over "entry" matched no arm and has no Else`) {
		t.Fatalf("err = %v, want the unmatched switch named", err)
	}
	recs, _ := j.History(context.Background(), "r")
	for _, r := range recs {
		if r.Name == runCompleteStep {
			t.Error("a failed run journaled its completion")
		}
	}
	if out, err := flow.Run(context.Background(), agenttest.MemJournal(), "r2", 500); err != nil || out != "big" {
		t.Errorf("a matching input = %q, %v; want big", out, err)
	}
}

// A duplicate registration is refused where it is made and again when a config is loaded, so a
// block cannot silently replace another.
func TestRegistry_DuplicatesAreRefusedAndReportedAtLoad(t *testing.T) {
	for name, dup := range map[string]func(*Registry) error{
		"block": func(r *Registry) error { return r.RegisterStep("classify", cfgClassify) },
		"predicate": func(r *Registry) error {
			return r.RegisterPredicate("rush", func(a cfgAssessment) bool { return a.Rush })
		},
	} {
		reg := triageRegistry(t)
		err := dup(reg)
		if err == nil || !strings.Contains(err.Error(), "duplicate "+name+" registration") {
			t.Errorf("%s: err = %v, want a duplicate error", name, err)
		}
		if _, lerr := Load[cfgOrder, cfgReceipt]([]byte(triageConfig), reg); lerr == nil || !strings.Contains(lerr.Error(), "duplicate "+name+" registration") {
			t.Errorf("%s: Load = %v, want the duplicate reported", name, lerr)
		}
	}
	reg := NewRegistry()
	merge := func(_ context.Context, a cfgAssessment, r cfgReservation) (cfgReceipt, error) {
		return cfgReceipt{}, nil
	}
	if err := reg.RegisterJoin2("m", merge); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterJoin2("m", merge); err == nil || !strings.Contains(err.Error(), "duplicate merge registration") {
		t.Errorf("a duplicate merge = %v", err)
	}
	if err := reg.RegisterJoin3("m", func(_ context.Context, a cfgAssessment, r cfgReservation, c cfgReceipt) (cfgReceipt, error) {
		return c, nil
	}); err == nil || !strings.Contains(err.Error(), "duplicate merge registration") {
		t.Errorf("a duplicate merge through Join3 = %v", err)
	}
}
