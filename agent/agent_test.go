package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// scriptModel returns a scripted set of events per Stream call (one entry per turn).
type scriptModel struct {
	turns [][]Emit
	i     int
}

func (m *scriptModel) Stream(_ context.Context, _ Request) (*Stream, error) {
	turn := m.turns[m.i]
	m.i++
	ch := make(chan Emit, len(turn))
	for _, e := range turn {
		ch <- e
	}
	close(ch)
	return NewStream(ch), nil
}

func toolTurn(id, name, args string) []Emit {
	return []Emit{
		{Event: ToolCallDelta{Index: 0, ID: id, Name: name, ArgsFragment: json.RawMessage(args)}},
		{Event: Finish{Reason: "tool_use"}},
	}
}
func textTurn(s string) []Emit {
	return []Emit{{Event: TextDelta{Text: s}}, {Event: Finish{Reason: "stop"}}}
}
func errTurn(err error) []Emit { return []Emit{{Err: err}} }

// countingTool records how many times it actually executed.
type countingTool struct {
	name   string
	safety Safety
	calls  *int
}

func (t *countingTool) Name() string                { return t.name }
func (t *countingTool) Description() string         { return "" }
func (t *countingTool) Safety() Safety              { return t.safety }
func (t *countingTool) ArgsSchema() json.RawMessage { return nil }
func (t *countingTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	*t.calls++
	return json.RawMessage(`{"ok":true}`), nil
}

// A full run: model asks for a tool, gets the result, then answers.
func TestRun_ToolThenAnswer(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{"q":"x"}`), textTurn("final")}}
	a := New(m, NewMemStore(), tool)

	out, err := a.Run(context.Background(), "run1", "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := textOf(out); got != "final" {
		t.Fatalf("answer = %q, want %q", got, "final")
	}
	if calls != 1 {
		t.Fatalf("tool ran %d times, want 1", calls)
	}
}

// The moat: crash after the tool completed, then resume. The tool must NOT re-run,
// and the run must complete from the journal.
func TestResume_DoesNotRerunCompletedTool(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	store := NewMemStore()

	// Turn 1 asks for the tool; turn 2 "crashes" (model error) after the tool result
	// is already journaled.
	crashy := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{"q":"x"}`), errTurn(errors.New("boom"))}}
	if _, err := New(crashy, store, tool).Run(context.Background(), "run2", "hi"); err == nil {
		t.Fatal("expected crash error on first attempt")
	}
	if calls != 1 {
		t.Fatalf("after crash, tool ran %d times, want 1", calls)
	}

	// Fresh agent (new process), same store: resume and finish.
	recovered := &scriptModel{turns: [][]Emit{textTurn("final")}}
	out, err := New(recovered, store, tool).Run(context.Background(), "run2", "hi")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := textOf(out); got != "final" {
		t.Fatalf("resumed answer = %q, want %q", got, "final")
	}
	if calls != 1 {
		t.Fatalf("tool re-ran on resume: ran %d times, want 1", calls)
	}
}

// Safety: a write tool journaled without a result has unknown outcome → halt.
func TestResume_HaltsOnUnsafeWrite(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()
	// Seed a journal where a WRITE tool was requested, we recorded an attempt marker
	// (started the side effect), but no result was written → crashed mid-write.
	asst := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "charge", Args: json.RawMessage(`{}`)}}}
	_, _ = store.Do(ctx, "run3", "@llm/0", func(context.Context) (Record, error) {
		return Record{Kind: StepModel, Message: &asst}, nil
	})
	_, _ = store.Do(ctx, "run3", toolAttemptStep("c1"), func(context.Context) (Record, error) {
		return Record{Kind: StepAttempt, ToolUseID: "c1"}, nil
	})

	var calls int
	write := &countingTool{name: "charge", safety: Safety{}, calls: &calls} // not ReadOnly, not Idempotent
	_, err := New(&scriptModel{}, store, write).Run(ctx, "run3", "hi")

	var halt *ResumeHalt
	if !errors.As(err, &halt) {
		t.Fatalf("err = %v, want *ResumeHalt", err)
	}
	if calls != 0 {
		t.Fatalf("unsafe write tool ran %d times on resume, want 0", calls)
	}
}

