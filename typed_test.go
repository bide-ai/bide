package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// ExampleRunTyped shows structured output: RunTyped returns a typed value, decoded from
// a schema-guided final_answer tool call (a real adapter streams the call the same way).
func ExampleRunTyped() {
	type Weather struct {
		City  string `json:"city"`
		TempF int    `json:"temp_f"`
	}
	m := &scriptModel{turns: [][]Emit{
		toolTurn("c1", "final_answer", `{"city":"SF","temp_f":68}`),
		textTurn("done"),
	}}
	a := New(m, NewMemStore())

	w, err := RunTyped[Weather](context.Background(), a, "run-1", "weather in SF?")
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
	a := New(m, NewMemStore())

	got, err := RunTyped[answer](context.Background(), a, "r", "what is the meaning?")
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
	a := New(m, NewMemStore())

	got, err := RunTyped[answer](context.Background(), a, "r", "q")
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
	a := New(m, NewMemStore(), work)

	got, err := RunTyped[answer](context.Background(), a, "r", "q")
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
	store := NewMemStore()

	crashy := &scriptModel{turns: [][]Emit{
		toolTurn("c1", "final_answer", `{"answer":"42","score":9}`),
		errTurn(errCrash),
	}}
	if _, err := RunTyped[answer](context.Background(), New(crashy, store), "r", "q"); err == nil {
		t.Fatal("expected crash on first attempt")
	}

	recovered := &scriptModel{turns: [][]Emit{textTurn("done")}}
	got, err := RunTyped[answer](context.Background(), New(recovered, store), "r", "q")
	if err != nil {
		t.Fatalf("resume RunTyped: %v", err)
	}
	if got.Answer != "42" || got.Score != 9 {
		t.Fatalf("got %+v, want {42 9} from journal", got)
	}
}

// If the agent already has a tool named final_answer, RunTyped refuses (config error).
func TestRunTyped_RejectsNameCollision(t *testing.T) {
	var c int
	clash := &countingTool{name: "final_answer", safety: Safety{ReadOnly: true}, calls: &c}
	a := New(&scriptModel{turns: [][]Emit{textTurn("x")}}, NewMemStore(), clash)

	_, err := RunTyped[answer](context.Background(), a, "r", "q")
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want errors.Is ErrConfig", err)
	}
}
