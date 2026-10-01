package agent

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
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
	if _, err := Parallel(ctx, m, "r", []Task[int]{{Name: "", Fn: fn}}); !errors.Is(err, ErrConfig) {
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

// A flow input with no canonical JSON (a repeated key, a lone surrogate, invalid UTF-8) could not be
// told apart from another input, so a flow's run refuses it, on the first drive as on a later one,
// and records nothing.
func TestFlowInputWithoutCanonicalJSONIsRefused(t *testing.T) {
	ctx := context.Background()
	for _, in := range []string{`{"a":1,"a":2}`, `"\ud800"`, `"\udc00x"`, `["\ud800A"]`, "\"\xff\""} {
		m := NewMemStore()
		start := RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}, Input: in}
		if _, _, err := journalhook.Begin(ctx, m, "r", start); !errors.Is(err, ErrConfig) {
			t.Errorf("Begin with input %q: %v, want ErrConfig", in, err)
		}
		if recs, _ := m.History(ctx, "r"); len(recs) != 0 {
			t.Errorf("Begin with input %q recorded %d records", in, len(recs))
		}
	}
	// A valid surrogate pair is a character, and compares with the character itself.
	m := NewMemStore()
	if _, _, err := journalhook.Begin(ctx, m, "r", RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}, Input: `"😀"`}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := journalhook.Begin(ctx, m, "r", RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}, Input: "\"\U0001F600\""}); err != nil {
		t.Fatalf("the same character, unescaped: %v", err)
	}
}

// Values are compared under one rule: a Step's value is journaled without HTML escapes, and a
// resolution of an outcome another writer recorded with them is the same outcome.
func TestOneJSONRule_Escaping(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	if _, err := Step(ctx, m, "r", "s", func(context.Context) (string, error) { return "a<b & c>d", nil }, WithSafety(Safety{ReadOnly: true})); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := m.Journal().Get(ctx, "r", "s")
	if err != nil || !ok || string(rec.Result) != `"a<b & c>d"` {
		t.Fatalf("the Step's value is journaled as %s (%v, %v), want it unescaped", rec.Result, ok, err)
	}
	// A driver recorded the outcome escaped; the resolution of the same outcome is not a conflict.
	for _, w := range []struct {
		name string
		rec  Record
	}{
		{stepAttemptStep("t"), Record{Kind: StepAttempt, ToolUseID: "t", AttemptedAt: 1}},
		{"t", Record{Kind: StepValue, Result: json.RawMessage(`"a` + ltEscape + `b"`)}},
	} {
		if _, err := m.Do(ctx, "r", w.name, func(context.Context) (Record, error) { return w.rec, nil }); err != nil {
			t.Fatal(err)
		}
	}
	if got, _, _ := m.Journal().Get(ctx, "r", "t"); !strings.Contains(string(got.Result), ltEscape) {
		t.Fatalf("the escaped outcome is stored as %s, want the escape kept", got.Result)
	}
	ref := HaltRef{RunID: "r", Op: OpRef{Kind: OpStep, ID: "t"}, Cause: HaltCrashed}
	if err := ResolveHaltRef(ctx, m, ref, Outcome{Result: "a<b"}); err != nil {
		t.Fatalf("resolving the recorded outcome again: %v, want nil", err)
	}
	if err := ResolveHaltRef(ctx, m, ref, Outcome{Result: "a>b"}); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("resolving another outcome: %v, want ErrAlreadyResolved", err)
	}
}

// ltEscape is the JSON escape json.Marshal writes for '<', built from its bytes.
var ltEscape = string([]byte{'\\', 'u', '0', '0', '3', 'c'})

// addExponent adds a small offset to an exponent's decimal text exactly, whatever its size,
// agreeing with math/big.
func TestAddExponent(t *testing.T) {
	big19 := "1" + strings.Repeat("0", 18)
	for _, e := range []string{"0", "7", "-7", "+42", "00012", "-00", big19, "-" + big19, "9999999999999999999999", "-9999999999999999999999",
		"1000000000000000000000", "-1000000000000000000000", "0000000000000000000000012", "-0000000000000000000000012", "+000000000000000000000000123456789012345678901", "-000000000000000000000000123456789012345678901", "+" + big19 + "5", "123456789012345678901234567890", "-123456789012345678901234567890"} {
		for _, d := range []int64{0, 1, -1, 9, -9, 10, -10, 12345, -12345, 999999999, -999999999} {
			want, ok := new(big.Int).SetString(strings.TrimPrefix(e, "+"), 10)
			if !ok {
				t.Fatalf("fixture %q", e)
			}
			want.Add(want, big.NewInt(d))
			if got := addExponent(e, d); got != want.String() {
				t.Errorf("addExponent(%s, %d) = %s, want %s", e, d, got, want)
			}
		}
	}
}

// Every value the engine journals is written with the journal's one encoding: no HTML escapes.
func TestOneJSONRule_EngineValuesUnescaped(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	const v = "a<b & c>d"
	if err := AnswerInterrupt(ctx, m, "r", "q", v); err != nil {
		t.Fatal(err)
	}
	if err := Signal(ctx, m, "r", "s", v); err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(ctx, m, "r", "c", "k", v); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{interruptStep("q"), signalStep("s"), chanStep("c", "k")} {
		rec, ok, err := m.Journal().Get(ctx, "r", key)
		if err != nil || !ok || !strings.Contains(string(rec.Result), v) {
			t.Errorf("%s is journaled as %s (%v, %v), want %q unescaped", key, rec.Result, ok, err, v)
		}
	}
}

// canonicalJSON reads at most maxCanonicalDepth levels of nesting, with or without
// encoding/json/v2 (whose decoder has a limit of its own; the v1 decoder's Token has none), and
// refuses deeper text without recursing into it.
func TestCanonicalJSON_DepthLimit(t *testing.T) {
	for _, tc := range []struct {
		depth int
		ok    bool
	}{{maxCanonicalDepth, true}, {maxCanonicalDepth + 1, false}, {1_000_000, false}} {
		for _, s := range []string{
			strings.Repeat(`{"a":`, tc.depth) + "1" + strings.Repeat("}", tc.depth),
			strings.Repeat(`[`, tc.depth) + "1" + strings.Repeat("]", tc.depth),
		} {
			_, err := canonicalJSON(s)
			if (err == nil) != tc.ok || err != nil && !errors.Is(err, errNotCanonical) {
				t.Errorf("depth %d (%.1s): err = %v, want ok=%v", tc.depth, s, err, tc.ok)
			}
		}
	}
}
