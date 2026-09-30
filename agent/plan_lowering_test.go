package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bide-ai/bide/internal/journalhook"
)

// The prefixes of a plan flow's keys are reserved, so a Step inside a node's body cannot name a
// node's result, a branch choice or the topology digest.
func TestPlanPrefixesAreReserved(t *testing.T) {
	for _, name := range []string{"node:x", "node:iter:0:x", "switch:x", "flow:digest"} {
		if !IsReservedStepName(name) {
			t.Errorf("%q is not reserved", name)
		}
		if _, err := Step(context.Background(), NewMemStore(), "r", name, func(context.Context) (int, error) { return 1, nil }); !errors.Is(err, ErrConfig) {
			t.Errorf("Step(%q): err = %v, want ErrConfig", name, err)
		}
	}
}

func TestPlanNodeStep(t *testing.T) {
	// name -> is a node key, is a node key or a node's Step
	for name, want := range map[string][2]bool{
		"node:a":                     {true, true},
		"node:iter":                  {true, true},
		"node:iter:0:a":              {true, true},
		"node:iter:12:iter":          {true, true},
		"node:a:step:x":              {false, true},
		"node:a:step:x:y":            {false, true},
		"node:iter:step:x":           {false, true},
		"node:iter:3:a:step:x":       {false, true},
		"node:iter:3:iter:step:x:1:": {false, true},
		"node:":                      {false, false},
		"node:a:b":                   {false, false},
		"node:a:step:":               {false, false},
		"node:a:steps:x":             {false, false},
		"node:iter:x:a":              {false, false},
		"node:iter::a":               {false, false},
		"node:iter:01:a":             {false, false},
		"node:iter:00:a":             {false, false},
		"node:iter:1:":               {false, false},
		"node:iter:1:a:b":            {false, false},
		"switch:a":                   {false, false},
		"a":                          {false, false},
		"attempt:step:a":             {false, false},
	} {
		if got := planNodeKey(name); got != want[0] {
			t.Errorf("planNodeKey(%q) = %v, want %v", name, got, want[0])
		}
		if got := planNodeStep(name); got != want[1] {
			t.Errorf("planNodeStep(%q) = %v, want %v", name, got, want[1])
		}
	}
}

// A Step run in a node's body is recorded under the node's key, for that run only.
func TestPlanScopedStep(t *testing.T) {
	ctx := context.WithValue(context.Background(), planScopeKey{}, planScope{runID: "r", node: "node:iter:2:inc"})
	if got := planScopedStep(ctx, "r", "charge"); got != "node:iter:2:inc:step:charge" {
		t.Fatalf("scoped = %q", got)
	}
	if got := planScopedStep(ctx, "other", "charge"); got != "charge" {
		t.Fatalf("another run's step = %q, want it unscoped", got)
	}
	if got := planScopedStep(context.Background(), "r", "charge"); got != "charge" {
		t.Fatalf("outside a node = %q", got)
	}
}

// The engine step hook runs a Step only under a plan node's key, with an agent.Safety.
func TestJournalhookStepRefusesOtherNames(t *testing.T) {
	ran := 0
	fn := func(context.Context) (json.RawMessage, error) { ran++; return json.RawMessage(`1`), nil }
	for _, name := range []string{"x", "switch:x", "flow:digest", "run:start", "attempt:step:node:x", "node:a:b", "node:a:step:x", "node:iter:01:a"} {
		if _, err := journalhook.Step(context.Background(), NewMemStore(), "r", name, Safety{}, fn); !errors.Is(err, ErrConfig) {
			t.Errorf("journalhook.Step(%q): err = %v, want ErrConfig", name, err)
		}
	}
	if _, err := journalhook.Step(context.Background(), NewMemStore(), "r", "node:x", "not a safety", fn); !errors.Is(err, ErrConfig) {
		t.Errorf("journalhook.Step with a non-Safety: err = %v, want ErrConfig", err)
	}
	if ran != 0 {
		t.Fatalf("a refused hook call ran its body %d times", ran)
	}
	got, err := journalhook.Step(context.Background(), NewMemStore(), "r", "node:x", Safety{}, fn)
	if err != nil || string(got) != "1" || ran != 1 {
		t.Fatalf("journalhook.Step(node:x) = %s, %v (ran %d); want 1, nil, 1", got, err, ran)
	}
}