// The Option-B primitive: named durable steps in plain-Go control flow memoize across
// a crash — a completed step is not re-run on resume.
func TestNamedStep_MemoizesAcrossResume(t *testing.T) {
	store := NewMemStore()
	var loads int

	work := func(runID string, crashAfterLoad bool) (string, error) {
		loaded, err := Step(context.Background(), store, runID, "load", func(context.Context) (string, error) {
			loads++
			return "loaded", nil
		})
		if err != nil {
			return "", err
		}
		if crashAfterLoad {
			return "", errors.New("crash before finish")
		}
		return Step(context.Background(), store, runID, "finish", func(context.Context) (string, error) {
			return loaded + "-done", nil
		})
	}

	if _, err := work("w1", true); err == nil {
		t.Fatal("expected crash")
	}
	if loads != 1 {
		t.Fatalf("load ran %d times before crash, want 1", loads)
	}

	out, err := work("w1", false) // resume
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if out != "loaded-done" {
		t.Fatalf("result = %q, want %q", out, "loaded-done")
	}
	if loads != 1 {
		t.Fatalf("load re-ran on resume: %d times, want 1", loads)
	}
}

// A truncated stream leaves tool-call args as invalid JSON — finalize must reject it
// rather than hand malformed args to a tool.
func TestStream_RejectsTruncatedToolArgs(t *testing.T) {
	ch := make(chan Emit, 3)
	ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "c1", Name: "lookup", ArgsFragment: json.RawMessage(`{"q":`)}}
	ch <- Emit{Event: ToolCallDelta{Index: 0, ArgsFragment: json.RawMessage(`"x"`)}} // stream cut off — no closing brace
	ch <- Emit{Event: Finish{Reason: "tool_use"}}
	close(ch)

	if _, _, err := NewStream(ch).Message(); err == nil {
		t.Fatal("expected error on truncated tool-call args")
	}
}

// Fragmented-but-complete args must concatenate by index into valid JSON.
func TestStream_AssemblesFragmentedToolArgs(t *testing.T) {
	ch := make(chan Emit, 3)
	ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "c1", Name: "lookup", ArgsFragment: json.RawMessage(`{"q":`)}}
	ch <- Emit{Event: ToolCallDelta{Index: 0, ArgsFragment: json.RawMessage(`"x"}`)}}
	ch <- Emit{Event: Finish{Reason: "tool_use"}}
	close(ch)

	msg, _, err := NewStream(ch).Message()
	if err != nil {
		t.Fatalf("Message: %v", err)
	}
	tus := msg.toolUses()
	if len(tus) != 1 || string(tus[0].Args) != `{"q":"x"}` {
		t.Fatalf("assembled args = %+v, want one call with {\"q\":\"x\"}", tus)
	}
}

// HITL: a tool requiring approval pauses the run durably (PendingApproval) without
// executing; after Approve, re-running resumes and executes it exactly once.
func TestHITL_PausesForApprovalThenResumes(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()
	var charged int
	charge := &countingTool{name: "charge", safety: Safety{RequiresApproval: true}, calls: &charged}

	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	_, err := New(m, store, charge).Run(ctx, "r1", "pay")

	var pend *PendingApproval
	if !errors.As(err, &pend) {
		t.Fatalf("err = %v, want *PendingApproval", err)
	}
	if pend.ToolName != "charge" || charged != 0 {
		t.Fatalf("paused wrong: charged=%d (want 0), pend=%+v", charged, pend)
	}

	// Human approves; resume (fresh model — the tool turn is already journaled).
	if err := Approve(ctx, store, "r1", pend.ToolUseID, true); err != nil {
		t.Fatal(err)
	}
	m2 := &scriptModel{turns: [][]Emit{textTurn("done")}}
	out, err := New(m2, store, charge).Run(ctx, "r1", "pay")
	if err != nil {
		t.Fatalf("resume after approval: %v", err)
	}
	if textOf(out) != "done" || charged != 1 {
		t.Fatalf("out=%q charged=%d, want done/1", textOf(out), charged)
	}
}

