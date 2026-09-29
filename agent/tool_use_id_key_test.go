package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// runRecovering runs a and turns a panic into an error, so a test can report a crash as a
// failure instead of taking the whole test binary down.
func runRecovering(a *Agent, runID string) (out Message, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return a.Run(context.Background(), runID, "go")
}

// adversarialToolUseIDs are tool-use IDs a provider may send that name, or nearly name, a key
// the engine writes, another call's key, or another call's sub-run, plus IDs a store could
// refuse as a key (very long, non-ASCII).
func adversarialToolUseIDs() []string {
	return []string{
		"x", "tool:x", "attempt:tool:x", "attempt:step:x", "approval:x", "approval-tally:x",
		"@saga/compensate/x", "run:complete", "run:aborted", "@llm/0", "@llm/1", "@llm/2",
		"a:b", "a%3Ab", "a/b", "a>b", "a%3Eb", "a.b", "user@host", "with space", "日本語",
		"tab\there", "nul\x00byte", "~x",
		strings.Repeat("k", 5000), strings.Repeat(":", 2000), strings.Repeat("é", 1000),
	}
}

// tracker is a non-retry-safe tool that counts its calls by tool-use ID. It reads the ID from
// its run scope: the sub-run ID the loop gives the call ends in it.
type tracker struct {
	name  string
	calls atomic.Int32
}

func (t *tracker) Name() string                { return t.name }
func (t *tracker) Description() string         { return "" }
func (t *tracker) Safety() Safety              { return Safety{} }
func (t *tracker) ArgsSchema() json.RawMessage { return nil }
func (t *tracker) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	t.calls.Add(1)
	return json.RawMessage(`"ok"`), nil
}

// A model may send any tool-use ID. Each call must still run exactly once and be journaled
// under its own keys: before tool-use IDs were encoded into their own key space, an ID such as
// "@llm/1" or "run:complete" took the key of one of the run's own steps, so the loop was
// refused (ErrToolUseIDReused) or corrupted.
func TestRun_AnyToolUseIDKeysItsOwnRecords(t *testing.T) {
	ids := adversarialToolUseIDs()
	turn := make([]Emit, 0, len(ids)+1)
	for i, id := range ids {
		turn = append(turn, Emit{Event: ToolCallDelta{Index: i, ID: id, Name: "w", ArgsFragment: json.RawMessage(`{}`)}})
	}
	turn = append(turn, Emit{Event: Finish{Reason: "tool_use"}})
	tool := &tracker{name: "w"}
	store := NewMemStore()
	a := New(&scriptModel{turns: [][]Emit{turn, textTurn("done")}}, store, tool)
	out, err := runRecovering(a, "r")
	if err != nil || out.Text() != "done" || int(tool.calls.Load()) != len(ids) {
		t.Fatalf("out = %q, err = %v, tool ran %d times; want done, nil, %d", out.Text(), err, tool.calls.Load(), len(ids))
	}
	if ok, err := IsComplete(context.Background(), store, "r"); !ok || err != nil {
		t.Fatalf("IsComplete = %v, %v; want true", ok, err)
	}
	recs, _ := store.History(context.Background(), "r")
	results := map[string]int{}
	for _, r := range recs {
		if r.Kind == StepToolResult {
			results[r.ToolUseID]++
		}
	}
	for _, id := range ids {
		if results[id] != 1 {
			t.Fatalf("call %.40q has %d results in the journal, want 1", id, results[id])
		}
	}
	// A finished run replays: nothing runs again.
	if out, err := runRecovering(a, "r"); err != nil || out.Text() != "done" || int(tool.calls.Load()) != len(ids) {
		t.Fatalf("replay: out = %q, err = %v, tool ran %d times", out.Text(), err, tool.calls.Load())
	}
}

