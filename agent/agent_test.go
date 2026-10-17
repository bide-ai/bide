package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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
	name     string
	safety   Safety
	approval *ApprovalPolicy
	calls    *int
}

func (t *countingTool) Spec() ToolSpec {
	return ToolSpec{Name: t.name, Input: json.RawMessage(`{"type":"object"}`), Safety: t.safety, Approval: t.approval}
}

func (t *countingTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	*t.calls++
	return json.RawMessage(`{"ok":true}`), nil
}

// A full run: model asks for a tool, gets the result, then answers.
func TestRun_ToolThenAnswer(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{"q":"x"}`), textTurn("final")}}
	a := mustNew(m, memJournal(), WithTools(tool))

	res, err := a.Run(context.Background(), "run1", UserText("hi"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := res.Message
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
	store := memJournal()

	// Turn 1 asks for the tool; turn 2 "crashes" (model error) after the tool result
	// is already journaled.
	crashy := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{"q":"x"}`), errTurn(errors.New("boom"))}}
	if _, err := mustNew(crashy, store, WithTools(tool)).Run(context.Background(), "run2", UserText("hi")); err == nil {
		t.Fatal("expected crash error on first attempt")
	}
	if calls != 1 {
		t.Fatalf("after crash, tool ran %d times, want 1", calls)
	}

	// Fresh agent (new process), same store: resume and finish.
	recovered := &scriptModel{turns: [][]Emit{textTurn("final")}}
	res, err := mustNew(recovered, store, WithTools(tool)).Run(context.Background(), "run2", UserText("hi"))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	out := res.Message
	if got := textOf(out); got != "final" {
		t.Fatalf("resumed answer = %q, want %q", got, "final")
	}
	if calls != 1 {
		t.Fatalf("tool re-ran on resume: ran %d times, want 1", calls)
	}
}

// Safety: a write tool journaled without a result has unknown outcome → halt.
func TestResume_HaltsOnUnsafeWrite(t *testing.T) {
	store := memJournal()
	ctx := context.Background()
	// Seed a journal where a WRITE tool was requested, we recorded an attempt marker
	// (started the side effect), but no result was written → crashed mid-write.
	asst := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "charge", Args: json.RawMessage(`{}`)}}}
	_, _ = store.do(ctx, "run3", "@llm/0", func(context.Context) (Record, error) {
		return Record{Kind: StepModel, Message: &asst}, nil
	})
	_, _ = store.do(ctx, "run3", toolAttemptStep("c1"), func(context.Context) (Record, error) {
		return Record{Kind: StepAttempt, ToolUseID: "c1"}, nil
	})

	var calls int
	write := &countingTool{name: "charge", safety: Safety{}, calls: &calls} // not ReadOnly, not Idempotent
	_, err := mustNew(&scriptModel{}, store, WithTools(write)).Run(ctx, "run3", UserText("hi"))

	var halt *OutcomeUnknown
	if !errors.As(err, &halt) {
		t.Fatalf("err = %v, want *OutcomeUnknown", err)
	}
	if calls != 0 {
		t.Fatalf("unsafe write tool ran %d times on resume, want 0", calls)
	}
}

