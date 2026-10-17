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

// otherAnswer is a second answer type, whose schema differs from typedAnswer's.
type otherAnswer struct {
	Count int `json:"count"`
}

// samplingModel records the sampling of every request and answers "ok".
type samplingModel struct{ seen []Sampling }

func (m *samplingModel) Stream(ctx context.Context, req Request) (*Stream, error) {
	m.seen = append(m.seen, req.Sampling)
	return (&scriptModel{turns: [][]Emit{textTurn("ok")}}).Stream(ctx, req)
}

// A run's sampling controls replace the agent's one control at a time: a control the run does not
// set keeps the agent's value, and the merged controls are what the model is called with.
func TestRun_SamplingOverlaysTheAgentsControls(t *testing.T) {
	ctx := context.Background()
	m := &samplingModel{}
	a := mustNew(m, memJournal(), WithSampling(Temperature(0.1), TopP(0.5), MaxTokens(10), Stop("a"), Seed(1)))
	if _, err := a.Run(ctx, "r1", UserText("go"), WithSampling(TopP(0.9), MaxTokens(99), Stop("b", "c"), Seed(2))); err != nil {
		t.Fatal(err)
	}
	got := m.seen[0]
	if got.Temperature == nil || *got.Temperature != 0.1 {
		t.Errorf("Temperature = %v; want the agent's 0.1", got.Temperature)
	}
	if got.TopP == nil || *got.TopP != 0.9 {
		t.Errorf("TopP = %v; want the run's 0.9", got.TopP)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 99 {
		t.Errorf("MaxTokens = %v; want the run's 99", got.MaxTokens)
	}
	if len(got.Stop) != 2 || got.Stop[0] != "b" {
		t.Errorf("Stop = %v; want the run's [b c]", got.Stop)
	}
	if got.Seed == nil || *got.Seed != 2 {
		t.Errorf("Seed = %v; want the run's 2", got.Seed)
	}
	// The agent's own controls are untouched by the run's.
	if _, err := a.Run(ctx, "r2", UserText("go")); err != nil {
		t.Fatal(err)
	}
	if got := m.seen[1]; got.Seed == nil || *got.Seed != 1 || *got.MaxTokens != 10 {
		t.Errorf("a later run saw %+v; want the agent's own controls", got)
	}
}

// A tool filter naming a tool the agent lacks, or leaving out the tool a forced choice names, is
// ErrConfig at the run's first drive, before any model call, and no start is recorded.
func TestRun_ToolFilterIsValidatedAtTheFirstDrive(t *testing.T) {
	ctx := context.Background()
	m := &countModel{inner: &scriptModel{turns: [][]Emit{textTurn("ok")}}}
	j := memJournal()
	a := mustNew(m, j, WithTools(&countingTool{name: "lookup", calls: new(int)}, &countingTool{name: "pay", calls: new(int)}))
	if _, err := a.Run(ctx, "r1", UserText("go"), WithToolFilter("ghost")); !errors.Is(err, ErrConfig) {
		t.Errorf("filter naming a missing tool = %v; want ErrConfig", err)
	}
	if _, err := a.Run(ctx, "r2", UserText("go"), WithToolFilter("lookup"), WithToolChoice(ToolChoice{Mode: "tool", Name: "pay"})); !errors.Is(err, ErrConfig) {
		t.Errorf("filter leaving out the forced tool = %v; want ErrConfig", err)
	}
	if n := m.calls.Load(); n != 0 {
		t.Errorf("the model was called %d times by refused runs", n)
	}
	for _, id := range []string{"r1", "r2"} {
		if _, ok, err := RecordedStart(ctx, j, id); ok || err != nil {
			t.Errorf("run %s recorded a start (%v, %v) though its options were refused", id, ok, err)
		}
	}
}

// A run is driven again only the way it started: a typed run is not resumed untyped, an untyped
// one not as typed, a typed one not under another answer type or output mode, and a saga not
// without WithSaga (nor a plain run with it).
func TestRun_RedriveMustMatchTheRecordedStart(t *testing.T) {
	ctx := context.Background()
	newAgent := func(j *Journal, turns ...[]Emit) *Agent {
		return mustNew(&scriptModel{turns: turns}, j)
	}
	// seed starts a run and leaves it unfinished by a model error on its first turn.
	seed := func(t *testing.T, id string, drive func(a *Agent) error) *Journal {
		t.Helper()
		j := memJournal()
		if err := drive(newAgent(j, errTurn(errors.New("model down")))); err == nil {
			t.Fatal("seeding drive did not stop")
		}
		if _, ok, err := RecordedStart(ctx, j, id); !ok || err != nil {
			t.Fatalf("seeding drive recorded no start: %v, %v", ok, err)
		}
		return j
	}
	typed := func(a *Agent, id string, opts ...RunOption) error {
		_, _, err := a.RunTyped[typedAnswer](ctx, id, UserText("go"), opts...)
		return err
	}
	t.Run("typed run resumed untyped", func(t *testing.T) {
		j := seed(t, "r", func(a *Agent) error { return typed(a, "r") })
		if _, err := newAgent(j, textTurn("x")).Run(ctx, "r", UserText("go")); !errors.Is(err, ErrConfig) {
			t.Fatalf("Run of a typed run = %v; want ErrConfig", err)
		}
	})
	t.Run("untyped run resumed typed", func(t *testing.T) {
		j := seed(t, "r", func(a *Agent) error { _, err := a.Run(ctx, "r", UserText("go")); return err })
		if err := typed(newAgent(j, textTurn(`{"name":"x"}`)), "r"); !errors.Is(err, ErrConfig) {
			t.Fatalf("RunTyped of an untyped run = %v; want ErrConfig", err)
		}
	})
	t.Run("another answer type", func(t *testing.T) {
		j := seed(t, "r", func(a *Agent) error { return typed(a, "r") })
		_, _, err := newAgent(j, textTurn(`{"count":1}`)).RunTyped[otherAnswer](ctx, "r", UserText("go"))
		if !errors.Is(err, ErrConfig) {
			t.Fatalf("RunTyped under another answer type = %v; want ErrConfig", err)
		}
	})
	t.Run("another output mode", func(t *testing.T) {
		j := seed(t, "r", func(a *Agent) error { return typed(a, "r") })
		if err := typed(newAgent(j, textTurn(`{"name":"x"}`)), "r", WithOutputMode(OutputNative)); !errors.Is(err, ErrConfig) {
			t.Fatalf("RunTyped under another output mode = %v; want ErrConfig", err)
		}
	})
	t.Run("plain run driven as saga", func(t *testing.T) {
		j := seed(t, "r", func(a *Agent) error { _, err := a.Run(ctx, "r", UserText("go")); return err })
		if _, err := newAgent(j, textTurn("x")).Run(ctx, "r", UserText("go"), WithSaga()); !errors.Is(err, ErrConfig) {
			t.Fatalf("WithSaga on a plain run = %v; want ErrConfig", err)
		}
	})
	t.Run("WithOutputMode on an untyped run's redrive", func(t *testing.T) {
		j := seed(t, "r", func(a *Agent) error { _, err := a.Run(ctx, "r", UserText("go")); return err })
		if _, err := newAgent(j, textTurn("x")).Run(ctx, "r", UserText("go"), WithOutputMode(OutputNative)); !errors.Is(err, ErrConfig) {
			t.Fatalf("WithOutputMode on a redrive of an untyped run = %v; want ErrConfig", err)
		}
	})
}

// A run whose recorded start is unreadable is not driven: ErrStorage, not a fresh start over it.
func TestRun_CorruptRecordedStart(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	writeRaw(t, j, "r", runStartStep, Record{Kind: StepValue, Result: []byte(`[1]`)})
	m := &countModel{inner: &scriptModel{turns: [][]Emit{textTurn("ok")}}}
	if _, err := mustNew(m, j).Run(ctx, "r", UserText("go")); !errors.Is(err, ErrStorage) {
		t.Fatalf("Run over a corrupt run:start = %v; want ErrStorage", err)
	}
	if m.calls.Load() != 0 {
		t.Fatal("the model was called for a run whose start could not be read")
	}
}

// A later drive's different limit is journaled as an amendment before it drives and honored by the
// drives after it; an amendment the journal holds that does not decode, or one the store cannot
// record, stops the drive (ErrStorage) rather than driving under limits nobody can read back.
func TestRun_LimitAmendments(t *testing.T) {
	ctx := context.Background()
	// seed leaves run "r" started, with a model that stops after one tool turn.
	seed := func(t *testing.T, s Store) *Journal {
		t.Helper()
		j := mustJournal(s)
		a := mustNew(&scriptModel{turns: [][]Emit{errTurn(errors.New("down"))}}, j)
		if _, err := a.Run(ctx, "r", UserText("go"), WithMaxTurns(5)); err == nil {
			t.Fatal("seed did not stop")
		}
		return j
	}
	t.Run("recorded", func(t *testing.T) {
		j := seed(t, NewMemStore())
		a := mustNew(&scriptModel{turns: [][]Emit{textTurn("ok")}}, j)
		if _, err := a.Run(ctx, "r", UserText("go"), WithMaxTurns(7)); err != nil {
			t.Fatal(err)
		}
		r, ok, err := j.Get(ctx, "r", runLimitsStep(0))
		if err != nil || !ok || string(r.Result) != `{"max_turns":7}` {
			t.Fatalf("amendment = %s, %v, %v; want max_turns 7", r.Result, ok, err)
		}
	})
	t.Run("corrupt amendment", func(t *testing.T) {
		j := seed(t, NewMemStore())
		writeRaw(t, j, "r", runLimitsStep(0), Record{Kind: StepValue, Result: []byte(`[1]`)})
		a := mustNew(&scriptModel{turns: [][]Emit{textTurn("ok")}}, j)
		if _, err := a.Run(ctx, "r", UserText("go")); !errors.Is(err, ErrStorage) {
			t.Fatalf("Run over a corrupt amendment = %v; want ErrStorage", err)
		}
	})
	t.Run("amendment cannot be recorded", func(t *testing.T) {
		s := NewMemStore()
		seed(t, s)
		j := mustJournal(covInsertFail{s, "run:limits:"})
		m := &countModel{inner: &scriptModel{turns: [][]Emit{textTurn("ok")}}}
		if _, err := mustNew(m, j).Run(ctx, "r", UserText("go"), WithMaxTurns(7)); !errors.Is(err, ErrStorage) {
			t.Fatalf("Run = %v; want ErrStorage", err)
		}
		if m.calls.Load() != 0 {
			t.Fatal("the run was driven under a limit that could not be journaled")
		}
	})
}

// An m-of-n gate is checked before it counts: an invalid policy, no approver verifiers, or an
// approver whose verifier has no key identity is ErrConfig, and the journal is not read.
func TestQuorumTally_RefusesAnUnusableGate(t *testing.T) {
	ctx := context.Background()
	tu := ToolUse{ID: "c1", Name: "charge"}
	good := &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	noKey := func(id string) (ApproverVerifier, bool) { return noKeyVerifier{fakeVerifier{id}}, true }
	for name, c := range map[string]struct {
		pol *ApprovalPolicy
		vf  ApproverVerifierFor
	}{
		"invalid policy":     {&ApprovalPolicy{Need: 2, Approvers: []string{"a"}}, fakeVerifiers("a")},
		"no verifiers":       {good, nil},
		"verifier no key id": {good, noKey},
	} {
		a := &Agent{store: mustJournal(loadFails{NewMemStore()}), approverVerifiers: c.vf}
		if _, _, err := a.quorumTally(ctx, "r", tu, c.pol); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: quorumTally = %v; want ErrConfig (before any read of the journal)", name, err)
		}
	}
}