// Messages must round-trip through JSON (with typed Parts) so any durable store can
// persist and reload them.
func TestMessage_JSONRoundTrip(t *testing.T) {
	in := Message{Role: RoleAssistant, Parts: []Part{
		Reasoning{Text: "think", Signature: "sig"},
		Text{Text: "hello"},
		ToolUse{ID: "c1", Name: "lookup", Args: json.RawMessage(`{"q":"x"}`)},
	}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Message
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Role != RoleAssistant || len(out.Parts) != 3 {
		t.Fatalf("round-trip shape: %+v", out)
	}
	if r, ok := out.Parts[0].(Reasoning); !ok || r.Signature != "sig" {
		t.Errorf("reasoning did not round-trip with signature: %+v", out.Parts[0])
	}
	if tu, ok := out.Parts[2].(ToolUse); !ok || string(tu.Args) != `{"q":"x"}` {
		t.Errorf("tool_use did not round-trip: %+v", out.Parts[2])
	}
}

// The derived graph: after a run, RenderMermaid reflects what actually executed.
func TestRenderMermaid_ReflectsExecution(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	store := NewMemStore()
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{"q":"x"}`), textTurn("final")}}
	if _, err := New(m, store, tool).Run(context.Background(), "g1", "hi"); err != nil {
		t.Fatal(err)
	}

	mermaid, err := RenderMermaid(context.Background(), store, "g1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"flowchart TD", "LLM", "tool: lookup", "done"} {
		if !strings.Contains(mermaid, want) {
			t.Errorf("mermaid missing %q:\n%s", want, mermaid)
		}
	}
}

// Landmark 1: a sub-agent runs as a durable tool; its sub-run journals under a
// hierarchical ID (parentRunID/toolUseID) in the shared store — a resumable tree.
func TestSubAgent_DurableTree(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()

	sub := New(&scriptModel{turns: [][]Emit{textTurn("sub-answer")}}, store)
	researcher := SubAgent("researcher", "researches things", sub)

	parent := New(&scriptModel{turns: [][]Emit{
		toolTurn("c1", "researcher", `{"task":"find x"}`),
		textTurn("parent-final"),
	}}, store, researcher)

	out, err := parent.Run(ctx, "root", "do it")
	if err != nil {
		t.Fatal(err)
	}
	if textOf(out) != "parent-final" {
		t.Fatalf("parent answer = %q", textOf(out))
	}

	// The sub-agent journaled under the hierarchical run ID — the tree is durable.
	subHist, err := store.History(ctx, SubRunID("root", "c1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(subHist) == 0 {
		t.Fatal("sub-run has no journal under root/c1 — tree not durable")
	}
	var sawSubModel bool
	for _, r := range subHist {
		if r.Kind == StepModel {
			sawSubModel = true
		}
	}
	if !sawSubModel {
		t.Errorf("sub-run journal missing model step: %+v", subHist)
	}
}

// Landmark 2: record a run, then replay its model outputs against a fresh store —
// deterministic re-execution with no live model.
func TestReplay_ReExecutesFromJournal(t *testing.T) {
	ctx := context.Background()

	// Record.
	rec := NewMemStore()
	var calls1 int
	tool1 := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls1}
	recModel := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{"q":"x"}`), textTurn("recorded-final")}}
	if _, err := New(recModel, rec, tool1).Run(ctx, "orig", "go"); err != nil {
		t.Fatal(err)
	}

	// Replay the recorded model outputs against a FRESH store — no live model involved.
	rm, err := Replay(ctx, rec, "orig")
	if err != nil {
		t.Fatal(err)
	}
	var calls2 int
	tool2 := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls2}
	out, err := New(rm, NewMemStore(), tool2).Run(ctx, "replay", "go")
	if err != nil {
		t.Fatalf("replay run: %v", err)
	}
	if textOf(out) != "recorded-final" {
		t.Fatalf("replayed answer = %q, want recorded-final", textOf(out))
	}
	if calls2 != 1 {
		t.Fatalf("tool executed %d times on replay, want 1 (deterministic re-execution)", calls2)
	}
}