// The Option-B primitive: named durable steps in plain-Go control flow memoize across
// a crash — a completed step is not re-run on resume.
func TestNamedStep_MemoizesAcrossResume(t *testing.T) {
	store := memJournal()
	var loads int

	work := func(runID string, crashAfterLoad bool) (string, error) {
		loaded, err := store.Step(context.Background(), runID, "load", func(context.Context) (string, error) {
			loads++
			return "loaded", nil
		})
		if err != nil {
			return "", err
		}
		if crashAfterLoad {
			return "", errors.New("crash before finish")
		}
		return store.Step(context.Background(), runID, "finish", func(context.Context) (string, error) {
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

// HITL: a tool requiring approval pauses the run durably (ApprovalPending) without
// executing; after Approve, re-running resumes and executes it exactly once.
func TestHITL_PausesForApprovalThenResumes(t *testing.T) {
	store := memJournal()
	ctx := context.Background()
	var charged int
	charge := &countingTool{name: "charge", approval: SingleApproval(), calls: &charged}

	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	_, err := mustNew(m, store, WithTools(charge)).Run(ctx, "r1", UserText("pay"))

	var pend *ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("err = %v, want *ApprovalPending", err)
	}
	if pend.ToolName != "charge" || charged != 0 {
		t.Fatalf("paused wrong: charged=%d (want 0), pend=%+v", charged, pend)
	}

	// Human approves; resume (fresh model — the tool turn is already journaled).
	if err := Approve(ctx, store, "r1", pend.ToolUseID, true); err != nil {
		t.Fatal(err)
	}
	m2 := &scriptModel{turns: [][]Emit{textTurn("done")}}
	res, err := mustNew(m2, store, WithTools(charge)).Run(ctx, "r1", UserText("pay"))
	if err != nil {
		t.Fatalf("resume after approval: %v", err)
	}
	out := res.Message
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
	store := memJournal()
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{"q":"x"}`), textTurn("final")}}
	if _, err := mustNew(m, store, WithTools(tool)).Run(context.Background(), "g1", UserText("hi")); err != nil {
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
	store := memJournal()
	ctx := context.Background()

	sub := mustNew(&scriptModel{turns: [][]Emit{textTurn("sub-answer")}}, store)
	researcher := MustSubAgent("researcher", "researches things", sub)

	parent := mustNew(
		&scriptModel{turns: [][]Emit{
			toolTurn("c1", "researcher", `{"task":"find x"}`),
			textTurn("parent-final"),
		}},
		store,
		WithTools(researcher),
	)

	res, err := parent.Run(ctx, "root", UserText("do it"))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Message
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
	rec := memJournal()
	var calls1 int
	tool1 := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls1}
	recModel := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{"q":"x"}`), textTurn("recorded-final")}}
	if _, err := mustNew(recModel, rec, WithTools(tool1)).Run(ctx, "orig", UserText("go")); err != nil {
		t.Fatal(err)
	}

	// Replay the recorded model outputs against a FRESH store — no live model involved.
	rm, err := Replay(ctx, rec, "orig")
	if err != nil {
		t.Fatal(err)
	}
	var calls2 int
	tool2 := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls2}
	res, err := mustNew(rm, memJournal(), WithTools(tool2)).Run(ctx, "replay", UserText("go"))
	if err != nil {
		t.Fatalf("replay run: %v", err)
	}
	out := res.Message
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

	charge := MustCompensatedFunc("charge_card", "charge the customer", func(context.Context, struct{}) (struct{}, error) { charged = true; return struct{}{}, nil },
		func(context.Context, struct{}, struct{}) error { charged = false; return nil }) // refund

	bookFlight := MustCompensatedFunc("book_flight", "book the flight", func(context.Context, struct{}) (struct{}, error) { booked = true; return struct{}{}, nil },
		func(context.Context, struct{}, struct{}) error { booked = false; return nil }) // cancel

	bookHotel := MustFunc("book_hotel", "book the hotel", func(context.Context, struct{}) (struct{}, error) { return struct{}{}, errors.New("no rooms") })

	model := &scriptModel{turns: [][]Emit{
		toolTurn("c1", "charge_card", `{}`),
		toolTurn("c2", "book_flight", `{}`),
		toolTurn("c3", "book_hotel", `{}`),
		textTurn("booked!"), // never reached
	}}

	a := mustNew(model, memJournal(), WithTools(charge, bookFlight, bookHotel))
	_, err := a.Run(ctx, "trip-1", UserText("book my trip"), WithSaga())

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

	charge := MustCompensatedFunc("charge_card", "", func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
		func(context.Context, struct{}, struct{}) error { charged = false; return nil })

	// This write's compensator FAILS (e.g. the airline API is down).
	bookFlight := MustCompensatedFunc("book_flight", "", func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
		func(context.Context, struct{}, struct{}) error { return errors.New("cancel API down") })

	bookHotel := MustFunc("book_hotel", "", func(context.Context, struct{}) (struct{}, error) { return struct{}{}, errors.New("no rooms") })

	model := &scriptModel{turns: [][]Emit{
		toolTurn("c1", "charge_card", `{}`),
		toolTurn("c2", "book_flight", `{}`),
		toolTurn("c3", "book_hotel", `{}`),
	}}

	a := mustNew(model, memJournal(), WithTools(charge, bookFlight, bookHotel))
	_, err := a.Run(ctx, "trip-2", UserText("book"), WithSaga())

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
	store := memJournal()
	var subCharged bool

	subCharge := MustCompensatedFunc("charge_card", "", func(context.Context, struct{}) (struct{}, error) { subCharged = true; return struct{}{}, nil },
		func(context.Context, struct{}, struct{}) error { subCharged = false; return nil })
	payAgent := mustNew(
		&scriptModel{turns: [][]Emit{toolTurn("s1", "charge_card", `{}`), textTurn("charged")}},
		store,
		WithTools(subCharge),
	)
	payment := MustSubAgent("payment", "handles payment", payAgent)

	bookHotel := MustFunc("book_hotel", "", func(context.Context, struct{}) (struct{}, error) { return struct{}{}, errors.New("no rooms") })

	parent := mustNew(
		&scriptModel{turns: [][]Emit{
			toolTurn("c1", "payment", `{"task":"charge the customer"}`),
			toolTurn("c2", "book_hotel", `{}`),
		}},
		store,
		WithTools(payment, bookHotel),
	)

	_, err := parent.Run(ctx, "trip", UserText("book"), WithSaga())
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
	store := memJournal()
	var parentCharged, seatReserved bool

	// Parent write: charge the customer (succeeds).
	chargeParent := MustCompensatedFunc("charge_customer", "", func(context.Context, struct{}) (struct{}, error) { parentCharged = true; return struct{}{}, nil },
		func(context.Context, struct{}, struct{}) error { parentCharged = false; return nil })

	// Sub-agent (booking): reserves a seat (succeeds), then hits a failing step.
	reserveSeat := MustCompensatedFunc("reserve_seat", "", func(context.Context, struct{}) (struct{}, error) { seatReserved = true; return struct{}{}, nil },
		func(context.Context, struct{}, struct{}) error { seatReserved = false; return nil })
	failStep := MustFunc("confirm_booking", "", func(context.Context, struct{}) (struct{}, error) { return struct{}{}, errors.New("carrier rejected") })
	bookingAgent := mustNew(
		&scriptModel{turns: [][]Emit{
			toolTurn("s1", "reserve_seat", `{}`),
			toolTurn("s2", "confirm_booking", `{}`),
		}},
		store,
		WithTools(reserveSeat, failStep),
	)
	booking := MustSubAgent("booking", "books travel", bookingAgent)

	parent := mustNew(
		&scriptModel{turns: [][]Emit{
			toolTurn("c1", "charge_customer", `{}`),
			toolTurn("c2", "booking", `{"task":"book the trip"}`),
		}},
		store,
		WithTools(chargeParent, booking),
	)

	_, err := parent.Run(ctx, "trip", UserText("charge then book"), WithSaga())
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