// A run's start holds its kind: an agent run (whose start records no kind) and a flow run never
// drive each other's journal, and a flow run is held to its flow's name.
func TestRunStartHoldsKindAndFlow(t *testing.T) {
	ctx := context.Background()
	flow := RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}, Input: "1"}
	for _, tc := range []struct {
		name        string
		first, next RunStart
		ok          bool
	}{
		{"same flow", flow, flow, true},
		{"agent then agent", RunStart{Input: "1"}, RunStart{Input: "1"}, true},
		{"explicit agent kind", RunStart{Input: "1"}, RunStart{Kind: RunKindAgent, Input: "1"}, true},
		{"agent then flow", RunStart{Input: "1"}, flow, false},
		{"flow then agent", flow, RunStart{Input: "1"}, false},
		{"another flow", flow, RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "g"}, Input: "1"}, false},
		{"flow with no name", flow, RunStart{Kind: RunKindFlow, Input: "1"}, false},
		{"another input", flow, RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}, Input: "2"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMemStore()
			if _, _, err := journalhook.Begin(ctx, m, "r", tc.first); err != nil {
				t.Fatal(err)
			}
			_, _, err := journalhook.Begin(ctx, m, "r", tc.next)
			if tc.ok != (err == nil) || err != nil && !errors.Is(err, ErrConfig) {
				t.Fatalf("second drive: err = %v, want ok=%v (else ErrConfig)", err, tc.ok)
			}
		})
	}
}

// An agent run's start encodes as before, with no kind, and a flow's start names its kind and flow.
func TestRunStartEncoding(t *testing.T) {
	for _, tc := range []struct {
		start RunStart
		want  string
	}{
		{RunStart{Input: "hi"}, `{"input":"hi"}`},
		{RunStart{Input: "hi", Saga: true}, `{"input":"hi","saga":true}`},
		{RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}, Input: `{"a":1}`}, `{"input":"{\"a\":1}","kind":"flow","flow":{"name":"f"}}`},
	} {
		b, err := marshalJournal(tc.start)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tc.want {
			t.Errorf("RunStart %+v encodes as %s, want %s", tc.start, b, tc.want)
		}
	}
}

// ResolveHaltRef resolves only an operation that halted: one with a live attempt marker. A tool
// call or Step never attempted, or whose only attempt is recorded as not started (the next drive
// re-attempts it), is refused with ErrNoLiveAttempt, and nothing is recorded.
func TestResolveHaltRef_RefusesAnOperationWithNoLiveAttempt(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	claim := "0123abcd0123abcd"
	for _, w := range []struct {
		name string
		rec  Record
	}{
		{stepAttemptStep("voided"), Record{Kind: StepAttempt, ToolUseID: "voided", AttemptedAt: 1, claim: claim}},
		{notStartedStep(stepAttemptStep("voided"), claim), Record{Kind: StepNotStarted, ToolUseID: "voided", claim: claim}},
	} {
		if _, err := m.Do(ctx, "r", w.name, func(context.Context) (Record, error) { return w.rec, nil }); err != nil {
			t.Fatal(err)
		}
	}
	for _, op := range []OpRef{{Kind: OpTool, ID: "never"}, {Kind: OpStep, ID: "never"}, {Kind: OpStep, ID: "voided"}, {Kind: OpStep, ID: "node:a"}} {
		err := ResolveHaltRef(ctx, m, HaltRef{RunID: "r", Op: op, Cause: HaltCrashed}, Outcome{Result: 1})
		if !errors.Is(err, ErrNoLiveAttempt) || !errors.Is(err, ErrConfig) {
			t.Errorf("ResolveHaltRef(%+v): err = %v, want ErrNoLiveAttempt", op, err)
		}
	}
	recs, err := m.History(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Name == "never" || r.Name == "voided" || r.Name == "node:a" || r.Name == ToolResultStep("never") {
			t.Fatalf("a refused resolution recorded %q", r.Name)
		}
	}
}

