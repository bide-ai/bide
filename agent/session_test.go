package agent

import (
	"context"
	"testing"
)

// A session carries the transcript across turns: turn 2's model call sees turn 1's Q&A.
func TestSession_MultiTurnCarriesHistory(t *testing.T) {
	var got Request
	inner := &scriptModel{turns: [][]Emit{textTurn("hello"), textTurn("as I said, hello")}}
	m := &captureModel{inner: inner, got: &got}
	a := mustNew(m, memJournal())

	s, err := a.Session(context.Background(), "conv")
	if err != nil {
		t.Fatal(err)
	}

	res, err := s.Send(context.Background(), UserText("hi"))
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	a1 := res.Message
	if textOf(a1) != "hello" {
		t.Fatalf("answer 1 = %q", textOf(a1))
	}

	res2, err := s.Send(context.Background(), UserText("what did you say?"))
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	a2 := res2.Message
	if textOf(a2) != "as I said, hello" {
		t.Fatalf("answer 2 = %q", textOf(a2))
	}

	// Turn 2's model call must have been seeded with the full prior transcript + new input.
	msgs := got.Messages
	if len(msgs) != 3 {
		t.Fatalf("turn 2 seed had %d messages, want 3 (user, assistant, user): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != RoleUser || textOf(msgs[0]) != "hi" {
		t.Errorf("seed[0] = %+v, want user 'hi'", msgs[0])
	}
	if msgs[1].Role != RoleAssistant || textOf(msgs[1]) != "hello" {
		t.Errorf("seed[1] = %+v, want assistant 'hello'", msgs[1])
	}
	if msgs[2].Role != RoleUser || textOf(msgs[2]) != "what did you say?" {
		t.Errorf("seed[2] = %+v, want user question", msgs[2])
	}

	if s.Turns() != 2 || len(s.History()) != 4 {
		t.Fatalf("turns=%d history=%d, want 2 and 4", s.Turns(), len(s.History()))
	}
}

// A session reloaded from the same store rebuilds the transcript and continues.
func TestSession_DurableReloadContinues(t *testing.T) {
	store := memJournal()
	m := &scriptModel{turns: [][]Emit{textTurn("a1"), textTurn("a2"), textTurn("a3")}}
	a := mustNew(m, store)

	s1, err := a.Session(context.Background(), "conv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Send(context.Background(), UserText("q1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Send(context.Background(), UserText("q2")); err != nil {
		t.Fatal(err)
	}

	// Fresh Session over the SAME store (simulating a restart) rebuilds the transcript.
	s2, err := a.Session(context.Background(), "conv")
	if err != nil {
		t.Fatal(err)
	}
	if s2.Turns() != 2 || len(s2.History()) != 4 {
		t.Fatalf("reloaded turns=%d history=%d, want 2 and 4", s2.Turns(), len(s2.History()))
	}
	h := s2.History()
	if textOf(h[0]) != "q1" || textOf(h[1]) != "a1" || textOf(h[2]) != "q2" || textOf(h[3]) != "a2" {
		t.Fatalf("reconstructed transcript wrong: %q/%q/%q/%q",
			textOf(h[0]), textOf(h[1]), textOf(h[2]), textOf(h[3]))
	}

	res, err := s2.Send(context.Background(), UserText("q3"))
	if err != nil {
		t.Fatalf("turn 3 after reload: %v", err)
	}
	a3 := res.Message
	if textOf(a3) != "a3" || s2.Turns() != 3 {
		t.Fatalf("turn 3 answer=%q turns=%d", textOf(a3), s2.Turns())
	}
}

// A tool call in a turn does not leak into the next turn's transcript (memory is Q&A).
func TestSession_ToolTurnThenPlainHistory(t *testing.T) {
	var got Request
	inner := &scriptModel{turns: [][]Emit{
		toolTurn("c1", "lookup", `{"q":"x"}`), textTurn("found it"), // turn 1: tool then answer
		textTurn("recalled"), // turn 2
	}}
	m := &captureModel{inner: inner, got: &got}
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	a := mustNew(m, memJournal(), WithTools(tool))

	s, _ := a.Session(context.Background(), "c")
	if _, err := s.Send(context.Background(), UserText("look")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(context.Background(), UserText("again")); err != nil {
		t.Fatal(err)
	}

	// Turn 2's seed is [user "look", assistant "found it", user "again"] — no tool_use /
	// tool_result from turn 1 leaks in.
	msgs := got.Messages
	if len(msgs) != 3 {
		t.Fatalf("turn 2 seed = %d messages, want 3 (no tool leakage): %+v", len(msgs), msgs)
	}
	if textOf(msgs[1]) != "found it" {
		t.Errorf("seed[1] should be the final answer, got %+v", msgs[1])
	}
}