// The tally reads the journal to count: an unreadable history is ErrStorage, and a recorded
// terminal tally that does not decode is ErrStorage rather than a pass.
func TestTallyGate_JournalFaults(t *testing.T) {
	ctx := context.Background()
	subj := ApprovalSubject{RunID: "r", ToolUseID: "c1", ToolName: "charge"}
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	vf := fakeVerifiers("a")
	if _, _, err := tallyGate(ctx, mustJournal(loadFails{NewMemStore()}), "r", subj, pol, vf, ""); !errors.Is(err, ErrStorage) {
		t.Errorf("unreadable history = %v; want ErrStorage", err)
	}
	j := memJournal()
	writeRaw(t, j, "r", ApprovalTallyStep("c1"), Record{Kind: StepValue, Result: []byte(`[1]`)})
	if _, _, err := tallyGate(ctx, j, "r", subj, pol, vf, ""); !errors.Is(err, ErrStorage) {
		t.Errorf("corrupt recorded tally = %v; want ErrStorage", err)
	}
}

// A call outside the run's tool filter is refused with an error result the model reads, and the tool
// never runs. When that refusal cannot be recorded, the run stops with the store's error: the model
// is not shown a refusal that is not in the journal, and the tool still does not run.
func TestRun_FilteredCallRefusal(t *testing.T) {
	ctx := context.Background()
	for _, fail := range []bool{false, true} {
		var pays, lookups int
		pay := &countingTool{name: "pay", calls: &pays}
		lookup := &countingTool{name: "lookup", calls: &lookups}
		var st Store = NewMemStore()
		if fail {
			st = covInsertFail{st, ToolResultStep("c1")}
		}
		j := mustJournal(st)
		m := &scriptModel{turns: [][]Emit{toolTurn("c1", "pay", `{}`), textTurn("done")}}
		a := mustNew(m, j, WithTools(pay, lookup))
		_, err := a.Run(ctx, "r", UserText("go"), WithToolFilter("lookup"))
		if pays != 0 {
			t.Fatalf("fail=%v: the filtered tool ran %d times", fail, pays)
		}
		if fail {
			if err == nil || errors.Is(err, ErrConfig) {
				t.Fatalf("Run = %v; want the store's error", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
		r, ok, gerr := j.Get(ctx, "r", ToolResultStep("c1"))
		if gerr != nil || !ok || !r.IsError || !strings.Contains(string(r.Result), "not available") {
			t.Fatalf("refusal = %+v, %v, %v; want a recorded error result", r, ok, gerr)
		}
	}
}

// The prefixes of a plan flow's keys are reserved, so a Step inside a node's body cannot name a
// node's result, a branch choice or the topology digest.
func TestPlanPrefixesAreReserved(t *testing.T) {
	for _, name := range []string{"node:x", "node:iter:0:x", "switch:x", "flow:digest"} {
		if !IsReservedStepName(name) {
			t.Errorf("%q is not reserved", name)
		}
		if _, err := memJournal().Step(context.Background(), "r", name, func(context.Context) (int, error) { return 1, nil }); !errors.Is(err, ErrConfig) {
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
		if _, err := journalhook.Step(context.Background(), memJournal(), "r", name, Safety{}, fn); !errors.Is(err, ErrConfig) {
			t.Errorf("journalhook.Step(%q): err = %v, want ErrConfig", name, err)
		}
	}
	if _, err := journalhook.Step(context.Background(), memJournal(), "r", "node:x", "not a safety", fn); !errors.Is(err, ErrConfig) {
		t.Errorf("journalhook.Step with a non-Safety: err = %v, want ErrConfig", err)
	}
	if ran != 0 {
		t.Fatalf("a refused hook call ran its body %d times", ran)
	}
	got, err := journalhook.Step(context.Background(), memJournal(), "r", "node:x", Safety{}, fn)
	if err != nil || string(got) != "1" || ran != 1 {
		t.Fatalf("journalhook.Step(node:x) = %s, %v (ran %d); want 1, nil, 1", got, err, ran)
	}
}

// A run's start holds its kind: an agent run (whose start records no kind) and a flow run never
// drive each other's journal, and a flow run is held to its flow's name.
func TestRunStartHoldsKindAndFlow(t *testing.T) {
	ctx := context.Background()
	flow := RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}, Input: UserText("1")}
	for _, tc := range []struct {
		name        string
		first, next RunStart
		ok          bool
	}{
		{"same flow", flow, flow, true},
		{"agent then agent", RunStart{Input: UserText("1")}, RunStart{Input: UserText("1")}, true},
		{"explicit agent kind", RunStart{Input: UserText("1")}, RunStart{Kind: RunKindAgent, Input: UserText("1")}, true},
		{"agent then flow", RunStart{Input: UserText("1")}, flow, false},
		{"flow then agent", flow, RunStart{Input: UserText("1")}, false},
		{"another flow", flow, RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "g"}, Input: UserText("1")}, false},
		{"flow with no name", flow, RunStart{Kind: RunKindFlow, Input: UserText("1")}, false},
		{"another input", flow, RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}, Input: UserText("2")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := memJournal()
			if _, _, _, err := journalhook.Begin(ctx, m, "r", tc.first); err != nil {
				t.Fatal(err)
			}
			_, _, _, err := journalhook.Begin(ctx, m, "r", tc.next)
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
		{RunStart{Input: UserText("hi")}, `{"input":"hi"}`},
		{RunStart{Input: UserText("hi"), Saga: true}, `{"input":"hi","saga":true}`},
		{RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}, Input: UserText(`{"a":1}`)}, `{"input":"{\"a\":1}","kind":"flow","flow":{"name":"f"}}`},
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