// The Saga Agent: charge + book flight succeed, hotel fails → the completed writes are
// automatically compensated in reverse (refund + cancel). No other framework does this.
func TestSaga_CompensatesCompletedWritesOnAbort(t *testing.T) {
	ctx := context.Background()
	var charged, booked bool

	charge := CompensatedFunc("charge_card", "charge the customer", Safety{},
		func(context.Context, struct{}) (struct{}, error) { charged = true; return struct{}{}, nil },
		func(context.Context, struct{}, struct{}) error { charged = false; return nil }) // refund

	bookFlight := CompensatedFunc("book_flight", "book the flight", Safety{},
		func(context.Context, struct{}) (struct{}, error) { booked = true; return struct{}{}, nil },
		func(context.Context, struct{}, struct{}) error { booked = false; return nil }) // cancel

	bookHotel := Func("book_hotel", "book the hotel", Safety{},
		func(context.Context, struct{}) (struct{}, error) { return struct{}{}, errors.New("no rooms") })

	model := &scriptModel{turns: [][]Emit{
		toolTurn("c1", "charge_card", `{}`),
		toolTurn("c2", "book_flight", `{}`),
		toolTurn("c3", "book_hotel", `{}`),
		textTurn("booked!"), // never reached
	}}

	a := New(model, NewMemStore(), charge, bookFlight, bookHotel)
	_, err := a.RunSaga(ctx, "trip-1", "book my trip")

	var aborted *SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	if aborted.CompensateErr != nil {
		t.Fatalf("rollback incomplete: %v", aborted.CompensateErr)
	}
	// Compensated in REVERSE execution order: flight first, then card.
	if len(aborted.Compensated) != 2 || aborted.Compensated[0] != "book_flight" || aborted.Compensated[1] != "charge_card" {
		t.Fatalf("compensated = %v, want [book_flight charge_card]", aborted.Compensated)
	}
	// The side effects were actually undone.
	if charged || booked {
		t.Fatalf("state not rolled back: charged=%v booked=%v", charged, booked)
	}
}

// Edge case: a compensator itself fails mid-rollback → the transaction is left partially
// rolled back and SagaAborted.CompensateErr flags it (earlier writes remain dangling).
func TestSaga_CompensatorFailureIsFlagged(t *testing.T) {
	ctx := context.Background()
	var charged = true

	charge := CompensatedFunc("charge_card", "", Safety{},
		func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
		func(context.Context, struct{}, struct{}) error { charged = false; return nil })

	// This write's compensator FAILS (e.g. the airline API is down).
	bookFlight := CompensatedFunc("book_flight", "", Safety{},
		func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
		func(context.Context, struct{}, struct{}) error { return errors.New("cancel API down") })

	bookHotel := Func("book_hotel", "", Safety{},
		func(context.Context, struct{}) (struct{}, error) { return struct{}{}, errors.New("no rooms") })

	model := &scriptModel{turns: [][]Emit{
		toolTurn("c1", "charge_card", `{}`),
		toolTurn("c2", "book_flight", `{}`),
		toolTurn("c3", "book_hotel", `{}`),
	}}

	a := New(model, NewMemStore(), charge, bookFlight, bookHotel)
	_, err := a.RunSaga(ctx, "trip-2", "book")

	var aborted *SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	if aborted.CompensateErr == nil {
		t.Fatal("expected CompensateErr to flag the incomplete rollback")
	}
	// Rollback stopped at book_flight (reverse order), so charge_card was never reached —
	// the card is still charged and the operator must intervene.
	if !charged {
		t.Fatal("charge should remain (rollback halted before reaching it)")
	}
}

