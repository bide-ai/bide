package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// lateTool blocks until its call's context is done, then returns what the test chose: a result,
// an error of its own, or the context's error. A test that gives it a short WithTimeout runs in a
// synctest bubble: on the wall clock the deadline may pass while the call is dispatched, and the
// base handler then refuses the call as not started, so the tool never runs. In a bubble the
// clock advances only once the tool blocks on its context, so the call always reaches it.
func lateTool(name string, safety Safety, calls *atomic.Int32, answer func(ctx context.Context) (string, error), opts ...ToolOption) Tool {
	return MustFunc(name, "", func(ctx context.Context, _ struct{}) (string, error) {
		calls.Add(1)
		<-ctx.Done()
		return answer(ctx)
	}, append([]ToolOption{WithSafety(safety)}, opts...)...)
}

// A call that returns a result after its deadline is recorded, like any other result: its
// outcome is known, so it is never discarded.
func TestTimeout_ResultAfterTheDeadlineIsRecorded(t *testing.T) {
	synctest.Test(t, testResultAfterTheDeadlineIsRecorded)
}

func testResultAfterTheDeadlineIsRecorded(t *testing.T) {
	var calls atomic.Int32
	charge := lateTool("charge", Safety{}, &calls, func(context.Context) (string, error) { return "charged", nil }, WithTimeout(time.Millisecond))
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	if _, err := mustNew(m, store, WithTools(charge)).Run(context.Background(), "r1", UserText("pay")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rec, ok := hasStep(t, store, "r1", ToolResultStep("c1"))
	if !ok || rec.IsError || string(rec.Result) != `"charged"` || calls.Load() != 1 {
		t.Fatalf("result %s (recorded %v, is_error %v), calls %d; want the late result recorded after one call", rec.Result, ok, rec.IsError, calls.Load())
	}
}

// A side effect that returns an error after its deadline may have taken effect before it was cut
// off: nothing is recorded, the run fails with ErrToolOutcomeUnknown, and a resume halts for the
// outcome rather than call the tool again. It holds for the context's own error and for any
// other error the tool returns once the deadline has passed.
func TestTimeout_LateErrorHaltsASideEffect(t *testing.T) {
	for name, answer := range map[string]func(ctx context.Context) (string, error){
		"context error": func(ctx context.Context) (string, error) { return "", ctx.Err() },
		"own error":     func(context.Context) (string, error) { return "", errors.New("gateway: connection reset") },
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls atomic.Int32
				charge := lateTool("charge", Safety{}, &calls, answer, WithTimeout(time.Millisecond))
				store := memJournal()
				m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
				_, err := mustNew(m, store, WithTools(charge)).Run(context.Background(), "r1", UserText("pay"))
				if !errors.Is(err, ErrToolOutcomeUnknown) || !strings.Contains(err.Error(), "timeout") {
					t.Fatalf("Run: err = %v, want ErrToolOutcomeUnknown naming the timeout", err)
				}
				if _, ok := hasStep(t, store, "r1", ToolResultStep("c1")); ok {
					t.Fatal("a result was recorded for a call whose outcome is unknown")
				}
				_, err = mustNew(m, store, WithTools(charge)).Run(context.Background(), "r1", UserText("pay"))
				var halt *OutcomeUnknown
				if !errors.As(err, &halt) || halt.Op.ID != "c1" {
					t.Fatalf("resume: err = %v, want *OutcomeUnknown for c1", err)
				}
				if calls.Load() != 1 {
					t.Fatalf("the side effect was called %d times, want 1", calls.Load())
				}
			})
		})
	}
}

// A retry-safe tool that returns an error after its deadline has its error recorded, as for any
// retry-safe call whose outcome is unknown: running it again is safe, so the model may.
func TestTimeout_LateErrorOfARetrySafeToolIsRecorded(t *testing.T) {
	synctest.Test(t, testLateErrorOfARetrySafeToolIsRecorded)
}

func testLateErrorOfARetrySafeToolIsRecorded(t *testing.T) {
	var calls atomic.Int32
	lookup := lateTool("lookup", Safety{Idempotent: true}, &calls, func(ctx context.Context) (string, error) { return "", ctx.Err() }, WithTimeout(time.Millisecond))
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "lookup", `{}`), TextTurn("done"))
	if _, err := mustNew(m, store, WithTools(lookup)).Run(context.Background(), "r1", UserText("q")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rec, ok := hasStep(t, store, "r1", ToolResultStep("c1"))
	if !ok || !rec.IsError || !strings.Contains(string(rec.Result), "after its 1ms timeout") || calls.Load() != 1 {
		t.Fatalf("result %s (recorded %v, is_error %v), calls %d; want the timeout recorded as the call's error after one call", rec.Result, ok, rec.IsError, calls.Load())
	}
}

