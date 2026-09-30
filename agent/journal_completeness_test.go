package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"
)

// These tests change a live input (the caller's input or entry point, the tool registry, a
// tool's approval gate) between two drives of one run, and check that every decision the second
// drive makes is the one the journal records, not the one the new live value would give.

// userTexts returns the text of every user message in msgs.
func userTexts(msgs []Message) []string {
	var out []string
	for _, m := range msgs {
		if m.Role == RoleUser {
			out = append(out, m.Text())
		}
	}
	return out
}

// A run's earlier model turns answered the input it started with. Resumed with another input,
// the next model call would be sent the new message beside turns that answered the old one: a
// conversation neither input produced. The input is journaled when the run starts, and a resume
// with a different one is refused.
func TestResume_DifferentInputIsRefused(t *testing.T) {
	ctx := context.Background()
	var n int
	gate := &countingTool{name: "refund", safety: Safety{RequiresApproval: true}, calls: &n}
	var got Request
	m := &captureModel{inner: NewScriptedModel(ToolTurn("c1", "refund", `{}`), TextTurn("refunded")), got: &got}
	store := NewMemStore()
	a := New(m, store, gate)

	var pa *PendingApproval
	if _, err := a.Run(ctx, "r", "refund order 17"); !errors.As(err, &pa) {
		t.Fatalf("first drive: err = %v, want *PendingApproval", err)
	}
	if err := Approve(ctx, store, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	got = Request{}
	_, err := a.Run(ctx, "r", "refund order 99")
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("resume with another input: err = %v, want ErrConfig; the model was sent user messages %q", err, userTexts(got.Messages))
	}
	if n != 0 {
		t.Fatalf("refused resume ran the tool %d times", n)
	}

	// The run's own input resumes it, and the model sees that input.
	out, err := a.Run(ctx, "r", "refund order 17")
	if err != nil || textOf(out) != "refunded" {
		t.Fatalf("resume with the recorded input: %q, %v", textOf(out), err)
	}
	if u := userTexts(got.Messages); !slices.Equal(u, []string{"refund order 17"}) {
		t.Fatalf("model was sent user messages %q, want the run's input", u)
	}
}

// SendOnce documents that reusing a key with a different input is ErrConfig. That held only
// for a key whose turn had finished: a paused turn was resumed with the new message, and the
// session recorded the new message as the turn's input beside an answer to the old one.
func TestSession_SendOnceOpenTurnDifferentInputIsRefused(t *testing.T) {
	ctx := context.Background()
	var n int
	gate := &countingTool{name: "refund", safety: Safety{RequiresApproval: true}, calls: &n}
	store := NewMemStore()
	a := New(NewScriptedModel(ToolTurn("c1", "refund", `{}`), TextTurn("refunded")), store, gate)
	s, err := a.Session(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	var pa *PendingApproval
	if _, err := s.SendOnce(ctx, "k1", "refund order 17"); !errors.As(err, &pa) {
		t.Fatalf("first send: err = %v, want *PendingApproval", err)
	}
	if err := Approve(ctx, store, pa.RunID, pa.ToolUseID, true); err != nil {
		t.Fatal(err)
	}
	_, err = s.SendOnce(ctx, "k1", "refund order 99")
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("SendOnce on an open key with another input: err = %v, want ErrConfig; transcript %q", err, userTexts(s.History()))
	}
	if n != 0 {
		t.Fatalf("refused send ran the tool %d times", n)
	}
	if _, err := s.SendOnce(ctx, "k1", "refund order 17"); err != nil {
		t.Fatalf("SendOnce with the key's own input: %v", err)
	}
	if u := userTexts(s.History()); !slices.Equal(u, []string{"refund order 17"}) {
		t.Fatalf("transcript user messages %q, want the key's input", u)
	}
}