// Distributed saga (fix #6): a parent saga delegates a charge to a SUB-agent, then a
// later parent step fails — rollback must recurse into the sub-run and undo its charge.
func TestSaga_DistributedRollbackAcrossSubAgent(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	var subCharged bool

	subCharge := CompensatedFunc("charge_card", "", Safety{},
		func(context.Context, struct{}) (struct{}, error) { subCharged = true; return struct{}{}, nil },
		func(context.Context, struct{}, struct{}) error { subCharged = false; return nil })
	payAgent := New(&scriptModel{turns: [][]Emit{toolTurn("s1", "charge_card", `{}`), textTurn("charged")}}, store, subCharge)
	payment := SubAgent("payment", "handles payment", payAgent)

	bookHotel := Func("book_hotel", "", Safety{},
		func(context.Context, struct{}) (struct{}, error) { return struct{}{}, errors.New("no rooms") })

	parent := New(&scriptModel{turns: [][]Emit{
		toolTurn("c1", "payment", `{"task":"charge the customer"}`),
		toolTurn("c2", "book_hotel", `{}`),
	}}, store, payment, bookHotel)

	_, err := parent.RunSaga(ctx, "trip", "book")
	var aborted *SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	if subCharged {
		t.Fatal("sub-agent charge NOT rolled back — distributed saga failed")
	}
	found := false
	for _, c := range aborted.Compensated {
		if c == "charge_card" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Compensated = %v, want it to include charge_card (from the sub-run)", aborted.Compensated)
	}
}

// The full distributed-saga capstone: a failure INSIDE a sub-agent propagates up and
// reverses the WHOLE tree — the sub-agent's own writes and the parent's writes both undo.
func TestSaga_SubAgentFailureReversesWholeTree(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	var parentCharged, seatReserved bool

	// Parent write: charge the customer (succeeds).
	chargeParent := CompensatedFunc("charge_customer", "", Safety{},
		func(context.Context, struct{}) (struct{}, error) { parentCharged = true; return struct{}{}, nil },
		func(context.Context, struct{}, struct{}) error { parentCharged = false; return nil })

	// Sub-agent (booking): reserves a seat (succeeds), then hits a failing step.
	reserveSeat := CompensatedFunc("reserve_seat", "", Safety{},
		func(context.Context, struct{}) (struct{}, error) { seatReserved = true; return struct{}{}, nil },
		func(context.Context, struct{}, struct{}) error { seatReserved = false; return nil })
	failStep := Func("confirm_booking", "", Safety{},
		func(context.Context, struct{}) (struct{}, error) { return struct{}{}, errors.New("carrier rejected") })
	bookingAgent := New(&scriptModel{turns: [][]Emit{
		toolTurn("s1", "reserve_seat", `{}`),
		toolTurn("s2", "confirm_booking", `{}`),
	}}, store, reserveSeat, failStep)
	booking := SubAgent("booking", "books travel", bookingAgent)

	parent := New(&scriptModel{turns: [][]Emit{
		toolTurn("c1", "charge_customer", `{}`),
		toolTurn("c2", "booking", `{"task":"book the trip"}`),
	}}, store, chargeParent, booking)

	_, err := parent.RunSaga(ctx, "trip", "charge then book")
	var aborted *SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	if parentCharged {
		t.Error("parent charge not reversed — tree rollback failed")
	}
	if seatReserved {
		t.Error("sub-agent seat reservation not reversed — sub-run self-rollback failed")
	}
}

