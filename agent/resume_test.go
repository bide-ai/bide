package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
)

// ValidateRunID refuses an empty ID, one that contains the sub-run separator, and one under @cap/.
func TestValidateRunID(t *testing.T) {
	for id, bad := range map[string]bool{"": true, "a>b": true, ">": true, "@cap/x": true, "plain": false, "with space/and:colon": false, "@capx": false, "x@cap/y": false} {
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

// markerCrashStore crashes at one exact point: the first persist of a run's completion marker
// (runCompleteStep), for the run named runID ("" = any run). Everything before it, the final
// answer included, is durably recorded; the marker is not, and the run unwinds as if the
// process died between the two writes.
type markerCrashStore struct {
	Store
	runID   string
	crashed atomic.Bool
}

func (s *markerCrashStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if name == runCompleteStep && (s.runID == "" || runID == s.runID) && s.crashed.CompareAndSwap(false, true) {
		return Entry{}, false, errCrash // crash: the marker is NOT persisted
	}
	return s.Store.Insert(ctx, runID, name, data)
}

// wantAnswerRecordedNoMarker checks the crash landed where intended: every scripted model turn
// is journaled (the final answer included) and the completion marker is not.
func wantAnswerRecordedNoMarker(t *testing.T, store *Journal, runID string, turns int) []Record {
	t.Helper()
	ctx := context.Background()
	recs, err := store.History(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	var models int
	for _, r := range recs {
		if r.Kind == StepModel {
			models++
		}
	}
	if models != turns {
		t.Fatalf("journal holds %d model turns before the crash, want all %d (the final answer included)", models, turns)
	}
	if done, err := IsComplete(ctx, store, runID); err != nil || done {
		t.Fatalf("IsComplete before resume = %v, %v; want false (the marker write crashed)", done, err)
	}
	return recs
}

// wantMarkerOnlyAppended checks the resume wrote the completion marker and nothing else.
func wantMarkerOnlyAppended(t *testing.T, store *Journal, runID string, before []Record) {
	t.Helper()
	ctx := context.Background()
	after, err := store.History(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 || !reflect.DeepEqual(after[:len(before)], before) ||
		after[len(before)].Name != runCompleteStep || after[len(before)].Kind != StepValue {
		t.Fatalf("resume changed the journal beyond the completion marker:\nbefore=%v\nafter =%v", stepNames(before), stepNames(after))
	}
	if done, err := IsComplete(ctx, store, runID); err != nil || !done {
		t.Fatalf("IsComplete after resume = %v, %v; want true", done, err)
	}
}

func stepNames(recs []Record) []string {
	names := make([]string, len(recs))
	for i, r := range recs {
		names[i] = r.Name
	}
	return names
}

// A crash after the run's final answer is durably journaled but before its completion marker
// is written. The resumed run must replay the recorded final turn and finish: return the
// recorded answer, make no model call (a real model's extra turn could change the answer, or
// request a tool under a new tool-use id that at-most-once would not recognize as a repeat),
// fire no side effect, and record only the missing marker. The resume model is greedy: any
// call it gets asks to charge again under a fresh id. Every entry point shares the loop.
func TestFinalAnswerCrash_ResumeReplaysAnswer(t *testing.T) {
	ctx := context.Background()
	chargeThenAnswer := [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}
	cases := []struct {
		name   string
		runID  string
		script [][]Emit
		opts   func(*Agent) *Agent
		entry  func(a *Agent) (string, error)
	}{
		{name: "Run", runID: "r1", script: chargeThenAnswer, entry: func(a *Agent) (string, error) {
			res, err := a.Run(ctx, "r1", UserText("pay"))
			var m Message
			if res != nil {
				m = res.Message
			}
			return textOf(m), err
		}},
		{name: "RunAnswerOnly", runID: "r1", script: [][]Emit{textTurn("done")}, entry: func(a *Agent) (string, error) {
			res, err := a.Run(ctx, "r1", UserText("hi"))
			var m Message
			if res != nil {
				m = res.Message
			}
			return textOf(m), err
		}},
		{
			// The run used its last allowed turn on the answer: resume must not refuse with
			// ErrMaxTurns for a turn it has no need to take.
			name: "RunAtMaxTurns", runID: "r1", script: chargeThenAnswer,
			opts: func(a *Agent) *Agent { return must(a.With(WithMaxTurns(2))) },
			entry: func(a *Agent) (string, error) {
				res, err := a.Run(ctx, "r1", UserText("pay"))
				var m Message
				if res != nil {
					m = res.Message
				}
				return textOf(m), err
			},
		},
		{
			// Likewise a run whose answer used up its token budget: resume must not refuse
			// with ErrBudgetExceeded.
			name: "RunAtTokenBudget", runID: "r1",
			script: [][]Emit{
				toolTurnWithUsage("c1", "charge", `{}`, Usage{InputTokens: 10}),
				textTurnWithUsage("done", Usage{InputTokens: 10}),
			},
			opts: func(a *Agent) *Agent { return must(a.With(WithTokenBudget(20))) },
			entry: func(a *Agent) (string, error) {
				res, err := a.Run(ctx, "r1", UserText("pay"))
				var m Message
				if res != nil {
					m = res.Message
				}
				return textOf(m), err
			},
		},
		{name: "Run Result", runID: "r1", script: chargeThenAnswer, entry: func(a *Agent) (string, error) {
			res, err := a.Run(ctx, "r1", UserText("pay"))
			if err != nil {
				return "", err
			}
			return textOf(res.Message), nil
		}},
		{name: "Run WithSaga", runID: "r1", script: chargeThenAnswer, entry: func(a *Agent) (string, error) {
			res, err := a.Run(ctx, "r1", UserText("pay"), WithSaga())
			var m Message
			if res != nil {
				m = res.Message
			}
			return textOf(m), err
		}},
		{name: "Stream WithSaga", runID: "r1", script: chargeThenAnswer, entry: func(a *Agent) (string, error) {
			res, err := a.Stream(ctx, "r1", UserText("pay"), WithSaga()).Result()
			var m Message
			if res != nil {
				m = res.Message
			}
			return textOf(m), err
		}},
		{name: "Stream", runID: "r1", script: chargeThenAnswer, entry: func(a *Agent) (string, error) {
			as := a.Stream(ctx, "r1", UserText("pay"))
			var finished bool
			var live int
			for ev := range as.Events() {
				switch e := ev.(type) {
				case Finished:
					finished = true
				case AssistantTurn:
					if !e.Replayed {
						live++
					}
				}
			}
			res, err := as.Result()
			if err != nil {
				return "", err
			}
			m := res.Message
			if !finished {
				return "", errors.New("no Finished event")
			}
			if live > 0 {
				return "", fmt.Errorf("resume streamed %d live model turns", live)
			}
			return textOf(m), nil
		}},
		{name: "RunTypedText", runID: "r1", script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn(`{"name":"done"}`)},
			entry: func(a *Agent) (string, error) {
				out, _, err := a.RunTyped[typedAnswer](ctx, "r1", UserText("pay"))
				return out.Name, err
			}},
		{name: "RunTypedTool", runID: "r1", script: [][]Emit{toolTurn("c1", "charge", `{}`), toolTurn("f1", "final_answer", `{"name":"done"}`)},
			entry: func(a *Agent) (string, error) {
				out, _, err := a.RunTyped[typedAnswer](ctx, "r1", UserText("pay"))
				return out.Name, err
			}},
		{name: "RunTyped OutputNative", runID: "r1", script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn(`{"name":"done"}`)},
			entry: func(a *Agent) (string, error) {
				out, _, err := a.RunTyped[typedAnswer](ctx, "r1", UserText("pay"), WithOutputMode(OutputNative))
				return out.Name, err
			}},
		{name: "SessionSend", runID: sessionTurnRunID("s", 0), script: chargeThenAnswer, entry: func(a *Agent) (string, error) {
			s, err := a.Session(ctx, "s")
			if err != nil {
				return "", err
			}
			res, err := s.Send(ctx, UserText("pay"))
			var m Message
			if res != nil {
				m = res.Message
			}
			if err == nil && s.Turns() != 1 {
				return "", fmt.Errorf("session has %d turns, want 1", s.Turns())
			}
			return textOf(m), err
		}},
		{name: "SessionSendOnce", runID: sessionEventRunID("s", "k1"), script: chargeThenAnswer, entry: func(a *Agent) (string, error) {
			s, err := a.Session(ctx, "s")
			if err != nil {
				return "", err
			}
			res, err := s.SendOnce(ctx, "k1", UserText("pay"))
			var m Message
			if res != nil {
				m = res.Message
			}
			return textOf(m), err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			if opts == nil {
				opts = func(a *Agent) *Agent { return a }
			}
			store := NewMemStore()
			j := mustJournal(store)
			var charged int
			charge := &countingTool{name: "charge", safety: Safety{}, calls: &charged}
			wantCharged := 0
			for _, turn := range tc.script {
				if d, ok := turn[0].Event.(ToolCallDelta); ok && d.Name == "charge" {
					wantCharged++
				}
			}

			// First attempt: crashes between the final answer and the completion marker.
			first := &greedyModel{script: tc.script}
			if _, err := tc.entry(opts(mustNew(first, mustJournal(&markerCrashStore{Store: store, runID: tc.runID}), WithTools(charge)))); !errors.Is(err, errCrash) {
				t.Fatalf("first attempt: err = %v, want the injected crash at the completion marker", err)
			}
			if first.calls != len(tc.script) || charged != wantCharged {
				t.Fatalf("first attempt: %d model calls, %d charges; want %d, %d", first.calls, charged, len(tc.script), wantCharged)
			}
			before := wantAnswerRecordedNoMarker(t, j, tc.runID, len(tc.script))

			// Resume: the recorded final turn ends the run.
			resume := &greedyModel{}
			got, err := tc.entry(opts(mustNew(resume, j, WithTools(charge))))
			if err != nil {
				t.Fatalf("resume: %v", err)
			}
			if resume.calls != 0 {
				t.Errorf("resume made %d model calls after the final answer was recorded, want 0", resume.calls)
			}
			if charged != wantCharged {
				t.Errorf("resume fired the side effect again: %d charges, want %d", charged, wantCharged)
			}
			if got != "done" {
				t.Errorf("resume answered %q, want the recorded %q", got, "done")
			}
			wantMarkerOnlyAppended(t, j, tc.runID, before)
		})
	}
}

// The same crash inside a sub-agent: the sub-run's final answer is journaled but its marker is
// not, and the parent has no result for the sub-agent call. SubAgent is retry-safe, so the
// resumed parent calls it again; the sub-run must replay its recorded answer rather than ask
// its model for another turn (which here would charge again under a new id).
func TestFinalAnswerCrash_SubAgentResume(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	j := mustJournal(store)
	var charged int
	charge := &countingTool{name: "charge", safety: Safety{}, calls: &charged}

	subRunID := SubRunID("root", "c1")
	subFirst := &greedyModel{script: [][]Emit{toolTurn("s1", "charge", `{}`), textTurn("sub-done")}}
	if _, err := mustNew(subFirst, mustJournal(&markerCrashStore{Store: store, runID: subRunID}), WithTools(charge)).Run(asToolCall(ctx, "root", "c1"), subRunID, UserText("charge it")); !errors.Is(err, errCrash) {
		t.Fatalf("sub-run: err = %v, want the injected crash at the completion marker", err)
	}
	before := wantAnswerRecordedNoMarker(t, j, subRunID, 2)
	// The parent's journal holds the turn that called the sub-agent, and no result for it.
	asst := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "worker", Args: json.RawMessage(`{"task":"charge it"}`)}}}
	if _, err := j.do(ctx, "root", "@llm/0", func(context.Context) (Record, error) {
		return Record{Kind: StepModel, Message: &asst}, nil
	}); err != nil {
		t.Fatal(err)
	}

	subResume := &greedyModel{}
	parent := mustNew(
		&greedyModel{script: [][]Emit{textTurn("parent-done")}},
		j,
		WithTools(MustSubAgent("worker", "does work", mustNew(subResume, j, WithTools(charge)))),
	)
	res, err := parent.Run(ctx, "root", UserText("delegate"))
	if err != nil {
		t.Fatalf("parent resume: %v", err)
	}
	out := res.Message
	if subResume.calls != 0 || charged != 1 {
		t.Fatalf("resumed sub-run made %d model calls and charged %d times; want 0, 1", subResume.calls, charged)
	}
	if textOf(out) != "parent-done" {
		t.Fatalf("parent answered %q, want parent-done", textOf(out))
	}
	recs, err := j.History(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	var subResult string
	for _, r := range recs {
		if r.Kind == StepToolResult && r.ToolUseID == "c1" {
			_ = json.Unmarshal(r.Result, &subResult)
		}
	}
	if subResult != "sub-done" {
		t.Fatalf("parent recorded sub-agent result %q, want the sub-run's recorded answer sub-done", subResult)
	}
	wantMarkerOnlyAppended(t, j, subRunID, before)
}

