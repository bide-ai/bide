package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

type typedAnswer struct {
	Name string `json:"name"`
}

// countModel counts model calls and delegates to another model.
type countModel struct {
	inner Model
	calls atomic.Int32
}

func (m *countModel) Stream(ctx context.Context, req Request) (*Stream, error) {
	m.calls.Add(1)
	return m.inner.Stream(ctx, req)
}

// eventTurnsModel emits turns[i] on the call that follows i assistant turns (the last one repeats).
type eventTurnsModel [][]Event

func (m eventTurnsModel) Stream(_ context.Context, req Request) (*Stream, error) {
	i := 0
	for _, msg := range req.Messages {
		if msg.Role == RoleAssistant {
			i++
		}
	}
	turn := m[min(i, len(m)-1)]
	ch := make(chan Emit, len(turn))
	for _, e := range turn {
		ch <- Emit{Event: e}
	}
	close(ch)
	return NewStream(ch), nil
}

// A final_answer call completes the run: the answer exists, so RunTyped does not ask the model
// for another turn. Before, the run went on until the model chose to stop, so a model that kept
// answering ran into ErrMaxTurns with an answer in hand.
func TestRunTyped_EndsAtFinalAnswer(t *testing.T) {
	m := &countModel{inner: NewScriptedModel(
		ToolTurn("f1", finalAnswerTool, `{"name":"first"}`),
		ToolTurn("f2", finalAnswerTool, `{"name":"second"}`),
		ToolTurn("f3", finalAnswerTool, `{"name":"third"}`),
	)}
	a := New(m, NewMemStore()).WithMaxTurns(3)
	got, err := RunTyped[typedAnswer](context.Background(), a, "r", "go")
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Name != "first" || m.calls.Load() != 1 {
		t.Fatalf("RunTyped = %+v after %d model calls, want the first answer after 1", got, m.calls.Load())
	}
}

// The answer is the first final_answer call the tool accepted: a call whose arguments do not
// decode is an error the model corrects, not the answer.
func TestRunTyped_AnswerIsTheAcceptedCall(t *testing.T) {
	m := &countModel{inner: NewScriptedModel(
		ToolTurn("f1", finalAnswerTool, `{"name":5}`),
		ToolTurn("f2", finalAnswerTool, `{"name":"ok"}`),
		TextTurn("done"),
	)}
	got, err := RunTyped[typedAnswer](context.Background(), New(m, NewMemStore()), "r", "go")
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Name != "ok" || m.calls.Load() != 2 {
		t.Fatalf("RunTyped = %+v after %d model calls, want \"ok\" after 2", got, m.calls.Load())
	}
}

// A final_answer call with no arguments is the answer the tool accepted, the zero value. It is
// not replaced by whatever prose the model wrote.
func TestRunTyped_EmptyFinalAnswerIsNotReplacedByProse(t *testing.T) {
	m := eventTurnsModel{
		{ToolCallDelta{Index: 0, ID: "f1", Name: finalAnswerTool}, Finish{Reason: "tool_use"}},
		{TextDelta{Text: `{"name":"from prose"}`}, Finish{Reason: "stop"}},
	}
	got, err := RunTyped[typedAnswer](context.Background(), New(m, NewMemStore()), "r", "go")
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got != (typedAnswer{}) {
		t.Fatalf("RunTyped = %+v, want the zero answer the final_answer call carried", got)
	}
}

// Two accepted final_answer calls in one turn: the first, in the model's order, is the answer.
func TestRunTyped_FirstAcceptedCallInATurnWins(t *testing.T) {
	m := eventTurnsModel{{
		ToolCallDelta{Index: 0, ID: "f1", Name: finalAnswerTool, ArgsFragment: []byte(`{"name":"a"}`)},
		ToolCallDelta{Index: 1, ID: "f2", Name: finalAnswerTool, ArgsFragment: []byte(`{"name":"b"}`)},
		Finish{Reason: "tool_use"},
	}}
	got, err := RunTyped[typedAnswer](context.Background(), New(m, NewMemStore()), "r", "go")
	if err != nil || got.Name != "a" {
		t.Fatalf("RunTyped = %+v, %v; want the first call's answer \"a\"", got, err)
	}
}

// failOnceStore fails the first write of one step name, standing in for a crash just before it.
type failOnceStore struct {
	*MemStore
	name   string
	failed atomic.Bool
}

func (s *failOnceStore) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	if name == s.name && s.failed.CompareAndSwap(false, true) {
		return Record{}, errors.New("crash")
	}
	return s.MemStore.Do(ctx, runID, name, fn)
}

// A run that crashed after its final_answer was recorded, but before it was marked complete,
// completes on resume without asking the model again.
func TestRunTyped_ResumeAfterFinalAnswerDoesNotAskAgain(t *testing.T) {
	store := &failOnceStore{MemStore: NewMemStore(), name: runCompleteStep}
	m := &countModel{inner: NewScriptedModel(
		ToolTurn("f1", finalAnswerTool, `{"name":"first"}`),
		ToolTurn("f2", finalAnswerTool, `{"name":"second"}`),
	)}
	a := New(m, store).WithMaxTurns(3)
	if _, err := RunTyped[typedAnswer](context.Background(), a, "r", "go"); err == nil {
		t.Fatal("the first attempt should fail writing the completion marker")
	}
	got, err := RunTyped[typedAnswer](context.Background(), a, "r", "go")
	if err != nil {
		t.Fatalf("resumed RunTyped: %v", err)
	}
	if got.Name != "first" || m.calls.Load() != 1 {
		t.Fatalf("resumed RunTyped = %+v after %d model calls, want the first answer after 1", got, m.calls.Load())
	}
	if ok, _ := IsComplete(context.Background(), store, "r"); !ok {
		t.Fatal("the resumed run is not marked complete")
	}
}