// Regression (sub-agent halt/approval propagation): a *OutcomeUnknown or *ApprovalPending
// raised INSIDE a sub-agent must propagate up through a non-saga parent Run — the parent
// must surface it (errors.As matches), must NOT mark the run complete, and after the
// operator resolves it (ResolveHalt / Approve) a re-Run must complete. Before the fix the
// parent loop only special-cased *InterruptPending/*TimerPending/*SignalPending, so a sub-agent halt or
// approval fell through to an errored tool-result and the parent ran to completion,
// permanently burying the signal.
func TestSubAgent_PropagatesHaltAndApproval(t *testing.T) {
	ctx := context.Background()

	t.Run("resume-halt", func(t *testing.T) {
		store := memJournal()
		var charged int
		// Non-retriable write tool inside the sub-agent.
		charge := &countingTool{name: "charge", safety: Safety{}, calls: &charged}
		sub := mustNew(
			&scriptModel{turns: [][]Emit{
				toolTurn("s1", "charge", `{}`),
				textTurn("sub-done"),
			}},
			store,
			WithTools(charge),
		)
		worker := MustSubAgent("worker", "does work", sub)
		parent := mustNew(
			&scriptModel{turns: [][]Emit{
				toolTurn("c1", "worker", `{"task":"charge it"}`),
				textTurn("parent-done"),
			}},
			store,
			WithTools(worker),
		)

		// Seed the sub-run's journal so its charge is ATTEMPTED (side effect started) but has
		// no recorded result — a crash mid-write. On resume the sub-agent must halt.
		subRunID := SubRunID("root", "c1")
		asst := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "s1", Name: "charge", Args: json.RawMessage(`{}`)}}}
		if _, err := store.do(ctx, subRunID, "@llm/0", func(context.Context) (Record, error) {
			return Record{Kind: StepModel, Message: &asst}, nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.do(ctx, subRunID, toolAttemptStep("s1"), func(context.Context) (Record, error) {
			return Record{Kind: StepAttempt, ToolUseID: "s1"}, nil
		}); err != nil {
			t.Fatal(err)
		}

		_, err := parent.Run(ctx, "root", UserText("delegate"))
		var halt *OutcomeUnknown
		if !errors.As(err, &halt) {
			t.Fatalf("parent Run err = %v, want *OutcomeUnknown propagated from sub-agent", err)
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
		if err := ResolveHalt(ctx, store, HaltRef{RunID: subRunID, Op: OpRef{Kind: OpTool, ID: "s1"}, Cause: HaltCrashed}, Outcome{Result: "charged (confirmed)", IsError: false}); err != nil {
			t.Fatal(err)
		}
		res, err := parent.Run(ctx, "root", UserText("delegate"))
		if err != nil {
			t.Fatalf("re-run after ResolveHalt: %v", err)
		}
		out := res.Message
		if textOf(out) != "parent-done" {
			t.Fatalf("resumed parent answer = %q, want parent-done", textOf(out))
		}
		if complete, _ := IsComplete(ctx, store, "root"); !complete {
			t.Fatal("parent run not marked complete after resolving the halt")
		}
	})

	t.Run("pending-approval", func(t *testing.T) {
		store := memJournal()
		var charged int
		approve := &countingTool{name: "charge", approval: SingleApproval(), calls: &charged}
		sub := mustNew(
			&scriptModel{turns: [][]Emit{
				toolTurn("s1", "charge", `{}`),
				textTurn("sub-done"),
			}},
			store,
			WithTools(approve),
		)
		worker := MustSubAgent("worker", "does work", sub)
		parent := mustNew(
			&scriptModel{turns: [][]Emit{
				toolTurn("c1", "worker", `{"task":"charge it"}`),
				textTurn("parent-done"),
			}},
			store,
			WithTools(worker),
		)

		_, err := parent.Run(ctx, "root", UserText("delegate"))
		var pend *ApprovalPending
		if !errors.As(err, &pend) {
			t.Fatalf("parent Run err = %v, want *ApprovalPending propagated from sub-agent", err)
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
		res, err := parent.Run(ctx, "root", UserText("delegate"))
		if err != nil {
			t.Fatalf("re-run after Approve: %v", err)
		}
		out := res.Message
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

// An APIError or RateLimited built with a huge body or message still prints a bounded line: a
// provider that echoes the prompt back must not put megabytes into every log line and trace.
func TestProviderErrors_BoundTheirText(t *testing.T) {
	huge := strings.Repeat("x", 32<<20)
	if n := len((&APIError{StatusCode: 500, Body: huge, Err: ErrModel}).Error()); n > 2*maxErrorBody {
		t.Errorf("APIError.Error() is %d bytes", n)
	}
	if n := len((&APIError{StatusCode: 500, Message: huge, Err: ErrModel}).Error()); n > 2*maxErrorBody {
		t.Errorf("APIError.Error() with a huge message is %d bytes", n)
	}
	if n := len((&RateLimited{Message: huge, Err: ErrModel}).Error()); n > 2*maxErrorBody {
		t.Errorf("RateLimited.Error() is %d bytes", n)
	}
}

// Each condition sentinel wraps its category, so errors.Is matches at both tiers.
func TestErrors_ConditionsWrapCategories(t *testing.T) {
	cases := []struct {
		cond, cat error
	}{
		{ErrUnknownTool, ErrTool},
		{ErrToolArgs, ErrTool},
		{ErrNoRecordedOutput, ErrModel},
		{ErrTruncatedToolArgs, ErrProtocol},
		{ErrBudgetExceeded, ErrBudget},
	}
	for _, c := range cases {
		if !errors.Is(c.cond, c.cat) {
			t.Errorf("errors.Is(%v, %v) = false, want true", c.cond, c.cat)
		}
	}
	// Categories are distinct — no accidental cross-matching.
	if errors.Is(ErrModel, ErrTool) || errors.Is(ErrStorage, ErrModel) {
		t.Fatal("distinct categories must not match each other")
	}
}

// A model calling an unregistered tool yields an error classified as ErrUnknownTool
// (and therefore ErrTool).
func TestErrors_UnknownToolClassified(t *testing.T) {
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "nope", `{}`)}}
	a := mustNew(m, memJournal())

	_, err := a.Run(context.Background(), "r", UserText("hi"))
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("err = %v, want errors.Is ErrUnknownTool", err)
	}
	if !errors.Is(err, ErrTool) {
		t.Fatalf("err = %v, want errors.Is ErrTool (category)", err)
	}
}

