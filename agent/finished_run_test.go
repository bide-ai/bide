package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

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
func finishedCharge(t *testing.T, charged *int) (Durable, Message, []Record) {
	t.Helper()
	store := NewMemStore()
	charge := &countingTool{name: "charge", safety: Safety{}, calls: charged} // a non-idempotent write
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	out, err := New(m, store, charge).Run(context.Background(), "r1", "pay")
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
func wantUntouched(t *testing.T, store Durable, m *greedyModel, charged int, before []Record, gotAnswer, wantAnswer Message) {
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
// a second time under a new tool-use id.
func TestFinishedRun_RunIsFinal(t *testing.T) {
	var charged int
	store, first, before := finishedCharge(t, &charged)
	charge := &countingTool{name: "charge", safety: Safety{}, calls: &charged}
	for i := 0; i < 3; i++ {
		m := &greedyModel{}
		out, err := New(m, store, charge).Run(context.Background(), "r1", "a different input")
		if err != nil {
			t.Fatalf("re-run %d: %v", i, err)
		}
		wantUntouched(t, store, m, charged, before, out, first)
	}
}

// The same holds through RunResult (zero usage, zero live turns), RunSaga, Stream, and
// StreamSaga, which all share the loop.
func TestFinishedRun_EveryEntryPoint(t *testing.T) {
	ctx := context.Background()
	entries := map[string]func(a *Agent) (Message, error){
		"RunResult": func(a *Agent) (Message, error) {
			res, err := a.RunResult(ctx, "r1", "pay")
			if err == nil && (res.Usage != (Usage{}) || res.Turns != 0) {
				return Message{}, fmt.Errorf("RunResult usage=%+v turns=%d, want zero for a finished run", res.Usage, res.Turns)
			}
			return res.Message, err
		},
		"RunSaga": func(a *Agent) (Message, error) { return a.RunSaga(ctx, "r1", "pay") },
		"Stream": func(a *Agent) (Message, error) {
			as := a.Stream(ctx, "r1", "pay")
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
			return as.Final()
		},
		"StreamSaga": func(a *Agent) (Message, error) { return a.StreamSaga(ctx, "r1", "pay").Final() },
	}
	for name, run := range entries {
		t.Run(name, func(t *testing.T) {
			var charged int
			store, first, before := finishedCharge(t, &charged)
			m := &greedyModel{}
			out, err := run(New(m, store, &countingTool{name: "charge", safety: Safety{}, calls: &charged}))
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
	store := NewMemStore()
	var charged int
	charge := &countingTool{name: "charge", safety: Safety{}, calls: &charged}
	subModel := &greedyModel{script: [][]Emit{toolTurn("s1", "charge", `{}`), textTurn("sub-done")}}
	sub := New(subModel, store, charge)

	// The sub-run completes (as it would have inside the first parent run).
	const subRunID = "root/c1"
	if _, err := sub.Run(ctx, subRunID, "charge it"); err != nil || charged != 1 {
		t.Fatalf("sub-run: err=%v charged=%d, want nil/1", err, charged)
	}
	// The parent's journal holds the turn that called the sub-agent, and no result for it.
	asst := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "worker", Args: json.RawMessage(`{"task":"charge it"}`)}}}
	if _, err := store.Do(ctx, "root", "@llm/0", func(context.Context) (Record, error) {
		return Record{Kind: StepModel, Message: &asst}, nil
	}); err != nil {
		t.Fatal(err)
	}

	subCalls := subModel.calls
	parent := New(&greedyModel{script: [][]Emit{textTurn("parent-done")}}, store, SubAgent("worker", "does work", sub))
	out, err := parent.Run(ctx, "root", "delegate")
	if err != nil {
		t.Fatalf("parent resume: %v", err)
	}
	if textOf(out) != "parent-done" || charged != 1 || subModel.calls != subCalls {
		t.Fatalf("out=%q charged=%d new sub model calls=%d, want parent-done, 1 charge, 0 new sub calls", textOf(out), charged, subModel.calls-subCalls)
	}
}

// A session turn re-entered after a crash between the turn finishing and the session
// recording it: Send retries the same turn run, which must return the recorded answer.
func TestFinishedRun_SessionTurnReentry(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	var charged int
	charge := &countingTool{name: "charge", safety: Safety{}, calls: &charged}
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	a := New(m, store, charge)

	// Turn 0's run finishes, but the session-level record of the turn was never written.
	if _, _, _, err := a.run(ctx, "s/t0", []Message{UserText("pay")}, false, nil); err != nil || charged != 1 {
		t.Fatalf("turn run: err=%v charged=%d", err, charged)
	}
	calls := m.calls
	s, err := a.Session(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Send(ctx, "pay")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if textOf(out) != "done" || charged != 1 || m.calls != calls || s.Turns() != 1 {
		t.Fatalf("out=%q charged=%d new model calls=%d turns=%d, want done, 1, 0, 1", textOf(out), charged, m.calls-calls, s.Turns())
	}
}
