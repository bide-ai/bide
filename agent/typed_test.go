package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// ExampleAgent_RunTyped shows structured output: RunTyped returns a typed value, decoded from
// a schema-guided final_answer tool call (a real adapter streams the call the same way).
func ExampleAgent_RunTyped() {
	type Weather struct {
		City  string `json:"city"`
		TempF int    `json:"temp_f"`
	}
	m := &scriptModel{turns: [][]Emit{
		toolTurn("c1", "final_answer", `{"city":"SF","temp_f":68}`),
		textTurn("done"),
	}}
	a := mustNew(m, memJournal())

	w, _, err := a.RunTyped[Weather](context.Background(), "run-1", UserText("weather in SF?"))
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("%s is %d°F\n", w.City, w.TempF)
	// Output: SF is 68°F
}

type answer struct {
	Answer string `json:"answer"`
	Score  int    `json:"score"`
}

// The model calls final_answer with structured args; RunTyped decodes them into T.
func TestRunTyped_ExtractsFromToolCall(t *testing.T) {
	m := &scriptModel{turns: [][]Emit{
		toolTurn("c1", "final_answer", `{"answer":"42","score":9}`),
		textTurn("done"),
	}}
	a := mustNew(m, memJournal())

	got, _, err := a.RunTyped[answer](context.Background(), "r", UserText("what is the meaning?"))
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Answer != "42" || got.Score != 9 {
		t.Fatalf("got %+v, want {42 9}", got)
	}
}

// If the model answers in plain JSON text instead of calling the tool, RunTyped falls
// back to parsing that text.
func TestRunTyped_FallbackToText(t *testing.T) {
	m := &scriptModel{turns: [][]Emit{textTurn(`{"answer":"7","score":5}`)}}
	a := mustNew(m, memJournal())

	got, _, err := a.RunTyped[answer](context.Background(), "r", UserText("q"))
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Answer != "7" || got.Score != 5 {
		t.Fatalf("got %+v, want {7 5}", got)
	}
}

// A real tool runs first (doing work), then final_answer carries the typed result.
func TestRunTyped_WithWorkTool(t *testing.T) {
	var calls int
	work := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{
		toolTurn("c1", "lookup", `{"q":"x"}`),
		toolTurn("c2", "final_answer", `{"answer":"ok","score":1}`),
		textTurn("done"),
	}}
	a := mustNew(m, memJournal(), WithTools(work))

	got, _, err := a.RunTyped[answer](context.Background(), "r", UserText("q"))
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if calls != 1 {
		t.Fatalf("work tool ran %d times, want 1", calls)
	}
	if got.Answer != "ok" || got.Score != 1 {
		t.Fatalf("got %+v", got)
	}
}

// The typed value is recovered from the journal on resume — a crash after the
// final_answer call is recorded still yields the value.
func TestRunTyped_ResumeSafe(t *testing.T) {
	// The accepted final_answer ends the run, so the crash comes as the run is marked complete.
	store := mustJournal(&failOnceStore{MemStore: NewMemStore(), name: runCompleteStep})

	crashy := &scriptModel{turns: [][]Emit{
		toolTurn("c1", "final_answer", `{"answer":"42","score":9}`),
	}}
	if _, _, err := mustNew(crashy, store).RunTyped[answer](context.Background(), "r", UserText("q")); err == nil {
		t.Fatal("expected crash on first attempt")
	}

	recovered := &scriptModel{turns: [][]Emit{textTurn("done")}}
	got, _, err := mustNew(recovered, store).RunTyped[answer](context.Background(), "r", UserText("q"))
	if err != nil {
		t.Fatalf("resume RunTyped: %v", err)
	}
	if got.Answer != "42" || got.Score != 9 {
		t.Fatalf("got %+v, want {42 9} from journal", got)
	}
}

// RunTyped reserves the tool name final_answer for its answer, so New refuses a tool of that name.
func TestRunTyped_RejectsNameCollision(t *testing.T) {
	var c int
	clash := &countingTool{name: "final_answer", safety: Safety{ReadOnly: true}, calls: &c}
	if a, err := New(&scriptModel{turns: [][]Emit{textTurn("x")}}, memJournal(), WithTools(clash)); !errors.Is(err, ErrConfig) || a != nil {
		t.Fatalf("New = %v, %v; want nil and ErrConfig for a tool named final_answer", a, err)
	}
}