// Bad tool arguments classify as ErrToolArgs (and ErrTool), while the underlying JSON
// error stays inspectable in the chain.
func TestErrors_ToolArgsClassified(t *testing.T) {
	tool := MustFunc("adder", "", func(_ context.Context, in struct {
		A int `json:"a"`
	}) (int, error) {
		return in.A, nil
	}, WithSafety(Safety{ReadOnly: true}))

	_, err := tool.Call(context.Background(), json.RawMessage(`{"a":"not-an-int"}`))
	if !errors.Is(err, ErrToolArgs) {
		t.Fatalf("err = %v, want errors.Is ErrToolArgs", err)
	}
	if !errors.Is(err, ErrTool) {
		t.Fatalf("err = %v, want errors.Is ErrTool (category)", err)
	}
}

// A truncated tool-call stream classifies as ErrTruncatedToolArgs (and ErrProtocol).
func TestErrors_TruncatedToolArgsClassified(t *testing.T) {
	ch := make(chan Emit, 3)
	ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "c1", Name: "lookup", ArgsFragment: json.RawMessage(`{"q":`)}}
	ch <- Emit{Event: ToolCallDelta{Index: 0, ArgsFragment: json.RawMessage(`"x"`)}} // no closing brace
	ch <- Emit{Event: Finish{Reason: "tool_use"}}
	close(ch)

	_, _, err := NewStream(ch).Message()
	if !errors.Is(err, ErrTruncatedToolArgs) {
		t.Fatalf("err = %v, want errors.Is ErrTruncatedToolArgs", err)
	}
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want errors.Is ErrProtocol (category)", err)
	}
}

// Control-flow signals stay concrete types matched with errors.As, and are NOT
// swept up by the category sentinels.
func TestErrors_ControlFlowStillTyped(t *testing.T) {
	var calls int
	tool := &countingTool{name: "charge", approval: SingleApproval(), calls: &calls}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	a := mustNew(m, memJournal(), WithTools(tool))

	_, err := a.Run(context.Background(), "r", UserText("pay"))
	var pend *ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("err = %v, want *ApprovalPending", err)
	}
	if errors.Is(err, ErrTool) {
		t.Fatal("an approval pause is not a tool failure")
	}
}

// requestModel answers with a final text and keeps the tool results of the request it was sent.
type requestModel struct{ results []string }

func (m *requestModel) Stream(_ context.Context, req Request) (*Stream, error) {
	for _, msg := range req.Messages {
		for _, p := range msg.Parts {
			if tr, ok := p.(ToolResult); ok {
				m.results = append(m.results, tr.ToolUseID+"="+string(tr.Result))
			}
		}
	}
	ch := make(chan Emit, 2)
	ch <- Emit{Event: TextDelta{Text: "done"}}
	ch <- Emit{Event: Finish{Reason: "stop"}}
	close(ch)
	return NewStream(ch), nil
}

func journal(t *testing.T, store *Journal, runID string, recs ...Record) {
	t.Helper()
	for _, r := range recs {
		if _, err := store.do(context.Background(), runID, r.Name, func(context.Context) (Record, error) { return r, nil }); err != nil {
			t.Fatal(err)
		}
	}
}

func assistantCalls(ids ...string) *Message {
	m := &Message{Role: RoleAssistant}
	for _, id := range ids {
		m.Parts = append(m.Parts, ToolUse{ID: id, Name: "lookup", Args: json.RawMessage(`{}`)})
	}
	return m
}