// An error returned before the deadline is an ordinary failure, recorded for the model: only an
// error after the deadline has an unknown outcome.
func TestTimeout_ErrorBeforeTheDeadlineIsAFailure(t *testing.T) {
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) {
		return "", errors.New("card declined")
	}, WithTimeout(time.Hour))
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	if _, err := mustNew(m, store, WithTools(charge)).Run(context.Background(), "r1", UserText("pay")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec, ok := hasStep(t, store, "r1", ToolResultStep("c1")); !ok || !rec.IsError || !strings.Contains(string(rec.Result), "declined") {
		t.Fatalf("result %s (recorded %v); want the failure recorded", rec.Result, ok)
	}
}

// The call runs under the deadline: a tool sees it on its context.
func TestTimeout_TheCallSeesTheDeadline(t *testing.T) {
	var left time.Duration
	lookup := MustFunc("lookup", "", func(ctx context.Context, _ struct{}) (string, error) {
		d, ok := ctx.Deadline()
		if !ok {
			return "", errors.New("no deadline")
		}
		left = time.Until(d)
		return "ok", nil
	}, WithSafety(Safety{ReadOnly: true}), WithTimeout(time.Hour))
	m := NewScriptedModel(ToolTurn("c1", "lookup", `{}`), TextTurn("done"))
	if _, err := mustNew(m, memJournal(), WithTools(lookup)).Run(context.Background(), "r1", UserText("q")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if left <= 59*time.Minute || left > time.Hour {
		t.Fatalf("the call's deadline was %s away, want about an hour", left)
	}
}

// Every tool result records the Safety and approval gate its call ran under, as journaled bytes.
func TestToolResult_RecordsSafetyAndApproval(t *testing.T) {
	ctx := context.Background()
	lookup := MustFunc("lookup", "", func(context.Context, struct{}) (string, error) { return "x", nil }, WithSafety(Safety{ReadOnly: true}))
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, WithApproval(SingleApproval()))
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "lookup", `{}`), ToolTurn("c2", "charge", `{}`), TextTurn("done"))
	a := mustNew(m, store, WithTools(lookup, charge))
	if _, err := a.Run(ctx, "r1", UserText("pay")); !IsPause(err) {
		t.Fatalf("first drive: %v, want the approval pause", err)
	}
	if err := Approve(ctx, store, "r1", "c2", true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, "r1", UserText("pay")); err != nil {
		t.Fatalf("resume: %v", err)
	}
	for id, want := range map[string]string{
		"c1": `"safety":{"read_only":true}`,
		"c2": `"safety":{},"approval":{"single":true}`,
	} {
		rec, ok := hasStep(t, store, "r1", ToolResultStep(id))
		if !ok || !strings.Contains(string(rec.Raw()), want) {
			t.Errorf("%s: journaled %s, want it to hold %s", id, rec.Raw(), want)
		}
	}
}

// Rollback reads the safety a call recorded, never its tool's live one, in both directions: a
// write relabelled ReadOnly is still compensated, and a read relabelled a write is still skipped.
func TestRollback_UsesTheRecordedSafety(t *testing.T) {
	var undone int
	undo := func(context.Context, struct{}, string) error { undone++; return nil }
	do := func(context.Context, struct{}) (string, error) { return "ok", nil }
	got := relabelSaga(t, MustCompensatedFunc("hold", "", do, undo), MustCompensatedFunc("hold", "", do, undo, WithSafety(Safety{ReadOnly: true})))
	if undone != 1 || !slices.Contains(got.Compensated, "hold") {
		t.Fatalf("write relabelled ReadOnly: compensations %d, compensated %q; want it compensated once", undone, got.Compensated)
	}
	undone = 0
	got = relabelSaga(t, MustCompensatedFunc("hold", "", do, undo, WithSafety(Safety{ReadOnly: true})), MustCompensatedFunc("hold", "", do, undo))
	if undone != 0 || len(got.Compensated) != 0 || len(got.Uncompensated) != 0 {
		t.Fatalf("read relabelled a write: compensations %d, compensated %q, uncompensated %q; want it skipped", undone, got.Compensated, got.Uncompensated)
	}
}

