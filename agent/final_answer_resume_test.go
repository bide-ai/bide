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
		{name: "RunResult", runID: "r1", script: chargeThenAnswer, entry: func(a *Agent) (string, error) {
			res, err := a.Run(ctx, "r1", UserText("pay"))
			if err != nil {
				return "", err
			}
			return textOf(res.Message), nil
		}},
		{name: "RunSaga", runID: "r1", script: chargeThenAnswer, entry: func(a *Agent) (string, error) {
			res, err := a.Run(ctx, "r1", UserText("pay"), WithSaga())
			var m Message
			if res != nil {
				m = res.Message
			}
			return textOf(m), err
		}},
		{name: "StreamSaga", runID: "r1", script: chargeThenAnswer, entry: func(a *Agent) (string, error) {
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
		{name: "RunTypedNative", runID: "r1", script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn(`{"name":"done"}`)},
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
		WithTools(SubAgent("worker", "does work", mustNew(subResume, j, WithTools(charge)))),
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
