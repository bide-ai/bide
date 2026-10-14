package agent

import (
	"context"
	"errors"
	"testing"
)

// ValidateRunID refuses an empty ID and one that contains the sub-run separator.
func TestValidateRunID(t *testing.T) {
	for id, bad := range map[string]bool{"": true, "a>b": true, ">": true, "plain": false, "with space/and:colon": false} {
		if err := ValidateRunID(id); errors.Is(err, ErrConfig) != bad || (err != nil) != bad {
			t.Errorf("ValidateRunID(%q) = %v; refused should be %v", id, err, bad)
		}
	}
}

// ResumeAgent drives only plain agent runs: a legacy start, a flow, a typed run is ErrNotResumable
// (never guessed at), and a recovery drive that tries to change a journaled setting is ErrConfig.
func TestResumeAgent_DrivesOnlyPlainAgentRuns(t *testing.T) {
	ctx := context.Background()
	a := mustNew(&scriptModel{turns: [][]Emit{textTurn("done")}}, memJournal())
	for name, start := range map[string]RunStart{
		"legacy":  {Input: UserText("go")},
		"flow":    {Input: UserText("{}"), Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}},
		"typed":   {Input: UserText("go"), Kind: RunKindAgent, Typed: &TypedStart{Mode: OutputTool}},
		"session": {Input: UserText("go"), Kind: RunKindSessionTurn},
	} {
		if err := ResumeAgent(a)(ctx, "r", start); !errors.Is(err, ErrNotResumable) {
			t.Errorf("%s: ResumeAgent = %v; want ErrNotResumable", name, err)
		}
	}
	plain := RunStart{Input: UserText("go"), Kind: RunKindAgent}
	for name, opt := range map[string]RunOption{
		"max turns": WithMaxTurns(3), "token budget": WithTokenBudget(10), "system prompt": WithSystemPrompt("x"),
		"sampling": WithSampling(Temperature(0)), "tool choice": WithToolChoice(ToolChoice{Mode: "none"}),
		"tool filter": WithToolFilter("x"), "saga": WithSaga(), "output mode": WithOutputMode(OutputNative),
		"principal": WithIdentity(Identity{OnBehalfOf: "d"}),
	} {
		if err := ResumeAgent(a, opt)(ctx, "r", plain); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: ResumeAgent = %v; want ErrConfig", name, err)
		}
	}
	if err := ResumeAgent(a, nil)(ctx, "r", plain); !errors.Is(err, ErrConfig) {
		t.Errorf("a nil option = %v; want ErrConfig", err)
	}
}

// ResumeAgent finishes an unfinished run from its journal, and ResumeTyped does the same for a
// typed run of its answer type only.
func TestResume_DrivesTheRecordedRun(t *testing.T) {
	ctx := context.Background()
	t.Run("agent", func(t *testing.T) {
		j := memJournal()
		if _, err := mustNew(&scriptModel{turns: [][]Emit{errTurn(errors.New("down"))}}, j).Run(ctx, "r", UserText("go")); err == nil {
			t.Fatal("seed run did not stop")
		}
		start, ok, err := RecordedStart(ctx, j, "r")
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		a := mustNew(&scriptModel{turns: [][]Emit{textTurn("done")}}, j)
		if err := ResumeAgent(a)(ctx, "r", start); err != nil {
			t.Fatalf("ResumeAgent = %v", err)
		}
		if st, err := Status(ctx, j, "r"); err != nil || st.State != RunCompleted {
			t.Fatalf("Status = %+v, %v; want complete", st, err)
		}
	})
	t.Run("typed", func(t *testing.T) {
		j := memJournal()
		seed := mustNew(&scriptModel{turns: [][]Emit{errTurn(errors.New("down"))}}, j)
		if _, _, err := seed.RunTyped[typedAnswer](ctx, "r", UserText("go")); err == nil {
			t.Fatal("seed run did not stop")
		}
		start, _, err := RecordedStart(ctx, j, "r")
		if err != nil {
			t.Fatal(err)
		}
		a := mustNew(&scriptModel{turns: [][]Emit{toolTurn("f1", "final_answer", `{"name":"done"}`)}}, j)
		if err := ResumeTyped[otherAnswer](a)(ctx, "r", start); !errors.Is(err, ErrNotResumable) {
			t.Errorf("ResumeTyped under another answer type = %v; want ErrNotResumable", err)
		}
		if err := ResumeTyped[typedAnswer](a, WithMaxTurns(2))(ctx, "r", start); !errors.Is(err, ErrConfig) {
			t.Errorf("ResumeTyped with a journaled option = %v; want ErrConfig", err)
		}
		if err := ResumeTyped[typedAnswer](a)(ctx, "r", start); err != nil {
			t.Fatalf("ResumeTyped = %v", err)
		}
		if st, err := Status(ctx, j, "r"); err != nil || st.State != RunCompleted {
			t.Fatalf("Status = %+v, %v; want complete", st, err)
		}
		plain := RunStart{Input: UserText("go"), Kind: RunKindAgent}
		if err := ResumeTyped[typedAnswer](a)(ctx, "r", plain); !errors.Is(err, ErrNotResumable) {
			t.Errorf("ResumeTyped of an untyped run = %v; want ErrNotResumable", err)
		}
	})
}

// RunTyped refuses, before any model call, an answer type that is not a JSON object (New already
// refuses an agent with a tool named final_answer); and a bad run ID or option comes back with the Result
// carrying the ID.
func TestRunTyped_Refusals(t *testing.T) {
	ctx := context.Background()
	m := &countModel{inner: &scriptModel{turns: [][]Emit{textTurn("x")}}}
	a := mustNew(m, memJournal())
	if _, _, err := a.RunTyped[string](ctx, "r", UserText("go")); !errors.Is(err, ErrConfig) {
		t.Errorf("RunTyped[string] = %v; want ErrConfig", err)
	}
	if _, err := New(m, memJournal(), WithTools(&countingTool{name: finalAnswerTool, calls: new(int)})); !errors.Is(err, ErrConfig) {
		t.Errorf("New with a final_answer tool = %v; want ErrConfig (the name is RunTyped's)", err)
	}
	if _, _, err := a.RunTyped[typedAnswer](ctx, "", UserText("go")); !errors.Is(err, ErrConfig) {
		t.Errorf("RunTyped with no run ID = %v; want ErrConfig", err)
	}
	if _, res, err := a.RunTyped[typedAnswer](ctx, "r2", UserText("go"), nil); !errors.Is(err, ErrConfig) || res == nil || res.RunID != "r2" {
		t.Errorf("RunTyped with a nil option = %v, %+v; want ErrConfig and a Result for r2", err, res)
	}
	if n := m.calls.Load(); n != 0 {
		t.Errorf("the model was called %d times by refused runs", n)
	}
}

// Generate returns the model's start error unchanged, and otherwise the assembled message.
func TestGenerate(t *testing.T) {
	ctx := context.Background()
	want := errors.New("no model")
	if _, _, err := Generate(ctx, errStartModel{want}, Request{}); !errors.Is(err, want) {
		t.Errorf("Generate = %v; want the start error", err)
	}
	msg, _, err := Generate(ctx, &scriptModel{turns: [][]Emit{textTurn("hi")}}, Request{})
	if err != nil || msg.Text() != "hi" {
		t.Errorf("Generate = %q, %v", msg.Text(), err)
	}
}

type errStartModel struct{ err error }

func (m errStartModel) Stream(context.Context, Request) (*Stream, error) { return nil, m.err }