// A saga run resumed through Run (a recovery callback that always calls Run, say) lost its
// saga semantics: a failing step was journaled as an ordinary tool error, the model carried
// on, and the run finished with the earlier charge never compensated. The entry point is
// journaled with the input, and a resume through the other one is refused.
func TestResume_SagaRunThroughRunIsRefused(t *testing.T) {
	ctx := context.Background()
	var undone, gated int
	charge := CompensatedFunc("charge", "", Safety{},
		func(context.Context, struct{}) (string, error) { return "ch_1", nil },
		func(context.Context, struct{}, string) error { undone++; return nil })
	gate := &countingTool{name: "gate", safety: Safety{ReadOnly: true, RequiresApproval: true}, calls: &gated}
	book := Func("book", "", Safety{}, func(context.Context, struct{}) (string, error) {
		return "", errors.New("no rooms left")
	})
	store := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), ToolTurn("c2", "gate", `{}`), ToolTurn("c3", "book", `{}`), TextTurn("done"))
	a := New(m, store, charge, gate, book)

	var pa *PendingApproval
	if _, err := a.RunSaga(ctx, "r", "trip"); !errors.As(err, &pa) {
		t.Fatalf("first drive: err = %v, want *PendingApproval", err)
	}
	if err := Approve(ctx, store, "r", "c2", true); err != nil {
		t.Fatal(err)
	}
	out, err := a.Run(ctx, "r", "trip")
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("saga resumed through Run: answer %q, err = %v, want ErrConfig (compensations run: %d)", textOf(out), err, undone)
	}
	var aborted *SagaAborted
	if _, err := a.RunSaga(ctx, "r", "trip"); !errors.As(err, &aborted) || undone != 1 {
		t.Fatalf("saga resumed through RunSaga: err = %v, compensations %d; want *SagaAborted and 1", err, undone)
	}
}

// A run started through Run and resumed through RunSaga would apply rollback to writes made
// under Run's rules; it is refused the same way.
func TestResume_RunThroughRunSagaIsRefused(t *testing.T) {
	ctx := context.Background()
	var gated int
	gate := &countingTool{name: "gate", safety: Safety{ReadOnly: true, RequiresApproval: true}, calls: &gated}
	store := NewMemStore()
	a := New(NewScriptedModel(ToolTurn("c1", "gate", `{}`), TextTurn("done")), store, gate)
	var pa *PendingApproval
	if _, err := a.Run(ctx, "r", "go"); !errors.As(err, &pa) {
		t.Fatalf("first drive: err = %v, want *PendingApproval", err)
	}
	if err := Approve(ctx, store, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RunSaga(ctx, "r", "go"); !errors.Is(err, ErrConfig) {
		t.Fatalf("run resumed through RunSaga: err = %v, want ErrConfig", err)
	}
	if _, err := a.Stream(ctx, "r", "go").Final(); err != nil {
		t.Fatalf("run resumed through Stream: %v", err)
	}
}

// A human's recorded denial is final. If the tool's approval gate is removed before the run is
// driven again, the gate no longer asks, and the denied call ran anyway.
func TestResume_RecordedDenialHoldsAfterGateRemoved(t *testing.T) {
	ctx := context.Background()
	var n int
	store := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "wire", `{"usd":5000}`), TextTurn("ok"))
	gated := &countingTool{name: "wire", safety: Safety{RequiresApproval: true}, calls: &n}
	var pa *PendingApproval
	if _, err := New(m, store, gated).Run(ctx, "r", "pay"); !errors.As(err, &pa) {
		t.Fatalf("first drive: err = %v, want *PendingApproval", err)
	}
	if err := Approve(ctx, store, "r", "c1", false); err != nil {
		t.Fatal(err)
	}
	ungated := &countingTool{name: "wire", safety: Safety{}, calls: &n}
	if _, err := New(m, store, ungated).Run(ctx, "r", "pay"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n != 0 {
		t.Fatalf("a call a human denied ran %d times once its gate was removed", n)
	}
	recs, _ := store.History(ctx, "r")
	if !deniedResult(recs, "c1") {
		t.Fatalf("no denial recorded for c1: %+v", recs)
	}
}