// Every key journaled for a call, and every sub-run ID, stays short and printable whatever the
// ID, so a store with a bounded, text key (Postgres refuses an index row over 2704 bytes)
// can record it.
func TestRun_ToolUseIDKeysAreBoundedAndPrintable(t *testing.T) {
	var scopes []string
	probe := Func("probe", "", Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
		scopes = append(scopes, RunScope(ctx))
		return "ok", nil
	})
	for _, id := range []string{strings.Repeat("k", 5000), strings.Repeat(":", 2000), "日本語", "nul\x00byte"} {
		scopes = nil
		store := NewMemStore()
		m := &scriptModel{turns: [][]Emit{toolTurn(id, "probe", `{}`), textTurn("done")}}
		if _, err := runRecovering(New(m, store, probe), "r"); err != nil {
			t.Fatalf("id %.20q: %v", id, err)
		}
		recs, _ := store.History(context.Background(), "r")
		names := append([]string(nil), scopes...)
		for _, r := range recs {
			names = append(names, r.Name)
		}
		for _, n := range names {
			if len(n) > 256 || strings.IndexFunc(n, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 {
				t.Fatalf("id %.20q: key or run ID %.60q (%d bytes) is not short, printable ASCII", id, n, len(n))
			}
		}
	}
}

// fixedTextModel is a model that answers every request with text.
type fixedTextModel string

func (m fixedTextModel) Stream(ctx context.Context, req Request) (*Stream, error) {
	return NewScriptedModel(TextTurn(string(m))).Stream(ctx, req)
}

// A sub-agent's run ID is derived from its call's ID, so no ID may name another call's sub-run:
// on main the root's call "a/b" ran as "r/a/b", the run of call "b" made by the sub-agent behind
// the root's call "a", and was handed that run's answer.
func TestRun_ToolUseIDsNeverNameAnotherSubRun(t *testing.T) {
	store := NewMemStore()
	b := New(fixedTextModel("from b"), store)
	a := New(NewScriptedModel(ToolTurn("b", "b", `{"task":"x"}`), TextTurn("a done")), store, SubAgent("b", "b", b))
	cModel := &countModel{inner: fixedTextModel("from c")}
	c := New(cModel, store)
	root := New(NewScriptedModel(
		ToolTurn("a", "a", `{"task":"x"}`),
		ToolTurn("a/b", "c", `{"task":"y"}`),
		ToolTurn("a>b", "c", `{"task":"y"}`),
		TextTurn("root done"),
	), store, SubAgent("a", "a", a), SubAgent("c", "c", c))
	if _, err := runRecovering(root, "r"); err != nil {
		t.Fatal(err)
	}
	if n := cModel.calls.Load(); n != 2 {
		t.Fatalf("sub-agent c's model ran %d times, want 2 (once per call)", n)
	}
	recs, _ := store.History(context.Background(), "r")
	for _, r := range recs {
		if r.Kind == StepToolResult && r.ToolUseID != "a" && string(r.Result) != `"from c"` {
			t.Fatalf("call %q returned %s, want sub-agent c's answer", r.ToolUseID, r.Result)
		}
	}
}

// A model turn with an ID that is not valid UTF-8 is refused: the journal cannot hold it (JSON
// rewrites the bad bytes), so the replayed ID would not be the one the live run keyed.
func TestRun_ToolUseIDNotValidUTF8IsRejected(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{toolTurn("bad\xff", "lookup", `{}`), textTurn("done")}}
	_, err := runRecovering(New(m, NewMemStore(), tool), "r")
	if !errors.Is(err, ErrToolUseIDReused) || calls != 0 {
		t.Fatalf("err = %v, tool ran %d times; want ErrToolUseIDReused and 0", err, calls)
	}
}

// crashOnKind runs the first step whose record has the given kind and then fails as if the
// process died before the record was written.
type crashOnKind struct {
	Durable
	kind    StepKind
	crashed atomic.Bool
}

func (c *crashOnKind) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	return c.Durable.Do(ctx, runID, name, func(ctx context.Context) (Record, error) {
		rec, err := fn(ctx)
		if err == nil && rec.Kind == c.kind && c.crashed.CompareAndSwap(false, true) {
			return Record{}, errDied
		}
		return rec, err
	})
}

// seedStepAttempt journals the attempt marker a side-effecting Step named name leaves when it
// dies mid-effect, attempted at at.
func seedStepAttempt(t *testing.T, d Durable, runID, name string, at time.Time) {
	t.Helper()
	rec := Record{Kind: StepAttempt, ToolUseID: name, AttemptedAt: at.UnixMilli()}
	if _, _, err := ClaimAttempt(context.Background(), d, runID, "attempt:step:"+name, rec); err != nil {
		t.Fatal(err)
	}
}