// A flow's input is held by its canonical JSON: key order and a number's spelling do not tell two
// inputs apart, so an input decoded from RecordedStart and encoded again resumes the run; a
// different value does not, however close (integers beyond 2^53, and near the uint64 maximum,
// are compared exactly).
func TestSameCanonicalJSON(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{`{"a":1,"b":[1,2]}`, `{"b":[1,2],"a":1}`, true},
		{`{"a":1}`, `{"a":1.0}`, true},
		{`{"a":1e2}`, `{"a":100}`, true},
		{`{"a":-0}`, `{"a":0}`, true},
		{`{"id":9007199254740993}`, `{"id":9007199254740992}`, false},
		{`9007199254740993`, `9007199254740993.0`, true},
		{`18446744073709551615`, `18446744073709551614`, false},
		{`18446744073709551615`, `1.8446744073709551615e19`, true},
		{`-9223372036854775808`, `-9223372036854775807`, false},
		{`0.1`, `1e-1`, true},
		{`0.10`, `0.1`, true},
		{`100`, `1E+2`, true},
		{`-0.0`, `0`, true},
		{`0e7`, `0`, true},
		{`1.5`, `15e-1`, true},
		{`1.5`, `1.50001`, false},
		{`-1`, `1`, false},
		{`1e9223372036854775807`, `1e9223372036854775807`, true},
		{` [1, "x" ] `, `[1,"x"]`, true},
		{`"<"`, `"<"`, true},
		{`{"a":1}`, `{"a":2}`, false},
		{`{"a":1}`, `{"a":1,"b":null}`, false},
		{`[1,2]`, `[2,1]`, false},
		{`{"a":1} x`, `{"a":1}`, false},
		{`1e999`, `1e999`, true},
		{`1e999`, `2e999`, false},
	} {
		if got := sameCanonicalJSON(tc.a, tc.b); got != tc.same {
			t.Errorf("sameCanonicalJSON(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.same)
		}
	}
}

// An empty step name is ErrConfig for Step, a Parallel task and a Step's resolution, inside a flow
// node or not: it would record a step under a key no resolution or conformance check names.
func TestEmptyStepNameIsRefused(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	ran := 0
	fn := func(context.Context) (int, error) { ran++; return 1, nil }
	if _, err := Step(ctx, m, "r", "", fn); !errors.Is(err, ErrConfig) {
		t.Errorf("Step(\"\"): %v, want ErrConfig", err)
	}
	inNode := context.WithValue(ctx, planScopeKey{}, planScope{runID: "r", node: "node:a"})
	if _, err := Step(inNode, m, "r", "", fn); !errors.Is(err, ErrConfig) {
		t.Errorf("Step(\"\") in a node: %v, want ErrConfig", err)
	}
	if _, err := Parallel(ctx, m, "r", 0, Task[int]{Name: "", Fn: fn}); !errors.Is(err, ErrConfig) {
		t.Errorf("Parallel with an empty task name: %v, want ErrConfig", err)
	}
	if err := ResolveHaltRef(ctx, m, HaltRef{RunID: "r", Op: OpRef{Kind: OpStep, ID: ""}, Cause: HaltCrashed}, Outcome{Result: 1}); !errors.Is(err, ErrConfig) {
		t.Errorf("ResolveHaltRef of an empty step: %v, want ErrConfig", err)
	}
	if ran != 0 {
		t.Fatalf("a refused step ran %d times", ran)
	}
	if recs, err := m.History(ctx, "r"); err != nil || len(recs) != 0 {
		t.Fatalf("refused steps recorded %d records (err %v)", len(recs), err)
	}
}
