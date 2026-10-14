package agent

import (
	"context"
	"errors"
	"testing"
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