// The same holds for an m-of-n gate whose terminal tally (a denial) was journaled and whose
// denial result was not, because the process died between the two writes: a deploy without the
// gate ran the call.
func TestResume_RecordedTallyDenialHoldsAfterGateRemoved(t *testing.T) {
	ctx := context.Background()
	var n int
	store := &failOnceStore{MemStore: NewMemStore(), name: ToolResultStep("c1")} // the call's result: its denial
	m := NewScriptedModel(ToolTurn("c1", "wire", `{}`), TextTurn("ok"))
	gated := &countingTool{name: "wire", safety: Safety{Approval: &ApprovalPolicy{Need: 2, Approvers: []string{"a", "b"}}}, calls: &n}
	a := New(m, store, gated).WithApproverVerifiers(fakeVerifiers("a", "b"))
	var pa *PendingApproval
	if _, err := a.Run(ctx, "r", "pay"); !errors.As(err, &pa) {
		t.Fatalf("first drive: err = %v, want *PendingApproval", err)
	}
	approveAs(t, store, "r", "c1", "a", false) // 2 of 2 can no longer be reached
	if _, err := a.Run(ctx, "r", "pay"); err == nil {
		t.Fatal("second drive: want the crash writing the denial")
	}
	ungated := &countingTool{name: "wire", safety: Safety{}, calls: &n}
	if _, err := New(m, store, ungated).Run(ctx, "r", "pay"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n != 0 {
		t.Fatalf("a call its quorum denied ran %d times once its gate was removed", n)
	}
	recs, _ := store.History(ctx, "r")
	if !deniedResult(recs, "c1") {
		t.Fatalf("no denial recorded for c1: %+v", recs)
	}
}

// A denial recorded under a 1-of-1 gate holds when the gate becomes m-of-n before the run is
// driven again: the call is denied, not paused for approvers who were never asked.
func TestResume_RecordedDenialHoldsAfterGateTightened(t *testing.T) {
	ctx := context.Background()
	var n int
	store := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "wire", `{}`), TextTurn("ok"))
	var pa *PendingApproval
	if _, err := New(m, store, &countingTool{name: "wire", safety: Safety{RequiresApproval: true}, calls: &n}).Run(ctx, "r", "pay"); !errors.As(err, &pa) {
		t.Fatalf("first drive: err = %v, want *PendingApproval", err)
	}
	if err := Approve(ctx, store, "r", "c1", false); err != nil {
		t.Fatal(err)
	}
	quorum := &countingTool{name: "wire", safety: Safety{Approval: &ApprovalPolicy{Need: 2, Approvers: []string{"a", "b"}}}, calls: &n}
	if _, err := New(m, store, quorum).WithApproverVerifiers(fakeVerifiers("a", "b")).Run(ctx, "r", "pay"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	recs, _ := store.History(ctx, "r")
	if n != 0 || !deniedResult(recs, "c1") {
		t.Fatalf("tool ran %d times, denial recorded %v; want 0 and true", n, deniedResult(recs, "c1"))
	}
}

func deniedResult(recs []Record, id string) bool {
	for _, r := range recs {
		if r.Kind == StepToolResult && r.ToolUseID == id && r.IsError && string(r.Result) == `"tool call denied by human"` {
			return true
		}
	}
	return false
}

// A completed write whose tool is no longer registered when the saga rolls back cannot be
// compensated. It was skipped without a word, so SagaAborted reported a clean rollback while
// the write stood.
func TestSaga_UnregisteredWriteIsReportedUncompensated(t *testing.T) {
	ctx := context.Background()
	var reserved, gated int
	reserve := &countingTool{name: "reserve", safety: Safety{}, calls: &reserved}
	gate := &countingTool{name: "gate", safety: Safety{ReadOnly: true, RequiresApproval: true}, calls: &gated}
	book := Func("book", "", Safety{}, func(context.Context, struct{}) (string, error) {
		return "", errors.New("no rooms left")
	})
	store := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "reserve", `{}`), ToolTurn("c2", "gate", `{}`), ToolTurn("c3", "book", `{}`), TextTurn("done"))
	var pa *PendingApproval
	if _, err := New(m, store, reserve, gate, book).RunSaga(ctx, "r", "trip"); !errors.As(err, &pa) {
		t.Fatalf("first drive: err = %v, want *PendingApproval", err)
	}
	if reserved != 1 {
		t.Fatalf("reserve ran %d times, want 1", reserved)
	}
	if err := Approve(ctx, store, "r", "c2", true); err != nil {
		t.Fatal(err)
	}
	_, err := New(m, store, gate, book).RunSaga(ctx, "r", "trip") // reserve is no longer registered
	var aborted *SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("resume: err = %v, want *SagaAborted", err)
	}
	if aborted.CompensateErr != nil || !slices.Contains(aborted.Uncompensated, "reserve") {
		t.Fatalf("rollback reported compensated %q, uncompensated %q, stopped by %v: want the completed reserve write listed and the rollback finished", aborted.Compensated, aborted.Uncompensated, aborted.CompensateErr)
	}
}