// ResolveHalt resolves only an operation that halted: one with a live attempt marker. A tool
// call or Step never attempted, or whose only attempt is recorded as not started (the next drive
// re-attempts it), is refused with ErrNoLiveAttempt, and nothing is recorded.
func TestResolveHalt_RefusesAnOperationWithNoLiveAttempt(t *testing.T) {
	ctx := context.Background()
	m := memJournal()
	claim := "0123abcd0123abcd"
	for _, w := range []struct {
		name string
		rec  Record
	}{
		{stepAttemptStep("voided"), Record{Kind: StepAttempt, ToolUseID: "voided", AttemptedAt: 1, claim: claim}},
		{notStartedStep(stepAttemptStep("voided"), claim), Record{Kind: StepNotStarted, ToolUseID: "voided", claim: claim}},
	} {
		if _, err := m.do(ctx, "r", w.name, func(context.Context) (Record, error) { return w.rec, nil }); err != nil {
			t.Fatal(err)
		}
	}
	for _, op := range []OpRef{{Kind: OpTool, ID: "never"}, {Kind: OpStep, ID: "never"}, {Kind: OpStep, ID: "voided"}, {Kind: OpStep, ID: "node:a"}} {
		err := ResolveHalt(ctx, m, HaltRef{RunID: "r", Op: op, Cause: HaltCrashed}, Outcome{Result: 1})
		if !errors.Is(err, ErrNoLiveAttempt) || !errors.Is(err, ErrConfig) {
			t.Errorf("ResolveHalt(%+v): err = %v, want ErrNoLiveAttempt", op, err)
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
	m := memJournal()
	ran := 0
	fn := func(context.Context) (int, error) { ran++; return 1, nil }
	if _, err := m.Step(ctx, "r", "", fn); !errors.Is(err, ErrConfig) {
		t.Errorf("Step(\"\"): %v, want ErrConfig", err)
	}
	inNode := context.WithValue(ctx, planScopeKey{}, planScope{runID: "r", node: "node:a"})
	if _, err := m.Step(inNode, "r", "", fn); !errors.Is(err, ErrConfig) {
		t.Errorf("Step(\"\") in a node: %v, want ErrConfig", err)
	}
	if _, err := m.Parallel(ctx, "r", []Task[int]{{Name: "", Fn: fn}}); !errors.Is(err, ErrConfig) {
		t.Errorf("Parallel with an empty task name: %v, want ErrConfig", err)
	}
	if err := ResolveHalt(ctx, m, HaltRef{RunID: "r", Op: OpRef{Kind: OpStep, ID: ""}, Cause: HaltCrashed}, Outcome{Result: 1}); !errors.Is(err, ErrConfig) {
		t.Errorf("ResolveHalt of an empty step: %v, want ErrConfig", err)
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
		j := memJournal()
		start := RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}, Input: UserText(in)}
		if _, _, _, err := journalhook.Begin(ctx, j, "r", start); !errors.Is(err, ErrConfig) {
			t.Errorf("Begin with input %q: %v, want ErrConfig", in, err)
		}
		if recs, _ := j.History(ctx, "r"); len(recs) != 0 {
			t.Errorf("Begin with input %q recorded %d records", in, len(recs))
		}
	}
	// A valid surrogate pair is a character, and compares with the character itself.
	m := memJournal()
	if _, _, _, err := journalhook.Begin(ctx, m, "r", RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}, Input: UserText(`"😀"`)}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := journalhook.Begin(ctx, m, "r", RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}, Input: UserText("\"\U0001F600\"")}); err != nil {
		t.Fatalf("the same character, unescaped: %v", err)
	}
}