// greedyModel is the worst case for re-entering a finished run: after its script runs out,
// its next call asks to charge again under a fresh tool-use id, which at-most-once (keyed by
// tool-use id) would not recognize as a repeat; later calls answer with text, so a regression
// fails as a second charge rather than looping. calls counts every model call.
type greedyModel struct {
	script [][]Emit
	calls  int
}

func (m *greedyModel) Stream(_ context.Context, _ Request) (*Stream, error) {
	m.calls++
	var turn []Emit
	switch {
	case m.calls <= len(m.script):
		turn = m.script[m.calls-1]
	case m.calls == len(m.script)+1:
		turn = toolTurn(fmt.Sprintf("again-%d", m.calls), "charge", `{}`)
	default:
		turn = textTurn("done again")
	}
	ch := make(chan Emit, len(turn))
	for _, e := range turn {
		ch <- e
	}
	close(ch)
	return NewStream(ch), nil
}

// finishedCharge completes one run that charges once, and returns its store, answer, and
// journal, plus a fresh greedy model for the re-entries.
func finishedCharge(t *testing.T, charged *int) (*Journal, Message, []Record) {
	t.Helper()
	store := memJournal()
	charge := &countingTool{name: "charge", safety: Safety{}, calls: charged} // a non-idempotent write
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	res, err := mustNew(m, store, WithTools(charge)).Run(context.Background(), "r1", UserText("pay"))
	var out Message
	if res != nil {
		out = res.Message
	}
	if err != nil || *charged != 1 {
		t.Fatalf("first run: err=%v charged=%d, want nil/1", err, *charged)
	}
	recs, err := store.History(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	return store, out, recs
}

// wantUntouched checks a re-entry changed nothing: no model call, no second charge, the
// same answer, and not one journal record appended.
func wantUntouched(t *testing.T, store *Journal, m *greedyModel, charged int, before []Record, gotAnswer, wantAnswer Message) {
	t.Helper()
	if m.calls != 0 {
		t.Fatalf("re-entering a finished run called the model %d times, want 0", m.calls)
	}
	if charged != 1 {
		t.Fatalf("re-entering a finished run charged again: %d charges, want 1", charged)
	}
	if !reflect.DeepEqual(gotAnswer, wantAnswer) {
		t.Fatalf("answer = %q, want the recorded %q", textOf(gotAnswer), textOf(wantAnswer))
	}
	after, err := store.History(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("re-entering a finished run changed the journal:\nbefore=%+v\nafter =%+v", before, after)
	}
}

// Re-invoking a finished run (a client retrying after a lost response, a redelivered job)
// returns the recorded answer and never asks the model again, so a side effect cannot fire
// a second time under a new tool-use id. A re-invocation with another input is ErrConfig (#137,
// R137-2): the recorded answer is not that input's. Neither touches the journal.
func TestFinishedRun_RunIsFinal(t *testing.T) {
	var charged int
	store, first, before := finishedCharge(t, &charged)
	charge := &countingTool{name: "charge", safety: Safety{}, calls: &charged}
	for i := 0; i < 3; i++ {
		m := &greedyModel{}
		res, err := mustNew(m, store, WithTools(charge)).Run(context.Background(), "r1", UserText("pay"))
		if err != nil {
			t.Fatalf("re-run %d: %v", i, err)
		}
		out := res.Message
		wantUntouched(t, store, m, charged, before, out, first)
		m = &greedyModel{}
		var res2 *Result
		res2, err = mustNew(m, store, WithTools(charge)).Run(context.Background(), "r1", UserText("a different input"))
		if res2 != nil {
			out = res2.Message
		}
		if !errors.Is(err, ErrConfig) {
			t.Fatalf("re-run %d with a different input = %q, %v; want ErrConfig", i, textOf(out), err)
		}
		wantUntouched(t, store, m, charged, before, first, first)
	}
}

// The same holds through Run (zero usage, zero live turns), saga Run, Stream, and
// saga Stream, which all share the loop.
func TestFinishedRun_EveryEntryPoint(t *testing.T) {
	ctx := context.Background()
	entries := map[string]func(a *Agent) (Message, error){
		"Run Result": func(a *Agent) (Message, error) {
			res, err := a.Run(ctx, "r1", UserText("pay"))
			if err == nil && (res.Usage != (Usage{}) || res.Turns != 0) {
				return Message{}, fmt.Errorf("Run usage=%+v turns=%d, want zero for a finished run", res.Usage, res.Turns)
			}
			return res.Message, err
		},
		"Run WithSaga": func(a *Agent) (Message, error) {
			res, err := a.Run(ctx, "r1", UserText("pay"), WithSaga())
			if err != nil {
				return Message{}, err
			}
			return res.Message, nil
		},
		"Stream": func(a *Agent) (Message, error) {
			as := a.Stream(ctx, "r1", UserText("pay"))
			var finished bool
			for ev := range as.Events() {
				switch e := ev.(type) {
				case Finished:
					finished = true
				case AssistantTurn:
					if !e.Replayed {
						return Message{}, errors.New("a finished run streamed a live model turn")
					}
				}
			}
			if !finished {
				return Message{}, errors.New("no Finished event for a finished run")
			}
			res, err := as.Result()
			if err != nil {
				return Message{}, err
			}
			return res.Message, nil
		},
		"Stream WithSaga": func(a *Agent) (Message, error) {
			res, err := a.Stream(ctx, "r1", UserText("pay"), WithSaga()).Result()
			if err != nil {
				return Message{}, err
			}
			return res.Message, nil
		},
	}
	for name, run := range entries {
		t.Run(name, func(t *testing.T) {
			var charged int
			store, first, before := finishedCharge(t, &charged)
			m := &greedyModel{}
			out, err := run(mustNew(m, store, WithTools(&countingTool{name: "charge", safety: Safety{}, calls: &charged})))
			if err != nil {
				t.Fatal(err)
			}
			wantUntouched(t, store, m, charged, before, out, first)
		})
	}
}

// A sub-agent re-entered on the parent's resume. The parent crashed after the sub-run
// finished but before it recorded the sub-agent tool's result; SubAgent is retry-safe, so the
// resumed parent calls it again. The sub-run must return its recorded answer, not ask its
// model for another turn, so the charge inside it does not fire twice.
func TestFinishedRun_SubAgentReentry(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	var charged int
	charge := &countingTool{name: "charge", safety: Safety{}, calls: &charged}
	subModel := &greedyModel{script: [][]Emit{toolTurn("s1", "charge", `{}`), textTurn("sub-done")}}
	sub := mustNew(subModel, store, WithTools(charge))

	// The sub-run completes (as it would have inside the first parent run).
	subRunID := SubRunID("root", "c1")
	if _, err := sub.Run(asToolCall(ctx, "root", "c1"), subRunID, UserText("charge it")); err != nil || charged != 1 {
		t.Fatalf("sub-run: err=%v charged=%d, want nil/1", err, charged)
	}
	// The parent's journal holds the turn that called the sub-agent, and no result for it.
	asst := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "worker", Args: json.RawMessage(`{"task":"charge it"}`)}}}
	if _, err := store.do(ctx, "root", "@llm/0", func(context.Context) (Record, error) {
		return Record{Kind: StepModel, Message: &asst}, nil
	}); err != nil {
		t.Fatal(err)
	}

	subCalls := subModel.calls
	parent := mustNew(
		&greedyModel{script: [][]Emit{textTurn("parent-done")}},
		store,
		WithTools(MustSubAgent("worker", "does work", sub)),
	)
	res, err := parent.Run(ctx, "root", UserText("delegate"))
	if err != nil {
		t.Fatalf("parent resume: %v", err)
	}
	out := res.Message
	if textOf(out) != "parent-done" || charged != 1 || subModel.calls != subCalls {
		t.Fatalf("out=%q charged=%d new sub model calls=%d, want parent-done, 1 charge, 0 new sub calls", textOf(out), charged, subModel.calls-subCalls)
	}
}

// A session turn re-entered after a crash between the turn finishing and the session
// recording it: Send retries the same turn run, which must return the recorded answer.
func TestFinishedRun_SessionTurnReentry(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	var charged int
	charge := &countingTool{name: "charge", safety: Safety{}, calls: &charged}
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	a := mustNew(m, store, WithTools(charge))

	// Turn 0's run finishes, but the session-level record of the turn was never written.
	turn := sessionTurnRunID("s", 0)
	if _, _, _, err := a.run(withSessionRun(ctx, turn), turn, &driveSpec{input: ptrTo(UserText("pay")), kind: RunKindSessionTurn, session: &SessionRef{ID: "s"}}); err != nil || charged != 1 {
		t.Fatalf("turn run: err=%v charged=%d", err, charged)
	}
	calls := m.calls
	s, err := a.Session(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Send(ctx, UserText("pay"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	out := res.Message
	if textOf(out) != "done" || charged != 1 || m.calls != calls || s.Turns() != 1 {
		t.Fatalf("out=%q charged=%d new model calls=%d turns=%d, want done, 1, 0, 1", textOf(out), charged, m.calls-calls, s.Turns())
	}
}

func ptrTo[T any](v T) *T { return &v }