// SubAgent takes WithApproval: the parent pauses before it delegates, and the sub-agent runs
// only once a human approves.
func TestSubAgent_WithApprovalPauses(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	var subCalls atomic.Int32
	sub := mustNew(&countingModel{n: &subCalls}, store)
	delegate := MustSubAgent("research", "delegate research", sub, WithApproval(SingleApproval()))
	m := NewScriptedModel(ToolTurn("c1", "research", `{"task":"find it"}`), TextTurn("done"))
	parent := mustNew(m, store, WithTools(delegate))
	_, err := parent.Run(ctx, "r1", UserText("go"))
	var pa *ApprovalPending
	if !errors.As(err, &pa) || pa.ToolName != "research" || pa.ToolUseID != "c1" {
		t.Fatalf("Run: err = %v, want *ApprovalPending for the delegation", err)
	}
	if subCalls.Load() != 0 {
		t.Fatalf("the sub-agent ran %d model calls before approval", subCalls.Load())
	}
	if err := Approve(ctx, store, "r1", "c1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := parent.Run(ctx, "r1", UserText("go")); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if subCalls.Load() != 1 {
		t.Fatalf("the sub-agent ran %d model calls after approval, want 1", subCalls.Load())
	}
}

// countingModel answers every request with text and counts the requests.
type countingModel struct{ n *atomic.Int32 }

func (m *countingModel) Stream(context.Context, Request) (*Stream, error) {
	m.n.Add(1)
	ch := make(chan Emit, 2)
	ch <- Emit{Event: TextDelta{Text: "sub answer"}}
	ch <- Emit{Event: Finish{Reason: FinishStop}}
	close(ch)
	return NewStream(ch), nil
}

// panicsWith runs f and returns the error it panicked with, or nil.
func panicsWith(f func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err, _ = r.(error)
			if err == nil {
				err = errors.New("panicked with a non-error")
			}
		}
	}()
	f()
	return nil
}

// SubAgent refuses WithSafety (its sub-run's calls carry their own) and WithTimeout (a deadline
// would cut the sub-run off mid-call), and every tool option refuses an invalid value, each with
// ErrConfig.
func TestToolOptions_Refusals(t *testing.T) {
	sub := mustNew(NewScriptedModel(TextTurn("x")), memJournal())
	fn := func(context.Context, struct{}) (string, error) { return "", nil }
	for name, build := range map[string]func(){
		"SubAgent WithSafety":         func() { MustSubAgent("s", "", sub, WithSafety(Safety{ReadOnly: true})) },
		"SubAgent WithTimeout":        func() { MustSubAgent("s", "", sub, WithTimeout(time.Second)) },
		"nil approval":                func() { MustFunc("f", "", fn, WithApproval(nil)) },
		"approval with no approvers":  func() { MustFunc("f", "", fn, WithApproval(&ApprovalPolicy{Need: 2})) },
		"approval need above n":       func() { MustFunc("f", "", fn, WithApproval(&ApprovalPolicy{Need: 2, Approvers: []string{"a"}})) },
		"zero timeout":                func() { MustFunc("f", "", fn, WithTimeout(0)) },
		"output schema not an object": func() { MustFunc("f", "", fn, WithOutputSchema(json.RawMessage(`[1]`))) },
		"output schema null":          func() { MustFunc("f", "", fn, WithOutputSchema(json.RawMessage(`null`))) },
		"nil option":                  func() { MustFunc("f", "", fn, nil) },
		"CompensatedFunc bad option": func() {
			MustCompensatedFunc("f", "", fn, func(context.Context, struct{}, string) error { return nil }, WithTimeout(-1))
		},
	} {
		if err := panicsWith(build); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: panicked with %v, want an error wrapping ErrConfig", name, err)
		}
	}
	if err := panicsWith(func() { MustSubAgent("s", "", sub, WithApproval(SingleApproval()), WithTitle("Research")) }); err != nil {
		t.Errorf("SubAgent with WithApproval and WithTitle: %v", err)
	}
}

// The options set the spec, and a later WithSafety replaces Func's safety argument. The policy is
// copied in and out, so neither the caller's policy nor a returned spec can change the tool.
func TestToolOptions_SetTheSpec(t *testing.T) {
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}
	out := json.RawMessage(`{"type":"string"}`)
	tool := MustFunc("f", "does f", func(context.Context, struct{}) (string, error) { return "", nil },
		WithSafety(Safety{Idempotent: true}), WithApproval(pol), WithTimeout(time.Second), WithTitle("F"), WithOutputSchema(out))
	pol.Approvers[0] = "mallory"
	s := tool.Spec()
	if s.Name != "f" || s.Description != "does f" || s.Title != "F" || !s.Safety.Idempotent || s.Timeout != time.Second ||
		string(s.Output) != `{"type":"string"}` || s.Approval == nil || s.Approval.Need != 1 || !slices.Equal(s.Approval.Approvers, []string{"ops"}) {
		t.Fatalf("spec = %+v", s)
	}
	if tool.Spec().Safety != (Safety{Idempotent: true}) || string(tool.Spec().Input) != string(s.Input) {
		t.Fatalf("the old methods disagree with the spec: safety %+v", tool.Spec().Safety)
	}
	s.Approval.Approvers[0] = "mallory"
	if tool.Spec().Approval.Approvers[0] != "ops" {
		t.Fatal("changing a returned spec's policy changed the tool's")
	}
}