// Regression (sub-agent halt/approval propagation): a *ResumeHalt or *PendingApproval
// raised INSIDE a sub-agent must propagate up through a non-saga parent Run — the parent
// must surface it (errors.As matches), must NOT mark the run complete, and after the
// operator resolves it (ResolveHalt / Approve) a re-Run must complete. Before the fix the
// parent loop only special-cased *Interrupted/*Sleeping/*Awaiting, so a sub-agent halt or
// approval fell through to an errored tool-result and the parent ran to completion,
// permanently burying the signal.
func TestSubAgent_PropagatesHaltAndApproval(t *testing.T) {
	ctx := context.Background()

	t.Run("resume-halt", func(t *testing.T) {
		store := NewMemStore()
		var charged int
		// Non-retriable write tool inside the sub-agent.
		charge := &countingTool{name: "charge", safety: Safety{}, calls: &charged}
		sub := New(&scriptModel{turns: [][]Emit{
			toolTurn("s1", "charge", `{}`),
			textTurn("sub-done"),
		}}, store, charge)
		worker := SubAgent("worker", "does work", sub)
		parent := New(&scriptModel{turns: [][]Emit{
			toolTurn("c1", "worker", `{"task":"charge it"}`),
			textTurn("parent-done"),
		}}, store, worker)

		// Seed the sub-run's journal so its charge is ATTEMPTED (side effect started) but has
		// no recorded result — a crash mid-write. On resume the sub-agent must halt.
		subRunID := SubRunID("root", "c1")
		asst := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "s1", Name: "charge", Args: json.RawMessage(`{}`)}}}
		if _, err := store.Do(ctx, subRunID, "@llm/0", func(context.Context) (Record, error) {
			return Record{Kind: StepModel, Message: &asst}, nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Do(ctx, subRunID, toolAttemptStep("s1"), func(context.Context) (Record, error) {
			return Record{Kind: StepAttempt, ToolUseID: "s1"}, nil
		}); err != nil {
			t.Fatal(err)
		}

		_, err := parent.Run(ctx, "root", "delegate")
		var halt *ResumeHalt
		if !errors.As(err, &halt) {
			t.Fatalf("parent Run err = %v, want *ResumeHalt propagated from sub-agent", err)
		}
		if halt.RunID != subRunID || halt.Op.ToolName != "charge" || halt.Op.ID != "s1" {
			t.Fatalf("halt = %+v, want sub-run charge/s1", halt)
		}
		if charged != 0 {
			t.Fatalf("halted charge ran %d times, want 0", charged)
		}
		if complete, _ := IsComplete(ctx, store, "root"); complete {
			t.Fatal("parent run marked complete despite a buried sub-agent halt")
		}

		// Operator confirms the outcome out of band and resolves the halt in the SUB-run.
		if err := ResolveHalt(ctx, store, subRunID, "s1", "charged (confirmed)", false); err != nil {
			t.Fatal(err)
		}
		out, err := parent.Run(ctx, "root", "delegate")
		if err != nil {
			t.Fatalf("re-run after ResolveHalt: %v", err)
		}
		if textOf(out) != "parent-done" {
			t.Fatalf("resumed parent answer = %q, want parent-done", textOf(out))
		}
		if complete, _ := IsComplete(ctx, store, "root"); !complete {
			t.Fatal("parent run not marked complete after resolving the halt")
		}
	})

	t.Run("pending-approval", func(t *testing.T) {
		store := NewMemStore()
		var charged int
		approve := &countingTool{name: "charge", safety: Safety{RequiresApproval: true}, calls: &charged}
		sub := New(&scriptModel{turns: [][]Emit{
			toolTurn("s1", "charge", `{}`),
			textTurn("sub-done"),
		}}, store, approve)
		worker := SubAgent("worker", "does work", sub)
		parent := New(&scriptModel{turns: [][]Emit{
			toolTurn("c1", "worker", `{"task":"charge it"}`),
			textTurn("parent-done"),
		}}, store, worker)

		_, err := parent.Run(ctx, "root", "delegate")
		var pend *PendingApproval
		if !errors.As(err, &pend) {
			t.Fatalf("parent Run err = %v, want *PendingApproval propagated from sub-agent", err)
		}
		if pend.ToolName != "charge" || pend.ToolUseID != "s1" {
			t.Fatalf("pend = %+v, want sub-run charge/s1", pend)
		}
		if charged != 0 {
			t.Fatalf("approval-gated charge ran %d times before approval, want 0", charged)
		}
		if complete, _ := IsComplete(ctx, store, "root"); complete {
			t.Fatal("parent run marked complete despite a buried sub-agent approval pause")
		}

		// Human approves the SUB-run's tool, then re-run the parent.
		if err := Approve(ctx, store, pend.RunID, pend.ToolUseID, true); err != nil {
			t.Fatal(err)
		}
		out, err := parent.Run(ctx, "root", "delegate")
		if err != nil {
			t.Fatalf("re-run after Approve: %v", err)
		}
		if textOf(out) != "parent-done" {
			t.Fatalf("resumed parent answer = %q, want parent-done", textOf(out))
		}
		if charged != 1 {
			t.Fatalf("approved charge ran %d times, want 1", charged)
		}
		if complete, _ := IsComplete(ctx, store, "root"); !complete {
			t.Fatal("parent run not marked complete after approval")
		}
	})
}

func textOf(m Message) string {
	for _, p := range m.Parts {
		if t, ok := p.(Text); ok {
			return t.Text
		}
	}
	return ""
}