// Values are compared under one rule: a Step's value is journaled without HTML escapes, and a
// resolution of an outcome another writer recorded with them is the same outcome.
func TestOneJSONRule_Escaping(t *testing.T) {
	ctx := context.Background()
	m := memJournal()
	if _, err := m.Step(ctx, "r", "s", func(context.Context) (string, error) { return "a<b & c>d", nil }, WithSafety(Safety{ReadOnly: true})); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := m.Get(ctx, "r", "s")
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
		if _, err := m.do(ctx, "r", w.name, func(context.Context) (Record, error) { return w.rec, nil }); err != nil {
			t.Fatal(err)
		}
	}
	if got, _, _ := m.Get(ctx, "r", "t"); !strings.Contains(string(got.Result), ltEscape) {
		t.Fatalf("the escaped outcome is stored as %s, want the escape kept", got.Result)
	}
	ref := HaltRef{RunID: "r", Op: OpRef{Kind: OpStep, ID: "t"}, Cause: HaltCrashed}
	if err := ResolveHalt(ctx, m, ref, Outcome{Result: "a<b"}); err != nil {
		t.Fatalf("resolving the recorded outcome again: %v, want nil", err)
	}
	if err := ResolveHalt(ctx, m, ref, Outcome{Result: "a>b"}); !errors.Is(err, ErrAlreadyResolved) {
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
	m := memJournal()
	const v = "a<b & c>d"
	if err := m.AnswerInterrupt(ctx, "r", "q", v); err != nil {
		t.Fatal(err)
	}
	if err := m.Signal(ctx, "r", "s", v); err != nil {
		t.Fatal(err)
	}
	if err := m.Enqueue(ctx, "r", "c", "k", v); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{interruptStep("q"), signalStep("s"), chanStep("c", "k")} {
		rec, ok, err := m.Get(ctx, "r", key)
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