// A Step and a tool call may share a string (a step "x", a call "x"): the step's attempt marker
// is not the call's. On main the resume gate read any StepAttempt record by its ToolUseID, so a
// call that never started halted as if its effect were unknown.
func TestRun_StepAttemptIsNotAToolCallAttempt(t *testing.T) {
	inner := NewMemStore()
	seedStepAttempt(t, inner, "r", "x", time.Now())
	tool := &tracker{name: "w"}
	m := &scriptModel{turns: [][]Emit{toolTurn("x", "w", `{}`), textTurn("done")}}
	store := &crashOnKind{Durable: inner, kind: StepAttempt} // the call's claim dies before it is written
	a := New(m, store, tool)
	if _, err := runRecovering(a, "r"); !errors.Is(err, errDied) {
		t.Fatalf("first run: %v, want the simulated crash", err)
	}
	out, err := runRecovering(a, "r")
	if err != nil || out.Text() != "done" || tool.calls.Load() != 1 {
		t.Fatalf("resume: out = %q, err = %v, tool ran %d times; want done, nil, 1", out.Text(), err, tool.calls.Load())
	}
}

// ResolveHalt's minimum age is measured from the call's own attempt, not from a step's that
// shares its string: on main an old step marker let a call attempted a moment ago be resolved.
func TestResolveHalt_MinAgeReadsTheCallsOwnAttempt(t *testing.T) {
	inner := NewMemStore()
	now := time.Now()
	seedStepAttempt(t, inner, "r", "x", now.Add(-time.Hour))
	tool := &tracker{name: "w"}
	m := &scriptModel{turns: [][]Emit{toolTurn("x", "w", `{}`), textTurn("done")}}
	store := &crashOnKind{Durable: inner, kind: StepToolResult} // the call's result dies before it is written
	if _, err := runRecovering(New(m, store, tool), "r"); !errors.Is(err, errDied) {
		t.Fatalf("run: %v, want the simulated crash", err)
	}
	err := ResolveHalt(context.Background(), inner, "r", "x", "ok", false,
		WithMinHaltAge(time.Minute), WithNow(func() time.Time { return now }))
	var young *HaltTooYoung
	if !errors.As(err, &young) {
		t.Fatalf("ResolveHalt = %v, want *HaltTooYoung: the call was attempted just now", err)
	}
}

// A saga rollback skips a call that never started, even when a step with the same string did:
// on main the step's attempt marker made the call look started, and the rollback halted.
func TestSaga_RollbackStepAttemptIsNotAToolCallAttempt(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	seedStepAttempt(t, store, "r1", "w1", time.Now())
	// The model called write (w1) and book (b1); the booking failed and aborted the saga before
	// the write started.
	turn := &Message{Role: RoleAssistant, Parts: []Part{
		ToolUse{ID: "w1", Name: "write", Args: json.RawMessage(`{}`)},
		ToolUse{ID: "b1", Name: "book", Args: json.RawMessage(`{}`)},
	}}
	for _, r := range []struct {
		name string
		rec  Record
	}{
		{"@llm/0", Record{Kind: StepModel, Message: turn}},
		{"b1 failed", Record{Kind: StepSagaFail, ToolUseID: "b1", Result: mustJSON("no seats")}},
	} {
		if _, err := store.Do(ctx, "r1", r.name, func(context.Context) (Record, error) { return r.rec, nil }); err != nil {
			t.Fatal(err)
		}
	}
	write := Func("write", "", Safety{}, func(context.Context, struct{}) (string, error) { return "ok", nil })
	book := Func("book", "", Safety{}, func(context.Context, struct{}) (string, error) { return "", errors.New("no seats") })
	_, err := New(fixedTextModel("done"), store, write, book).RunSaga(ctx, "r1", "trip")
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || aborted.CompensateErr != nil {
		t.Fatalf("err = %v, want *SagaAborted with a finished rollback (the write never started)", err)
	}
}