// oldTool has only the old method set: SpecOf describes it by those methods.
type oldTool struct{ safety Safety }

func (oldTool) Name() string { return "old" }

// Spec describes the tool to the agent (see Tool).
func (t oldTool) Spec() ToolSpec {
	return ToolSpec{Name: t.Name(), Description: t.Description(), Input: t.ArgsSchema(), Safety: t.Safety()}
}

func (oldTool) Description() string         { return "an old tool" }
func (oldTool) ArgsSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t oldTool) Safety() Safety            { return t.safety }
func (oldTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`"ok"`), nil
}

func TestSpecOf_OldMethodSet(t *testing.T) {
	s := (oldTool{safety: Safety{ReadOnly: true}}).Spec()
	want := ToolSpec{Name: "old", Description: "an old tool", Input: json.RawMessage(`{"type":"object"}`), Safety: Safety{ReadOnly: true}}
	if s.Name != want.Name || s.Description != want.Description || string(s.Input) != string(want.Input) || s.Safety != want.Safety || s.Approval != nil || s.Timeout != 0 {
		t.Fatalf("SpecOf = %+v, want %+v", s, want)
	}
}

// fickleTool describes itself differently each time it is asked: the agent must ask once, when
// the tool is registered, and decide every call from that answer.
type fickleTool struct {
	asked *atomic.Int32
	calls *atomic.Int32
}

func (t fickleTool) Spec() ToolSpec {
	s := ToolSpec{Name: "charge", Input: json.RawMessage(`{"type":"object"}`)}
	if t.asked.Add(1) > 1 {
		s.Safety = Safety{ReadOnly: true} // every later answer claims a read
	}
	return s
}
func (fickleTool) Name() string                { return "charge" }
func (fickleTool) Description() string         { return "" }
func (fickleTool) ArgsSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (fickleTool) Safety() Safety              { return Safety{ReadOnly: true} }
func (t fickleTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	t.calls.Add(1)
	return nil, errors.New("gateway timeout")
}

// The spec is read once, at registration, so a side effect stays one for every decision the
// agent makes about its calls: it is claimed, and a middleware that retries it cannot run it twice.
func TestSpec_ReadOnceAtRegistration(t *testing.T) {
	var asked, calls atomic.Int32
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	a := mustNew(
		m,
		store,
		WithTools(fickleTool{asked: &asked, calls: &calls}),
		WithToolMiddleware(naiveRetry(3, false)),
	)
	if _, err := a.Run(context.Background(), "r1", UserText("pay")); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || asked.Load() != 1 {
		t.Fatalf("the tool ran %d times and was asked for its spec %d times; want 1 and 1", calls.Load(), asked.Load())
	}
	if _, ok := hasStep(t, store, "r1", toolAttemptStep("c1")); !ok {
		t.Fatal("no attempt marker: the call was not treated as the side effect its spec declared")
	}
}

// Each model request is sent the tool specs sorted by name, in a slice of its own: a middleware
// that appends to or reorders one request's tools does not change the next request's.
func TestRequestTools_SortedAndOwnedPerRequest(t *testing.T) {
	fn := func(context.Context, struct{}) (string, error) { return "", nil }
	b := MustFunc("b", "", fn, WithSafety(Safety{ReadOnly: true}))
	a := MustFunc("a", "", fn, WithSafety(Safety{ReadOnly: true}))
	var seen [][]string
	mw := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			var names []string
			for _, s := range call.Request.Tools {
				names = append(names, s.Name)
			}
			seen = append(seen, names)
			slices.Reverse(call.Request.Tools)
			call.Request.Tools = append(call.Request.Tools, ToolSpec{Name: "injected"})
			return next(ctx, call)
		}
	}
	m := NewScriptedModel(ToolTurn("c1", "a", `{}`), TextTurn("done"))
	if _, err := mustNew(m, memJournal(), WithTools(b, a), WithMiddleware(mw)).Run(context.Background(), "r1", UserText("q")); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || !slices.Equal(seen[0], []string{"a", "b"}) || !slices.Equal(seen[1], []string{"a", "b"}) {
		t.Fatalf("requests were sent tools %q; want [a b] each time", seen)
	}
}