// A call that was attempted as a side effect and has no result may have taken effect: rollback
// stops for a human. Driven again after its tool was unregistered, rollback skipped the call,
// reported a clean rollback, and marked the run aborted, so the halt was lost.
func TestSaga_UnregisteredAttemptedCallStillHalts(t *testing.T) {
	ctx := context.Background()
	// reserve and book run concurrently in one turn; once reserve has been called, book fails,
	// the saga cancels reserve, and reserve's attempt marker stays without a result.
	entered := make(chan struct{})
	reserve := Func("reserve", "", Safety{}, func(ctx context.Context, _ struct{}) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	})
	book := Func("book", "", Safety{}, func(context.Context, struct{}) (string, error) {
		<-entered
		return "", errors.New("no rooms left")
	})
	turn := []Emit{
		{Event: ToolCallDelta{Index: 0, ID: "c1", Name: "reserve", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: ToolCallDelta{Index: 1, ID: "c2", Name: "book", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: Finish{Reason: "tool_use"}},
	}
	store := NewMemStore()
	wantHalt := func(err error) {
		t.Helper()
		var aborted *SagaAborted
		var halt *ResumeHalt
		if !errors.As(err, &aborted) || !errors.As(aborted.CompensateErr, &halt) || halt.ToolUseID != "c1" {
			t.Fatalf("rollback: err = %v, want *SagaAborted stopped by a *ResumeHalt on c1", err)
		}
	}
	_, err := New(&scriptModel{turns: [][]Emit{turn}}, store, reserve, book).RunSaga(ctx, "r", "trip")
	wantHalt(err)
	_, err = New(&scriptModel{}, store, book).RunSaga(ctx, "r", "trip") // reserve is no longer registered
	wantHalt(err)
	if done, _ := hasValueStep(ctx, store, "r", runAbortedStep); done {
		t.Fatal("the run was marked aborted with a call of unknown outcome")
	}
}

// Saga rollback still reads a registered tool's Safety live: a completed write whose tool is
// relabelled ReadOnly before the rollback runs is skipped as if it had changed nothing, and the
// rollback reports clean. Closing this needs the call's safety journaled with its result; that is
// a journal-format decision left to the maintainer (see the PR that added this test), so the test
// is skipped until it is taken.
func TestSaga_RelabelledReadOnlyWriteIsSkipped(t *testing.T) {
	if os.Getenv("BIDE_OPEN_GAPS") == "" {
		t.Skip("open gap: rollback reads a registered tool's Safety live; set BIDE_OPEN_GAPS=1 to run")
	}
	ctx := context.Background()
	var reserved, gated int
	gate := &countingTool{name: "gate", safety: Safety{ReadOnly: true, RequiresApproval: true}, calls: &gated}
	book := Func("book", "", Safety{}, func(context.Context, struct{}) (string, error) {
		return "", errors.New("no rooms left")
	})
	store := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "reserve", `{}`), ToolTurn("c2", "gate", `{}`), ToolTurn("c3", "book", `{}`), TextTurn("done"))
	var pa *PendingApproval
	write := &countingTool{name: "reserve", safety: Safety{}, calls: &reserved}
	if _, err := New(m, store, write, gate, book).RunSaga(ctx, "r", "trip"); !errors.As(err, &pa) {
		t.Fatalf("first drive: err = %v, want *PendingApproval", err)
	}
	if err := Approve(ctx, store, "r", "c2", true); err != nil {
		t.Fatal(err)
	}
	relabelled := &countingTool{name: "reserve", safety: Safety{ReadOnly: true}, calls: &reserved}
	_, err := New(m, store, relabelled, gate, book).RunSaga(ctx, "r", "trip")
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || !slices.Contains(aborted.Uncompensated, "reserve") {
		t.Fatalf("rollback: err = %v; the completed reserve write is not reported", err)
	}
}