// A resumed run rebuilds the conversation in call order, whatever order the journal recorded the
// results in, and places each result once, after the first turn that made the call (a journal
// written before tool-use IDs were checked may reuse one). A result no call made is left out.
func TestResume_ConversationFromJournal(t *testing.T) {
	store := memJournal()
	journal(t, store, "r",
		Record{Name: "@llm/0", Kind: StepModel, Message: assistantCalls("c1", "c2")},
		Record{Name: "c2", Kind: StepToolResult, ToolUseID: "c2", Result: json.RawMessage(`2`)},
		Record{Name: "c1", Kind: StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`1`)},
		Record{Name: "stray", Kind: StepToolResult, ToolUseID: "stray", Result: json.RawMessage(`0`)},
		Record{Name: "@llm/1", Kind: StepModel, Message: assistantCalls("c1")},
	)
	var calls int
	m := &requestModel{}
	if _, err := mustNew(
		m,
		store,
		WithTools(&countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}),
	).Run(context.Background(), "r", UserText("go")); err != nil {
		t.Fatal(err)
	}
	if want := []string{"c1=1", "c2=2"}; !reflect.DeepEqual(m.results, want) || calls != 0 {
		t.Fatalf("the model was shown results %v and the tool ran %d times; want %v and 0", m.results, calls, want)
	}
}

// An Image part (both the raw-bytes and URL variants) survives the type-tagged JSON
// round-trip that durable stores rely on.
func TestImagePart_RoundTrips(t *testing.T) {
	raw := []byte{0x89, 0x50, 0x4e, 0x47} // arbitrary bytes
	msg := UserParts(
		Text{"what is this?"},
		ImageData("image/png", raw),
		ImageURL("https://example.com/cat.jpg"),
	)

	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Message
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Role != RoleUser {
		t.Errorf("role = %q, want user", got.Role)
	}
	if len(got.Parts) != 3 {
		t.Fatalf("parts = %d, want 3", len(got.Parts))
	}
	if txt, ok := got.Parts[0].(Text); !ok || txt.Text != "what is this?" {
		t.Errorf("part[0] = %+v, want text", got.Parts[0])
	}
	img, ok := got.Parts[1].(Image)
	if !ok {
		t.Fatalf("part[1] type = %T, want Image", got.Parts[1])
	}
	if img.Mime != "image/png" || !bytes.Equal(img.Data, raw) || img.URL != "" {
		t.Errorf("bytes image round-trip = %+v", img)
	}
	urlImg, ok := got.Parts[2].(Image)
	if !ok {
		t.Fatalf("part[2] type = %T, want Image", got.Parts[2])
	}
	if urlImg.URL != "https://example.com/cat.jpg" || len(urlImg.Data) != 0 {
		t.Errorf("url image round-trip = %+v", urlImg)
	}
}

// toolTurnWithUsage returns a scripted tool-call turn whose Finish event carries known
// usage. Use this in Run tests to assert usage accumulation.
func toolTurnWithUsage(id, name, args string, u Usage) []Emit {
	return []Emit{
		{Event: ToolCallDelta{Index: 0, ID: id, Name: name, ArgsFragment: []byte(args)}},
		{Event: Finish{Reason: "tool_use", Usage: u}},
	}
}

// textTurnWithUsage returns a scripted text-answer turn whose Finish event carries known
// usage.
func textTurnWithUsage(s string, u Usage) []Emit {
	return []Emit{
		{Event: TextDelta{Text: s}},
		{Event: Finish{Reason: "stop", Usage: u}},
	}
}

// TestResult_TwoTurnAccumulatesUsage drives a two-turn run (tool call then final answer)
// and verifies that Result.Usage is the sum of both turns' usages, Result.Turns == 2,
// Result.Duration > 0, and Result.RunID is correct.
func TestResult_TwoTurnAccumulatesUsage(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}

	u1 := Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 2, CacheWriteTokens: 1}
	u2 := Usage{InputTokens: 20, OutputTokens: 8, CacheReadTokens: 0, CacheWriteTokens: 3}

	m := &scriptModel{turns: [][]Emit{
		toolTurnWithUsage("c1", "lookup", `{"q":"x"}`, u1),
		textTurnWithUsage("final answer", u2),
	}}
	a := mustNew(m, memJournal(), WithTools(tool))

	res, err := a.Run(context.Background(), "run-result-1", UserText("hi"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Message must equal the final text.
	if got := textOf(res.Message); got != "final answer" {
		t.Errorf("Message text = %q, want %q", got, "final answer")
	}

	// RunID must echo the argument.
	if res.RunID != "run-result-1" {
		t.Errorf("RunID = %q, want %q", res.RunID, "run-result-1")
	}

	// Turns must be 2 (one tool-call turn, one answer turn).
	if res.Turns != 2 {
		t.Errorf("Turns = %d, want 2", res.Turns)
	}

	// Usage must be the element-wise sum of u1 + u2.
	wantUsage := Usage{
		InputTokens:      u1.InputTokens + u2.InputTokens,
		OutputTokens:     u1.OutputTokens + u2.OutputTokens,
		CacheReadTokens:  u1.CacheReadTokens + u2.CacheReadTokens,
		CacheWriteTokens: u1.CacheWriteTokens + u2.CacheWriteTokens,
	}
	if res.Usage != wantUsage {
		t.Errorf("Usage = %+v, want %+v", res.Usage, wantUsage)
	}

	// Duration is measured wall-clock; a zero-work mock run can legitimately round to 0
	// on coarse-resolution clocks (seen on the Windows runner), so only require non-negative.
	if res.Duration < 0 {
		t.Errorf("Duration = %v, want >= 0", res.Duration)
	}

	// Tool must have executed exactly once.
	if calls != 1 {
		t.Errorf("tool called %d times, want 1", calls)
	}
}

// TestResult_SingleTurnNoTools verifies a simple one-turn (no tool calls) run.
func TestResult_SingleTurnNoTools(t *testing.T) {
	u := Usage{InputTokens: 7, OutputTokens: 4}
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("hello", u)}}
	a := mustNew(m, memJournal())

	res, err := a.Run(context.Background(), "single", UserText("hi"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if textOf(res.Message) != "hello" {
		t.Errorf("Message = %q, want %q", textOf(res.Message), "hello")
	}
	if res.Turns != 1 {
		t.Errorf("Turns = %d, want 1", res.Turns)
	}
	if res.Usage != u {
		t.Errorf("Usage = %+v, want %+v", res.Usage, u)
	}
	if res.RunID != "single" {
		t.Errorf("RunID = %q, want %q", res.RunID, "single")
	}
}

// A developer-chosen step name shares the run's journal with the engine's own keys, so a name
// with an engine prefix is refused. On main a Step named "run:complete" wrote the completion
// marker, and IsComplete reported a run that never finished as complete.
func TestStep_ReservedNameIsRefused(t *testing.T) {
	for _, name := range []string{"run:complete", "run:aborted", "@llm/0", "@saga/compensate/x", "@spend/0",
		"tool:x", "attempt:step:x", "attempt:tool:x", "approval:x", "approval-tally:x", "signal:x",
		"await-timeout:x", "await-resolved:x", "timer:x", "interrupt:x", "chan:1:c:k", "chanack:1:c:k",
		"turn/0", "start/0", "from/x", "audit:policy:x"} {
		store := memJournal()
		ran := false
		_, err := store.Step(context.Background(), "r", name, func(context.Context) (string, error) { ran = true; return "v", nil })
		if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), name) || ran {
			complete, _ := IsComplete(context.Background(), store, "r")
			t.Fatalf("Step %q: err = %v, ran = %v (IsComplete %v); want ErrConfig naming it, not run", name, err, ran, complete)
		}
	}
}