// A denied call's record carries the gate that denied it, and a saga step's failure record the
// safety and gate the step ran under, as a completed call's result does.
func TestDenialAndSagaFailure_RecordSafetyAndApproval(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	fn := func(context.Context, struct{}) (string, error) { return "ok", nil }
	charge := MustFunc("charge", "", fn, WithApproval(SingleApproval()))
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	a := mustNew(m, store, WithTools(charge))
	if _, err := a.Run(ctx, "r1", UserText("pay")); !IsPause(err) {
		t.Fatalf("first drive: %v, want the approval pause", err)
	}
	if err := Approve(ctx, store, "r1", "c1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, "r1", UserText("pay")); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if rec, ok := hasStep(t, store, "r1", ToolResultStep("c1")); !ok || !rec.IsError || !strings.Contains(string(rec.Raw()), `"safety":{},"approval":{"single":true}`) {
		t.Fatalf("denial journaled %s, want the safety and the gate", rec.Raw())
	}

	book := MustFunc("book", "", func(context.Context, struct{}) (string, error) {
		return "", errors.New("no rooms left")
	}, WithSafety(Safety{Idempotent: true}), WithApproval(&ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}))
	sm := NewScriptedModel(ToolTurn("b1", "book", `{}`), TextTurn("done"))
	sa := mustNew(sm, store, WithTools(book), WithApproverVerifiers(fakeVerifiers("ops")))
	if _, err := sa.Run(ctx, "s1", UserText("trip"), WithSaga()); !IsPause(err) {
		t.Fatalf("saga first drive: %v, want the quorum pause", err)
	}
	approveAs(t, store, "s1", "b1", "ops", true)
	var aborted *SagaAborted
	if _, err := sa.Run(ctx, "s1", UserText("trip"), WithSaga()); !errors.As(err, &aborted) {
		t.Fatalf("saga resume: %v, want *SagaAborted", err)
	}
	recs, err := store.History(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Kind == StepSagaFail {
			if !strings.Contains(string(r.Raw()), `"safety":{"idempotent":true},"approval":{"need":1,"approvers":["ops"]}`) {
				t.Fatalf("saga failure journaled %s, want the safety and the gate", r.Raw())
			}
			return
		}
	}
	t.Fatal("no saga failure record")
}

// The journal encodes SingleApproval as {"single":true}, the protocol's form, and reads it back as
// SingleApproval; an m-of-n policy round-trips as need and approvers; {"single":true} with need or
// approvers is refused.
func TestApprovalPolicy_JSON(t *testing.T) {
	b, err := json.Marshal(SingleApproval())
	if err != nil || string(b) != `{"single":true}` {
		t.Fatalf("SingleApproval encodes as %s, %v", b, err)
	}
	var back ApprovalPolicy
	if err := json.Unmarshal(b, &back); err != nil || !back.single() || back.Validate() != nil {
		t.Fatalf("decoded %+v (single %v), %v", back, back.single(), err)
	}
	m := &ApprovalPolicy{Need: 2, Approvers: []string{"a", "b"}}
	b, _ = json.Marshal(m)
	if string(b) != `{"need":2,"approvers":["a","b"]}` {
		t.Fatalf("m-of-n encodes as %s", b)
	}
	var mb ApprovalPolicy
	if err := json.Unmarshal(b, &mb); err != nil || mb.single() || mb.Need != 2 || len(mb.Approvers) != 2 {
		t.Fatalf("m-of-n decoded %+v, %v", mb, err)
	}
	for _, bad := range []string{
		`{"single":true,"need":1}`, `{"single":true,"approvers":[]}`, `{"single":false}`,
		`{"single":false,"need":1,"approvers":["a"]}`, `{"single":1}`, `{"approvers":["a"]}`, `{}`,
		`{"need":1,"approvers":["a"],"extra":1}`, `{"need":1,"need":2,"approvers":["a"]}`, `{"need":"1","approvers":["a"]}`,
	} {
		if err := json.Unmarshal([]byte(bad), &mb); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s decoded: %v, want ErrProtocol", bad, err)
		}
	}
	p := SingleApproval()
	p.Need = 2
	if _, err := json.Marshal(p); err == nil {
		t.Fatal("a changed SingleApproval encoded")
	}
}
