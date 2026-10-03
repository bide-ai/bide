package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

var errCrash = errors.New("crash")

// ExampleAgent_Stream shows the streaming API: range Events for live progress, then
// call Final for the terminal answer. Run is the blocking equivalent of Final.
func ExampleAgent_Stream() {
	// A model that answers directly (a real adapter streams token deltas the same way).
	m := &scriptModel{turns: [][]Emit{textTurn("hello world")}}
	a := mustNew(m, memJournal())

	stream := a.Stream(context.Background(), "run-1", UserText("hi"))
	for ev := range stream.Events() {
		switch e := ev.(type) {
		case TurnStarted:
			fmt.Println("turn started")
		case ModelEvent:
			if d, ok := e.Event.(TextDelta); ok {
				fmt.Printf("delta: %q\n", d.Text)
			}
		case Finished:
			fmt.Println("finished")
		}
	}
	res, err := stream.Result()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	answer := res.Message
	fmt.Println("answer:", textOf(answer))
	// Output:
	// turn started
	// delta: "hello world"
	// finished
	// answer: hello world
}

// collect drains a stream's events and its final result together.
func collect(as *RunStream) ([]RunEvent, Message, error) {
	var evs []RunEvent
	for e := range as.Events() {
		evs = append(evs, e)
	}
	res, err := as.Result()
	var msg Message
	if res != nil {
		msg = res.Message
	}
	return evs, msg, err
}

func kinds(evs []RunEvent) []string {
	var out []string
	for _, e := range evs {
		switch ev := e.(type) {
		case TurnStarted:
			out = append(out, "turn")
		case ModelEvent:
			out = append(out, "model")
		case AssistantTurn:
			if ev.Replayed {
				out = append(out, "asst(replay)")
			} else {
				out = append(out, "asst")
			}
		case ToolStarted:
			out = append(out, "tool.start")
		case ToolCompleted:
			out = append(out, "tool.done")
		case ApprovalRequired:
			out = append(out, "approval")
		case Finished:
			out = append(out, "finished")
		}
	}
	return out
}

// Streaming surfaces the whole lifecycle: turn boundaries, live model deltas, tool
// start/finish, and a terminal Finished — and Final returns the same answer as Run.
func TestStream_EmitsLifecycle(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{"q":"x"}`), textTurn("final")}}
	a := mustNew(m, memJournal(), WithTools(tool))

	evs, out, err := collect(a.Stream(context.Background(), "run1", UserText("hi")))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := textOf(out); got != "final" {
		t.Fatalf("answer = %q, want %q", got, "final")
	}
	if calls != 1 {
		t.Fatalf("tool ran %d times, want 1", calls)
	}

	// Turn 1: tool call. Turn 2: text answer. Deltas arrive as ModelEvents before the
	// assembled AssistantTurn; the tool runs between the turns; Finished is last.
	want := []string{
		"turn", "model", "model", "asst", // model asks for the tool
		"tool.start", "tool.done", // tool executes
		"turn", "model", "model", "asst", // model answers
		"finished",
	}
	got := kinds(evs)
	if len(got) != len(want) {
		t.Fatalf("event kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}

	// The last event carries the same terminal message Final reports.
	fin, ok := evs[len(evs)-1].(Finished)
	if !ok || textOf(fin.Final) != "final" {
		t.Fatalf("last event = %+v, want Finished{final}", evs[len(evs)-1])
	}
}

// Final without ranging Events behaves exactly like Run (drains and discards events).
func TestStream_FinalOnlyMatchesRun(t *testing.T) {
	tool := func() Tool {
		var c int
		return &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &c}
	}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{"q":"x"}`), textTurn("done")}}
	a := mustNew(m, memJournal(), WithTools(tool()))

	res, err := a.Stream(context.Background(), "r", UserText("hi")).Result()
	if err != nil {
		t.Fatalf("Final: %v", err)
	}
	out := res.Message
	if got := textOf(out); got != "done" {
		t.Fatalf("answer = %q, want %q", got, "done")
	}
}

// On resume, a fresh stream replays the journaled transcript (AssistantTurn{Replayed}
// + ToolCompleted) before live progress, so a UI reconstructs the full story.
func TestStream_ReplaysJournalOnResume(t *testing.T) {
	store := memJournal()
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}

	// First attempt crashes after the tool result is journaled (model errors on turn 2).
	crashy := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{"q":"x"}`), errTurn(errCrash)}}
	if _, err := mustNew(crashy, store, WithTools(tool)).Run(context.Background(), "run1", UserText("hi")); err == nil {
		t.Fatal("expected crash on first attempt")
	}

	// Resume with a healthy model, streaming this time.
	recovered := &scriptModel{turns: [][]Emit{textTurn("final")}}
	evs, out, err := collect(mustNew(recovered, store, WithTools(tool)).Stream(context.Background(), "run1", UserText("hi")))
	if err != nil {
		t.Fatalf("resume Stream: %v", err)
	}
	if got := textOf(out); got != "final" {
		t.Fatalf("answer = %q, want %q", got, "final")
	}
	if calls != 1 {
		t.Fatalf("tool ran %d times across crash+resume, want 1", calls)
	}

	got := kinds(evs)
	// Replayed assistant turn + replayed tool result come first, then the live answer.
	want := []string{"asst(replay)", "tool.done", "turn", "model", "model", "asst", "finished"}
	if len(got) != len(want) {
		t.Fatalf("event kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}