// Parallel refuses a reserved task name before any task runs.
func TestParallel_ReservedTaskNameIsRefused(t *testing.T) {
	var ran int
	task := func(context.Context) (int, error) { ran++; return 1, nil }
	_, err := memJournal().Parallel(context.Background(), "r", []Task[int]{
		{Name: "fine", Fn: task, Safety: Safety{ReadOnly: true}},
		{Name: "@llm/0", Fn: task, Safety: Safety{ReadOnly: true}}})
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "@llm/0") || ran != 0 {
		t.Fatalf("err = %v, %d tasks ran; want ErrConfig naming %q and none run", err, ran, "@llm/0")
	}
}

// A sub-agent's run is "<parent>><call>", so a top-level run ID with a '>' could name one; it is
// refused. A '/' stays allowed ("tenant/123").
func TestRun_RunIDWithSubRunSeparatorIsRefused(t *testing.T) {
	a := mustNew(NewScriptedModel(TextTurn("done")), memJournal())
	for _, run := range []func(string) error{
		func(id string) error { _, err := a.Run(context.Background(), id, UserText("go")); return err },
		func(id string) error {
			_, err := a.Run(context.Background(), id, UserText("go"), WithSaga())
			return err
		},
		func(id string) error { _, err := a.Run(context.Background(), id, UserText("go")); return err },
		func(id string) error {
			_, err := a.Stream(context.Background(), id, UserText("go")).Result()
			return err
		},
	} {
		if err := run("tenant>1"); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "tenant>1") {
			t.Fatalf("err = %v, want ErrConfig naming the run ID", err)
		}
	}
	b := mustNew(NewScriptedModel(TextTurn("done")), memJournal())
	if out, err := answerOf(b.Run(context.Background(), "tenant/123", UserText("go"))); err != nil || out.Text() != "done" {
		t.Fatalf("run tenant/123: %q, %v", out.Text(), err)
	}
}

// A session's run IDs are "<id>>@...", so a session ID with a '>' is refused. (A SendOnce key is
// encoded, so it may hold one; see TestSendOnce_AnyKeyGetsItsOwnTurn.)
func TestSession_IDWithSubRunSeparatorIsRefused(t *testing.T) {
	a := mustNew(NewScriptedModel(TextTurn("done")), memJournal())
	if _, err := a.Session(context.Background(), "chat>1"); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "chat>1") {
		t.Fatalf("Session err = %v, want ErrConfig naming the id", err)
	}
}

// Recover leaves a sub-agent's run to its root: the root's re-run resumes it, and the sub-run
// alone cannot be driven by the root agent's resume callback.
func TestRecover_SkipsSubRuns(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	charge := MustFunc("charge", "charge a card", func(context.Context, struct{}) (string, error) { return "charged", nil }, WithApproval(SingleApproval()))
	root := clerkTree(store, charge)
	var pa *ApprovalPending
	if _, err := root.Run(ctx, "p", UserText("go")); !errors.As(err, &pa) {
		t.Fatalf("run: %v, want a pending approval inside the sub-agent", err)
	}
	var asked []string
	if _, err := Recover(ctx, store, func(ctx context.Context, runID string, _ RunStart) error {
		asked = append(asked, runID)
		_, err := root.Run(ctx, runID, UserText("go"))
		return err
	}); err != nil {
		t.Fatalf("Recover: %v (asked %v)", err, asked)
	}
	if !slices.Equal(asked, []string{"p"}) {
		t.Fatalf("Recover asked to re-drive %v, want only the root run p", asked)
	}
}

// renderLine is every statement RenderMermaid writes. A quoted label holds no '"', which would
// end it.
var renderLine = regexp.MustCompile(`^(flowchart TD|  start\(\[user\]\)|  n\d+\["([^"\n]*)"\]|  (start|n\d+) --> (n\d+|done\(\[done\]\)))$`)

func decodeMermaidEntities(s string) string {
	return regexp.MustCompile(`#(\d+);`).ReplaceAllStringFunc(s, func(m string) string {
		n, _ := strconv.Atoi(m[1 : len(m)-1])
		return string(rune(n))
	})
}

// A tool name is chosen by the model, and RenderMermaid writes it into the diagram. Whatever it
// holds, it stays inside its own label, so a model cannot draw steps the run did not take.
func TestRenderMermaid_ToolNamesCannotAddStatements(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{
		`x"] --> fake[ok] %%`,
		"x\"]\n  fake --> done([done])\n  n9[\"",
		`x #quot; <b>y</b> & z`,
	} {
		store := memJournal()
		msg := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: name, Args: []byte(`{}`)}}}
		if _, err := store.do(ctx, "r", "@llm/0", func(context.Context) (Record, error) {
			return Record{Kind: StepModel, Message: &msg}, nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.do(ctx, "r", "c1", func(context.Context) (Record, error) {
			return Record{Kind: StepToolResult, ToolUseID: "c1", Result: []byte(`1`)}, nil
		}); err != nil {
			t.Fatal(err)
		}
		got, err := RenderMermaid(ctx, store, "r")
		if err != nil {
			t.Fatal(err)
		}
		var labels []string
		for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
			m := renderLine.FindStringSubmatch(line)
			if m == nil {
				t.Errorf("%q: line %q is not a statement RenderMermaid writes; output:\n%s", name, line, got)
				continue
			}
			if m[2] != "" {
				labels = append(labels, decodeMermaidEntities(m[2]))
			}
		}
		if want := []string{"LLM", "tool: " + name}; strings.Join(labels, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("%q: labels %q; want %q", name, labels, want)
		}
	}
}

// The scripted model, as package agenttest has it, for this package's own tests (which cannot import
// agenttest, which imports agent).

// ScriptedModel is a deterministic, LLM-free Model that replays a fixed script of turns,
// one turn per model call. It is the batteries-included version of the model fakes people
// otherwise reverse-engineer from ToolCallDelta + Finish for tests, examples, and local
// dev, so you can drive the whole agent loop offline.
//
// It is replay-safe: it selects the turn to emit by counting the assistant messages
// already in the request, not by an internal cursor. On a durable resume the agent loop
// replays journaled assistant turns without calling the model, so a cursor would drift;
// counting the transcript instead lines the script back up with the live turn every time,
// including after a crash and re-run. Once the script is exhausted it repeats its last
// turn, so an over-long conversation does not panic.
//
// Build turns with ToolTurn, TextTurn, and ErrorTurn:
//
// A ScriptedModel is safe for the sequential agent loop; it is not intended for concurrent
// Stream calls from multiple runs at once.
type ScriptedModel struct {
	turns []ScriptedTurn
}

// ScriptedTurn is one scripted model output. Construct one with ToolTurn, TextTurn, or
// ErrorTurn rather than building it directly.
type ScriptedTurn struct {
	toolID   string
	toolName string
	args     json.RawMessage
	text     string
	err      error
}

// NewScriptedModel builds a ScriptedModel that replays turns in order, one per model call.
func NewScriptedModel(turns ...ScriptedTurn) *ScriptedModel {
	return &ScriptedModel{turns: turns}
}

// ToolTurn scripts a turn in which the model calls one tool: id is the tool-use ID the
// journal keys the call under, name is the tool, and args is its JSON arguments.
func ToolTurn(id, name, args string) ScriptedTurn {
	return ScriptedTurn{toolID: id, toolName: name, args: json.RawMessage(args)}
}

// TextTurn scripts a turn in which the model emits a final text answer, ending the run.
func TextTurn(text string) ScriptedTurn {
	return ScriptedTurn{text: text}
}

// ErrorTurn scripts a turn that fails the model call with err, so a test can exercise the
// loop's error path (a mid-run "crash" before the next step is journaled, say).
func ErrorTurn(err error) ScriptedTurn {
	return ScriptedTurn{err: err}
}

// Stream implements Model. It counts the assistant turns already in the request to pick
// the script entry to emit, so replay after a durable resume lines up with the live turn.
func (m *ScriptedModel) Stream(_ context.Context, req Request) (*Stream, error) {
	idx := 0
	for _, msg := range req.Messages {
		if msg.Role == RoleAssistant {
			idx++
		}
	}
	if idx >= len(m.turns) {
		idx = len(m.turns) - 1 // exhausted: repeat the last turn rather than panic
	}
	t := m.turns[idx]

	if t.err != nil {
		ch := make(chan Emit, 1)
		ch <- Emit{Err: t.err}
		close(ch)
		return NewStream(ch), nil
	}

	ch := make(chan Emit, 2)
	if t.toolName != "" {
		ch <- Emit{Event: ToolCallDelta{Index: 0, ID: t.toolID, Name: t.toolName, ArgsFragment: t.args}}
		ch <- Emit{Event: Finish{Reason: "tool_use"}}
	} else {
		ch <- Emit{Event: TextDelta{Text: t.text}}
		ch <- Emit{Event: Finish{Reason: "stop"}}
	}
	close(ch)
	return NewStream(ch), nil
}

// A newcomer's errors say what to do next: each message below names the call or the choice that
// gets the user unstuck, and its sentinel still matches.
func TestErrorsSayWhatToDoNext(t *testing.T) {
	var charged int
	store, _, _ := finishedCharge(t, &charged)
	charge := &countingTool{name: "charge", safety: Safety{}, calls: &charged}
	_, err := mustNew(&greedyModel{}, store, WithTools(charge)).Run(context.Background(), "r1", UserText("a different input"))
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "new run ID") {
		t.Errorf("finished run, other input: %v; want ErrConfig naming a new run ID", err)
	}

	halt := toolHalt("r1", "r1", "call-1", "charge", time.Time{}, HaltCrashed)
	if msg := halt.Error(); !strings.Contains(msg, "resume halted") || !strings.Contains(msg, "ResolveHalt") {
		t.Errorf("OutcomeUnknown = %q; want it to name ResolveHalt", msg)
	}
	if msg := (&ApprovalPending{RunRef: RunRef{RunID: "r1"}, ToolName: "refund", ToolUseID: "c1"}).Error(); !strings.Contains(msg, "SubmitDecision") {
		t.Errorf("ApprovalPending = %q; want it to name SubmitDecision", msg)
	}
	if _, err := New(nil, nil); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "NewScriptedModel") {
		t.Errorf("New(nil model) = %v; want ErrConfig pointing at the scripted model", err)
	}
}

// loopModel always asks for a tool call, under a new ID each turn (a runaway model), so
// the loop only ends if a turn cap stops it.
type loopModel struct{ n int }

func (m *loopModel) Stream(context.Context, Request) (*Stream, error) {
	m.n++
	ch := make(chan Emit, 2)
	ch <- Emit{Event: ToolCallDelta{Index: 0, ID: fmt.Sprintf("c%d", m.n), Name: "spin", ArgsFragment: []byte(`{}`)}}
	ch <- Emit{Event: Finish{Reason: "tool_use"}}
	close(ch)
	return NewStream(ch), nil
}

// WithMaxTurns stops a runaway tool loop with ErrMaxTurns (category ErrBudget).
func TestMaxTurns_StopsRunawayLoop(t *testing.T) {
	var calls int
	tool := &countingTool{name: "spin", safety: Safety{ReadOnly: true}, calls: &calls}
	a := mustNew(&loopModel{}, memJournal(), WithTools(tool), WithMaxTurns(3))

	_, err := a.Run(context.Background(), "r", UserText("go"))
	if !errors.Is(err, ErrMaxTurns) {
		t.Fatalf("err = %v, want errors.Is ErrMaxTurns", err)
	}
	if !errors.Is(err, ErrBudget) {
		t.Fatalf("err = %v, want errors.Is ErrBudget (category)", err)
	}
	// The cap is on model turns: 3 turns generated, so at most 3 tool executions.
	if calls > 3 {
		t.Fatalf("tool ran %d times, want <= 3 (turn cap)", calls)
	}
}

// A run that finishes within the cap is unaffected.
func TestMaxTurns_UnderLimitCompletes(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{}`), textTurn("done")}}
	a := mustNew(m, memJournal(), WithTools(tool), WithMaxTurns(5))

	res, err := a.Run(context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := res.Message
	if textOf(out) != "done" {
		t.Fatalf("answer = %q", textOf(out))
	}
}

// Default (no cap) is unbounded — WithMaxTurns(0) does not limit.
func TestMaxTurns_ZeroIsUnbounded(t *testing.T) {
	m := &scriptModel{turns: [][]Emit{textTurn("ok")}}
	a := mustNew(m, memJournal(), WithMaxTurns(0))
	if _, err := a.Run(context.Background(), "r", UserText("hi")); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestNew_RefusesNilModelOrJournal(t *testing.T) {
	if a, err := New(nil, memJournal()); !errors.Is(err, ErrConfig) || a != nil {
		t.Errorf("New(nil model) = %v, %v; want nil and ErrConfig", a, err)
	}
	if a, err := New(stubModel{}, nil); !errors.Is(err, ErrConfig) || a != nil {
		t.Errorf("New(nil journal) = %v, %v; want nil and ErrConfig", a, err)
	}
}

func TestEntryPoints_RejectEmptyRunID(t *testing.T) {
	ctx := context.Background()
	s := memJournal()
	a := mustNew(stubModel{}, s)

	if _, err := a.Run(ctx, "", UserText("hi")); !errors.Is(err, ErrConfig) {
		t.Errorf("Run(empty) err = %v, want ErrConfig", err)
	}
	if _, err := a.Run(ctx, "", UserText("hi"), WithSaga()); !errors.Is(err, ErrConfig) {
		t.Errorf("saga Run(empty) err = %v, want ErrConfig", err)
	}
	if _, err := a.Session(ctx, ""); !errors.Is(err, ErrConfig) {
		t.Errorf("Session(empty) err = %v, want ErrConfig", err)
	}
	if err := Approve(ctx, s, "", "tu", true); !errors.Is(err, ErrConfig) {
		t.Errorf("Approve(empty) err = %v, want ErrConfig", err)
	}
	if err := s.AnswerInterrupt(ctx, "", "k", 1); !errors.Is(err, ErrConfig) {
		t.Errorf("Resume(empty) err = %v, want ErrConfig", err)
	}
}

// stubModel is a minimal Model that never gets called (the validation guards fire first).
type stubModel struct{}

func (stubModel) Stream(context.Context, Request) (*Stream, error) { return nil, nil }
