package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/url"
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

// Two tools with one name would leave the model's call to whichever registered last, silently:
// a host that adds tools from a runtime source (an MCP server) after its own would have its
// own tool replaced by the server's, and the model's arguments sent there. New refuses such an
// agent with ErrConfig, so no run of it starts.
func TestNew_DuplicateToolNamesFailTheRun(t *testing.T) {
	var local, remote int
	mine := MustFunc("lookup", "look up a customer", func(context.Context, struct{}) (string, error) {
		local++
		return "local", nil
	}, WithSafety(Safety{ReadOnly: true}))
	theirs := MustFunc("lookup", "look up anything", func(context.Context, struct{}) (string, error) {
		remote++
		return "remote", nil
	}, WithSafety(Safety{ReadOnly: true}))
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "lookup", `{}`), textTurn("done")}}
	if a, err := New(m, memJournal(), WithTools(mine, theirs)); !errors.Is(err, ErrConfig) || a != nil {
		t.Fatalf("New = %v, %v; want nil and ErrConfig for two tools named lookup", a, err)
	}
	if local+remote != 0 {
		t.Fatalf("a tool ran (local %d, remote %d); want none", local, remote)
	}
}

// A saga resumed to finish its rollback looks each compensator up by tool name, so an agent
// with two tools of one name must fail before the rollback, not undo a write with the wrong
// tool's compensator: New refuses it.
func TestNew_DuplicateToolNamesFailTheSagaRollback(t *testing.T) {
	for _, entry := range []struct {
		name string
		run  func(*Agent) error
	}{
		{"Run WithSaga", func(a *Agent) error {
			_, err := a.Run(context.Background(), "root", UserText("go"), WithSaga())
			return err
		}},
	} {
		t.Run(entry.name, func(t *testing.T) {
			l := newLedger()
			store := &flakyStore{MemStore: NewMemStore(), failCompensations: 1}
			j := mustJournal(store) // the rollback stops
			model := func() Model {
				return &scriptModel{turns: [][]Emit{toolTurn("c1", "A", `{}`), toolTurn("c2", "boom", `{}`)}}
			}
			var ab *SagaAborted
			if err := entry.run(mustNew(model(), j, WithTools(l.write("A"), failTool("boom")))); !errors.As(err, &ab) || ab.CompensateErr == nil {
				t.Fatalf("first attempt err = %v, want *SagaAborted with CompensateErr", err)
			}
			store.mu.Lock()
			store.failCompensations = 0
			store.mu.Unlock()

			impostor := newLedger()
			if a, err := New(model(), j, WithTools(l.write("A"), failTool("boom"), impostor.write("A"))); !errors.Is(err, ErrConfig) || a != nil {
				t.Fatalf("New = %v, %v; want nil and ErrConfig for two tools named A", a, err)
			}
			if n := impostor.undoCount["A"] + l.undoCount["A"]; n != 0 {
				t.Fatalf("a compensator ran %d times on an agent with two tools named A", n)
			}
			// The saga's own tools finish the rollback with the right compensator.
			if err := entry.run(mustNew(model(), j, WithTools(l.write("A"), failTool("boom")))); !errors.As(err, &ab) || ab.CompensateErr != nil {
				t.Fatalf("resume with the saga's own tools: err = %v, want the rollback finished", err)
			}
			if l.undoCount["A"] != 1 || impostor.undoCount["A"] != 0 {
				t.Fatalf("compensations: A %d, impostor %d; want 1 and 0", l.undoCount["A"], impostor.undoCount["A"])
			}
		})
	}
}

// naiveRetry is a third-party retry middleware that ignores Safety: it calls next up to
// 1+n times until one succeeds. spoof, if set, re-labels every call as ReadOnly first.
func naiveRetry(n int, spoof bool) ToolMiddleware {
	return func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			var res json.RawMessage
			var err error
			for range 1 + n {
				if spoof {
					call.Spec.Safety = Safety{ReadOnly: true}
				}
				if res, err = next(ctx, call); err == nil {
					return res, nil
				}
			}
			return res, err
		}
	}
}

func chargeRun(t *testing.T, mw ToolMiddleware) (charged int, result string) {
	t.Helper()
	charge := MustFunc("charge", "charge the card", func(context.Context, struct{}) (string, error) {
		charged++ // the payment went through
		return "", errors.New("gateway timeout")
	})
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	if _, err := mustNew(m, store, WithTools(charge), WithToolMiddleware(mw)).Run(context.Background(), "r1", UserText("pay")); err != nil {
		t.Fatal(err)
	}
	rec, _ := hasStep(t, store, "r1", ToolResultStep("c1"))
	return charged, string(rec.Result)
}

// Whatever a middleware does, a tool that is not retry-safe runs at most once per tool call.
func TestToolReinvoke_NaiveRetryCannotRepeatASideEffect(t *testing.T) {
	for _, spoof := range []bool{false, true} {
		if charged, _ := chargeRun(t, naiveRetry(3, spoof)); charged != 1 {
			t.Errorf("spoofed safety=%v: charged %d times, want 1", spoof, charged)
		}
	}
}

// A retry-safe tool is not held to one invocation: retrying it is the middleware's call.
func TestToolReinvoke_RetrySafeToolMayRetry(t *testing.T) {
	var calls int
	flaky := MustFunc("lookup", "look up", func(context.Context, struct{}) (string, error) {
		if calls++; calls < 3 {
			return "", errors.New("flaky")
		}
		return "found", nil
	}, WithSafety(Safety{Idempotent: true}))
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "lookup", `{}`), TextTurn("done"))
	if _, err := mustNew(m, store, WithTools(flaky), WithToolMiddleware(naiveRetry(3, false))).Run(context.Background(), "r1", UserText("q")); err != nil {
		t.Fatal(err)
	}
	if rec, _ := hasStep(t, store, "r1", ToolResultStep("c1")); calls != 3 || rec.IsError || !strings.Contains(string(rec.Result), "found") {
		t.Fatalf("calls = %d, result %s (is_error=%v); want 3 calls ending in found", calls, rec.Result, rec.IsError)
	}
}

// Tool middleware sees the registered tool's spec, and the run the call belongs to.
func TestToolCall_CarriesTheSpec(t *testing.T) {
	var got ToolCall
	peek := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			got = call
			return next(ctx, call)
		}
	}
	lookup := MustFunc("lookup", "look up", func(context.Context, struct{}) (string, error) { return "x", nil }, WithSafety(Safety{ReadOnly: true}), WithTitle("Lookup"), WithTimeout(time.Minute))
	m := NewScriptedModel(ToolTurn("c1", "lookup", `{}`), TextTurn("done"))
	if _, err := mustNew(m, memJournal(), WithTools(lookup), WithToolMiddleware(peek)).Run(context.Background(), "r1", UserText("q")); err != nil {
		t.Fatal(err)
	}
	if got.Use.ID != "c1" || got.RunID != "r1" || got.Spec.Name != "lookup" || !got.Spec.Safety.ReadOnly || got.Spec.Title != "Lookup" || got.Spec.Timeout != time.Minute {
		t.Fatalf("ToolCall = %+v; want call c1 of run r1 with lookup's spec", got)
	}
}

func TestRedactURLs(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"no url here", "no url here"},
		{"see https://docs.test/errors/42.", "see https://docs.test/errors/42."},
		{"https://u:p@h.test/x", "https://REDACTED@h.test/x"},
		{"https://tok@h.test", "https://REDACTED@h.test"},
		{"https://h.test/a@b/c", "https://h.test/a@b/c"},
		{"https://h.test/x?a=1&b=&c", "https://h.test/x?a=REDACTED&b=REDACTED&REDACTED"}, // a bare parameter can be the token itself
		{"https://h.test/x?", "https://h.test/x?"},
		{"https://h.test/x?&a=1&", "https://h.test/x?&a=REDACTED&"},
		{"https://h.test/x#access_token=T", "https://h.test/x#REDACTED"},
		{"https://h.test?k=v#f", "https://h.test?k=REDACTED#REDACTED"},
		{"(at https://h.test/x?k=v).", "(at https://h.test/x?k=REDACTED)."},
		{"a postgres://u:pw@db:5432/app?sslmode=off and ftp://h/f", "a postgres://REDACTED@db:5432/app?sslmode=REDACTED and ftp://h/f"},
	} {
		if got := redactURLs(c.in); got != c.want {
			t.Errorf("redactURLs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A *url.Error's URL is redacted as a whole, even where a scan of the text would end it early (a
// quote in the query) and leave the rest of the credential behind.
func TestToolErrorTextURLError(t *testing.T) {
	ue := &url.Error{Op: "Get", URL: `https://h.test/x?q=a"b&key=SK-SECRET`, Err: errors.New("timeout")}
	got := toolErrorText(nil, "fetch", fmt.Errorf("fetch page: %w", ue))
	if strings.Contains(got, "SK-SECRET") || !strings.Contains(got, `fetch page: Get "https://h.test/x?q=REDACTED&key=REDACTED": timeout`) {
		t.Errorf("toolErrorText = %q", got)
	}
}

// WithToolErrorRedactor replaces the text journaled for a failed call; URL credentials are still
// redacted from whatever it returns.
func TestWithToolErrorRedactor(t *testing.T) {
	type in struct{}
	tool := MustFunc("lookup", "lookup", func(context.Context, in) (string, error) {
		return "", errors.New("account ACCT-NUMBER-SECRET not found")
	}, WithSafety(Safety{ReadOnly: true}))
	var gotTool string
	var gotErr error
	redact := func(tool string, err error) string {
		gotTool, gotErr = tool, err
		return "lookup failed: not found (see https://docs.test/errors?session=SK-QUERY-SECRET)"
	}
	st := memJournal()
	a := mustNew(
		NewScriptedModel(ToolTurn("tu1", "lookup", `{}`), TextTurn("done")),
		st,
		WithTools(tool),
		WithToolErrorRedactor(redact),
	)
	if _, err := a.Run(context.Background(), "r1", UserText("go")); err != nil {
		t.Fatal(err)
	}
	if gotTool != "lookup" || gotErr == nil || !strings.Contains(gotErr.Error(), "ACCT-NUMBER-SECRET") {
		t.Errorf("redactor called with (%q, %v), want the tool's name and its own error", gotTool, gotErr)
	}
	recs, _ := st.History(context.Background(), "r1")
	var result string
	for _, r := range recs {
		if r.Kind == StepToolResult {
			result = string(r.Result)
		}
	}
	if want := `"lookup failed: not found (see https://docs.test/errors?session=REDACTED)"`; result != want {
		t.Errorf("journaled result = %s, want %s", result, want)
	}
	// A derived agent (RunTyped clones the agent) keeps the redactor.
	if c := a.clone(); c.toolErrRedact == nil {
		t.Error("a cloned agent dropped its tool-error redactor")
	}
}

// ToolErrorText gives a tool middleware the text the agent journals for a failed call: the
// agent's redactor's, with URLs redacted. Outside an agent's tool call it redacts URLs only.
func TestToolErrorTextInMiddleware(t *testing.T) {
	type in struct{}
	tool := MustFunc("lookup", "lookup", func(context.Context, in) (string, error) {
		return "", errors.New("account ACCT-NUMBER-SECRET: https://h.test/x?key=SK-SECRET")
	}, WithSafety(Safety{ReadOnly: true}))
	var seen string
	st := memJournal()
	a := must(mustNew(
		NewScriptedModel(ToolTurn("tu1", "lookup", `{}`), TextTurn("done")),
		st,
		WithTools(tool),
		WithToolErrorRedactor(func(_ string, err error) string {
			return strings.ReplaceAll(err.Error(), "ACCT-NUMBER-SECRET", "ACCT")
		}),
	).With(WithToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			res, err := next(ctx, call)
			if err != nil {
				seen = call.ErrorText(err)
			}
			return res, err
		}
	})))
	if _, err := a.Run(context.Background(), "r1", UserText("go")); err != nil {
		t.Fatal(err)
	}
	recs, _ := st.History(context.Background(), "r1")
	var journaled string
	for _, r := range recs {
		if r.Kind == StepToolResult {
			_ = json.Unmarshal(r.Result, &journaled)
		}
	}
	if want := "account ACCT: https://h.test/x?key=REDACTED"; seen != want || journaled != want {
		t.Errorf("middleware saw %q and the journal holds %q, want both %q", seen, journaled, want)
	}
	if got := (ToolCall{Use: ToolUse{Name: "lookup"}}).ErrorText(errors.New("at https://h.test/x?key=SK-SECRET")); got != "at https://h.test/x?key=REDACTED" {
		t.Errorf("ErrorText of a ToolCall the agent did not build = %q", got)
	}
	if got := RedactURLs("https://u:p@h.test/x?key=SK-SECRET"); got != "https://REDACTED@h.test/x?key=REDACTED" {
		t.Errorf("RedactURLs = %q", got)
	}
}

// A model turn that reuses a tool-use ID from an earlier turn is a protocol fault: the loop
// keys results and journal steps by that ID, so the second call would be skipped as if it
// had already run and the run would report success with the call never executed.
func TestRun_ToolUseIDReusedAcrossTurnsIsAnError(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{
		toolTurn("call_lookup0", "lookup", `{"q":"first"}`),
		toolTurn("call_lookup0", "lookup", `{"q":"second"}`),
		textTurn("done"),
	}}
	_, err := mustNew(m, memJournal(), WithTools(tool)).Run(context.Background(), "r", UserText("go"))
	if !errors.Is(err, ErrToolUseIDReused) || !errors.Is(err, ErrProtocol) || !errors.Is(err, ErrModel) {
		t.Fatalf("err = %v (tool ran %d times), want ErrToolUseIDReused wrapping ErrProtocol and ErrModel", err, calls)
	}
	if calls != 1 {
		t.Fatalf("tool ran %d times, want 1", calls)
	}
}

// Two calls in one turn under the same ID are the same fault.
func TestRun_ToolUseIDTwiceInOneTurnIsAnError(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{{
		{Event: ToolCallDelta{Index: 0, ID: "c", Name: "lookup", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: ToolCallDelta{Index: 1, ID: "c", Name: "lookup", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: Finish{Reason: "tool_use"}},
	}, textTurn("done")}}
	_, err := mustNew(m, memJournal(), WithTools(tool)).Run(context.Background(), "r", UserText("go"))
	if !errors.Is(err, ErrToolUseIDReused) {
		t.Fatalf("err = %v (tool ran %d times), want ErrToolUseIDReused", err, calls)
	}
	if calls != 0 {
		t.Fatalf("tool ran %d times, want 0", calls)
	}
}

// A tool call with no ID cannot be keyed at all.
func TestRun_EmptyToolUseIDIsAnError(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{toolTurn("", "lookup", `{}`), textTurn("done")}}
	_, err := mustNew(m, memJournal(), WithTools(tool)).Run(context.Background(), "r", UserText("go"))
	if !errors.Is(err, ErrToolUseIDReused) {
		t.Fatalf("err = %v (tool ran %d times), want ErrToolUseIDReused", err, calls)
	}
}

// The check runs inside the model handler, below middleware, so a retry middleware sees the
// fault and a fresh attempt that issues a new ID completes the run. The rejected turn is never
// journaled.
func TestRun_ToolUseIDReusedIsRetriedByMiddleware(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{
		toolTurn("c1", "lookup", `{"q":"first"}`),
		toolTurn("c1", "lookup", `{"q":"second"}`), // rejected
		toolTurn("c2", "lookup", `{"q":"second"}`), // the retry
		textTurn("done"),
	}}
	retryOnce := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			resp, err := next(ctx, call)
			if errors.Is(err, ErrModel) {
				return next(ctx, call)
			}
			return resp, err
		}
	}
	store := memJournal()
	res, err := mustNew(m, store, WithTools(tool), WithMiddleware(retryOnce)).Run(context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Message
	if out.Text() != "done" || calls != 2 {
		t.Fatalf("out = %q, tool ran %d times; want done and 2", out.Text(), calls)
	}
	recs, _ := store.History(context.Background(), "r")
	for _, r := range recs {
		if r.Kind == StepModel {
			for _, tu := range r.Message.toolUses() {
				if tu.Args != nil && string(tu.Args) == `{"q":"second"}` && tu.ID == "c1" {
					t.Fatal("the rejected turn was journaled")
				}
			}
		}
	}
}

// A journal written before the check existed (an old Gemini run whose second call reused the
// first call's ID) still replays: recorded turns are not re-checked, only live ones.
func TestRun_ReplaysJournalWithReusedToolUseID(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	asst := func(id string) *Message {
		return &Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: id, Name: "lookup", Args: json.RawMessage(`{}`)}}}
	}
	for _, r := range []struct {
		name string
		rec  Record
	}{
		{"@llm/0", Record{Kind: StepModel, Message: asst("call_lookup0")}},
		{"call_lookup0", Record{Kind: StepToolResult, ToolUseID: "call_lookup0", Result: json.RawMessage(`1`)}},
		{"@llm/1", Record{Kind: StepModel, Message: asst("call_lookup0")}},
	} {
		if _, err := store.do(ctx, "r", r.name, func(context.Context) (Record, error) { return r.rec, nil }); err != nil {
			t.Fatal(err)
		}
	}
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{textTurn("done")}}
	res, err := mustNew(m, store, WithTools(tool)).Run(ctx, "r", UserText("go"))
	if err != nil {
		t.Fatalf("replay of a recorded journal failed: %v", err)
	}
	out := res.Message
	if out.Text() != "done" || calls != 0 {
		t.Fatalf("out = %q, tool ran %d times; want done and 0", out.Text(), calls)
	}
}

// RunTyped: a final_answer rejected on one turn must not leave its ID to shadow a valid
// final_answer on the next. Before the check, the second call was skipped as already done and
// the run failed with ErrProtocol; now the reuse is reported as the model fault it is.
func TestRunTyped_ReusedFinalAnswerIDIsReported(t *testing.T) {
	m := &countModel{inner: NewScriptedModel(
		ToolTurn("call_final_answer0", finalAnswerTool, `{"name":5}`),
		ToolTurn("call_final_answer0", finalAnswerTool, `{"name":"ok"}`),
		TextTurn("done"),
	)}
	_, _, err := mustNew(m, memJournal(), WithMaxTurns(4)).RunTyped[typedAnswer](context.Background(), "r", UserText("go"))
	if !errors.Is(err, ErrToolUseIDReused) {
		t.Fatalf("err = %v, want ErrToolUseIDReused", err)
	}
}

// The check must also cover a response that a middleware returns without getting it from the
// model handler below it (a hedge backup, a fallback model, a cache): the journal records what
// the chain returns, so a reused ID from there skips the call just the same.
func TestRun_ToolUseIDReusedBySubstitutedResponseIsAnError(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{}`), textTurn("done")}}
	turn := 0
	substitute := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			turn++
			if turn == 2 { // answer the second turn from elsewhere, reusing the first turn's ID
				return ModelResponse{Message: Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "lookup", Args: json.RawMessage(`{}`)}}}}, nil
			}
			return next(ctx, call)
		}
	}
	_, err := must(mustNew(m, memJournal(), WithTools(tool), WithMiddleware(substitute)).With(WithMaxTurns(4))).Run(context.Background(), "r", UserText("go"))
	if !errors.Is(err, ErrToolUseIDReused) {
		t.Fatalf("err = %v (tool ran %d times), want ErrToolUseIDReused", err, calls)
	}
	if calls != 1 {
		t.Fatalf("tool ran %d times, want 1", calls)
	}
}

// runRecovering runs a and turns a panic into an error, so a test can report a crash as a
// failure instead of taking the whole test binary down.
func runRecovering(a *Agent, runID string) (out Message, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	res, err := a.Run(context.Background(), runID, UserText("go"))
	if err != nil {
		return Message{}, err
	}
	return res.Message, nil
}

// adversarialToolUseIDs are tool-use IDs a provider may send that name, or nearly name, a key
// the engine writes, another call's key, or another call's sub-run, plus IDs a store could
// refuse as a key (very long, non-ASCII).
func adversarialToolUseIDs() []string {
	return []string{
		"x", "tool:x", "attempt:tool:x", "attempt:step:x", "approval:x", "approval-tally:x",
		"@saga/compensate/x", "@saga/args/x", "run:complete", "run:aborted", "@llm/0", "@llm/1", "@llm/2",
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

// Spec describes the tool to the agent (see Tool).
func (t *tracker) Spec() ToolSpec {
	return ToolSpec{Name: t.name, Description: "", Input: json.RawMessage(`{"type":"object"}`), Safety: Safety{}}
}

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
	store := memJournal()
	a := mustNew(&scriptModel{turns: [][]Emit{turn, textTurn("done")}}, store, WithTools(tool))
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
	probe := MustFunc("probe", "", func(ctx context.Context, _ struct{}) (string, error) {
		scopes = append(scopes, runScope(ctx))
		return "ok", nil
	}, WithSafety(Safety{ReadOnly: true}))
	for _, id := range []string{strings.Repeat("k", 5000), strings.Repeat(":", 2000), "日本語", "nul\x00byte"} {
		scopes = nil
		store := memJournal()
		m := &scriptModel{turns: [][]Emit{toolTurn(id, "probe", `{}`), textTurn("done")}}
		if _, err := runRecovering(mustNew(m, store, WithTools(probe)), "r"); err != nil {
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
	store := memJournal()
	b := mustNew(fixedTextModel("from b"), store)
	a := mustNew(
		NewScriptedModel(ToolTurn("b", "b", `{"task":"x"}`), TextTurn("a done")),
		store,
		WithTools(MustSubAgent("b", "b", b)),
	)
	cModel := &countModel{inner: fixedTextModel("from c")}
	c := mustNew(cModel, store)
	root := mustNew(
		NewScriptedModel(
			ToolTurn("a", "a", `{"task":"x"}`),
			ToolTurn("a/b", "c", `{"task":"y"}`),
			ToolTurn("a>b", "c", `{"task":"y"}`),
			TextTurn("root done"),
		),
		store,
		WithTools(MustSubAgent("a", "a", a), MustSubAgent("c", "c", c)),
	)
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
	_, err := runRecovering(mustNew(m, memJournal(), WithTools(tool)), "r")
	if !errors.Is(err, ErrToolUseIDReused) || calls != 0 {
		t.Fatalf("err = %v, tool ran %d times; want ErrToolUseIDReused and 0", err, calls)
	}
}

// crashOnKind fails the first Insert of a record of the given kind without storing it, as if the
// process died after the step that made the record ran and before the record was written.
type crashOnKind struct {
	Store
	kind    StepKind
	crashed atomic.Bool
}

func (c *crashOnKind) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if rec, err := DecodeRecord(data); err == nil && rec.Kind == c.kind && c.crashed.CompareAndSwap(false, true) {
		return Entry{}, false, errDied
	}
	return c.Store.Insert(ctx, runID, name, data)
}

// seedStepAttempt journals the attempt marker a side-effecting Step named name leaves when it
// dies mid-effect, attempted at at.
func seedStepAttempt(t *testing.T, d *Journal, runID, name string, at time.Time) {
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
	j := mustJournal(inner)
	seedStepAttempt(t, j, "r", "x", time.Now())
	tool := &tracker{name: "w"}
	m := &scriptModel{turns: [][]Emit{toolTurn("x", "w", `{}`), textTurn("done")}}
	store := &crashOnKind{Store: inner, kind: StepAttempt} // the call's claim dies before it is written
	a := mustNew(m, mustJournal(store), WithTools(tool))
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
	j := mustJournal(inner)
	now := time.Now()
	seedStepAttempt(t, j, "r", "x", now.Add(-time.Hour))
	tool := &tracker{name: "w"}
	m := &scriptModel{turns: [][]Emit{toolTurn("x", "w", `{}`), textTurn("done")}}
	store := &crashOnKind{Store: inner, kind: StepToolResult} // the call's result dies before it is written
	if _, err := runRecovering(mustNew(m, mustJournal(store), WithTools(tool)), "r"); !errors.Is(err, errDied) {
		t.Fatalf("run: %v, want the simulated crash", err)
	}
	err := ResolveHalt(context.Background(), j, HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "x"}, Cause: HaltCrashed}, Outcome{Result: "ok", IsError: false}, WithMinHaltAge(time.Minute), WithClock(func() time.Time { return now }))
	var young *HaltTooYoung
	if !errors.As(err, &young) {
		t.Fatalf("ResolveHalt = %v, want *HaltTooYoung: the call was attempted just now", err)
	}
}

// A saga rollback skips a call that never started, even when a step with the same string did:
// on main the step's attempt marker made the call look started, and the rollback halted.
func TestSaga_RollbackStepAttemptIsNotAToolCallAttempt(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
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
		if _, err := store.do(ctx, "r1", r.name, func(context.Context) (Record, error) { return r.rec, nil }); err != nil {
			t.Fatal(err)
		}
	}
	write := MustFunc("write", "", func(context.Context, struct{}) (string, error) { return "ok", nil })
	book := MustFunc("book", "", func(context.Context, struct{}) (string, error) { return "", errors.New("no seats") })
	_, err := mustNew(fixedTextModel("done"), store, WithTools(write, book)).Run(ctx, "r1", UserText("trip"), WithSaga())
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || aborted.CompensateErr != nil {
		t.Fatalf("err = %v, want *SagaAborted with a finished rollback (the write never started)", err)
	}
}

// maxEchoedError bounds the error text a model's output can produce: the model-supplied part is
// cut to maxErrorBody bytes, and the rest of the message is short.
const maxEchoedError = maxErrorBody + 1024

// Tool-call arguments that are not valid JSON are the model's own output, of any size. The error
// that reports them must not carry them in full into the caller's logs.
func TestTruncatedToolArgs_ErrorIsBounded(t *testing.T) {
	huge := `{"q":"` + strings.Repeat("a", 1<<20) // never closed
	ch := make(chan Emit, 2)
	ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "c1", Name: "lookup", ArgsFragment: []byte(huge)}}
	ch <- Emit{Event: Finish{Reason: "tool_use"}}
	close(ch)
	_, _, err := NewStream(ch).Message()
	if !errors.Is(err, ErrTruncatedToolArgs) {
		t.Fatalf("err = %v, want ErrTruncatedToolArgs", err)
	}
	if n := len(err.Error()); n > maxEchoedError {
		t.Fatalf("error text is %d bytes, want at most %d", n, maxEchoedError)
	}
}

// The same holds for the tool name, which the model also chooses.
func TestTruncatedToolArgs_ErrorBoundsTheName(t *testing.T) {
	ch := make(chan Emit, 2)
	ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "c1", Name: strings.Repeat("n", 1<<20), ArgsFragment: []byte(`{`)}}
	ch <- Emit{Event: Finish{Reason: "tool_use"}}
	close(ch)
	_, _, err := NewStream(ch).Message()
	if !errors.Is(err, ErrTruncatedToolArgs) {
		t.Fatalf("err = %v, want ErrTruncatedToolArgs", err)
	}
	if n := len(err.Error()); n > maxEchoedError {
		t.Fatalf("error text is %d bytes, want at most %d", n, maxEchoedError)
	}
}

// A model that calls a tool by a name no tool has gets ErrUnknownTool, naming what it asked for,
// but not all of a name of any length.
func TestUnknownTool_ErrorIsBounded(t *testing.T) {
	m := NewScriptedModel(ToolTurn("c1", strings.Repeat("x", 1<<20), `{}`), TextTurn("done"))
	_, err := mustNew(m, memJournal()).Run(context.Background(), "run", UserText("go"))
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("err = %v, want ErrUnknownTool", err)
	}
	if n := len(err.Error()); n > maxEchoedError {
		t.Fatalf("error text is %d bytes, want at most %d", n, maxEchoedError)
	}
}

// A tool middleware can hand the base handler a name no tool has; that error is journaled as the
// call's result, so it is bounded too.
func TestToolHandler_UnknownToolErrorIsBounded(t *testing.T) {
	a := mustNew(NewScriptedModel(TextTurn("x")), memJournal())
	_, _, err := a.toolHandler("r")(context.Background(), ToolUse{ID: "c1", Name: strings.Repeat("y", 1<<20)})
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("err = %v, want ErrUnknownTool", err)
	}
	if n := len(err.Error()); n > maxEchoedError {
		t.Fatalf("error text is %d bytes, want at most %d", n, maxEchoedError)
	}
}

// unwrapStore wraps inner and exposes it through Unwrap, implementing nothing else.
type unwrapStore struct{ inner Store }

func (u unwrapStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	return u.inner.Insert(ctx, runID, name, data)
}
func (u unwrapStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	return u.inner.Get(ctx, runID, name)
}
func (u unwrapStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	return u.inner.Load(ctx, runID, after)
}
func (u unwrapStore) Unwrap() Store { return u.inner }

// listingWrapper overrides Lister itself, so it must take precedence over the inner store's.
type listingWrapper struct{ unwrapStore }

func (listingWrapper) Runs(context.Context, RunFilter) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) { yield("renamed", nil) }
}

// plainStore is a Store with no capabilities.
type plainStore struct{ Store }

func TestCapability(t *testing.T) {
	mem := NewMemStore()

	if l, ok := Capability[Leaser](mem); !ok || l != Leaser(mem) {
		t.Fatalf("Capability[Leaser](MemStore) = %v, %v; want the store itself", l, ok)
	}
	// Two wrappers deep: the lookup follows every Unwrap to the store that has it.
	deep := unwrapStore{unwrapStore{mem}}
	if l, ok := Capability[Leaser](deep); !ok || l != Leaser(mem) {
		t.Fatalf("Capability[Leaser] through two wrappers = %v, %v; want the inner MemStore", l, ok)
	}
	if l, ok := Capability[Lister](deep); !ok || l != Lister(mem) {
		t.Fatalf("Capability[Lister] through two wrappers = %v, %v; want the inner MemStore", l, ok)
	}
	// No more capabilities than the wrapped store: a store without Lister stays without one.
	if _, ok := Capability[Lister](unwrapStore{plainStore{mem}}); ok {
		t.Fatal("a wrapper over a non-Lister reports a Lister")
	}
	// Unwrap returning nil ends the search instead of panicking.
	if _, ok := Capability[Leaser](unwrapStore{nil}); ok {
		t.Fatal("a wrapper whose Unwrap returns nil reports a Leaser")
	}
	if _, ok := Capability[Leaser](nil); ok {
		t.Fatal("a nil store reports a Leaser")
	}
	// A wrapper that implements the capability itself wins over the store it wraps.
	lw := listingWrapper{unwrapStore{mem}}
	l, ok := Capability[Lister](lw)
	if !ok {
		t.Fatal("the overriding wrapper reports no Lister")
	}
	var runs []string
	for id := range l.Runs(context.Background(), RunFilter{}) {
		runs = append(runs, id)
	}
	if len(runs) != 1 || runs[0] != "renamed" {
		t.Fatalf("Capability[Lister] returned the inner store's Lister (runs %v), want the wrapper's", runs)
	}
}

// The engine finds a capability on the store a Journal writes to, directly and through a wrapper's
// Unwrap.
func TestCapabilityOfJournalStore(t *testing.T) {
	mem := NewMemStore()
	j2 := mustJournal(mem)
	j, err := NewJournal(unwrapStore{mem})
	if err != nil {
		t.Fatal(err)
	}
	for name, d := range map[string]*Journal{
		"MemStore":               j2,
		"Journal over a wrapper": j,
	} {
		if l, ok := Capability[Leaser](d.Store()); !ok || l != Leaser(mem) {
			t.Errorf("%s: Capability[Leaser] = %v, %v; want the MemStore", name, l, ok)
		}
	}
}

// ResolveHalt's live-driver check finds the run's Leaser through a Journal and through store
// wrappers that implement Unwrap() Store, as Lease and Recover do: a resolution through a Journal
// over a wrapped MemStore is refused while a driver holds the root run's lease, and goes through
// once it is released. A Journal over a store that exposes no Leaser falls back to the rule for a
// store that cannot lease runs: WithMinHaltAge, or the explicit opt-out.
func TestResolveHalt_FindsTheLeaserThroughAJournal(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	j := newJournal(unwrapStore{unwrapStore{mem}})
	if won, _, err := ClaimAttempt(ctx, j, "r1", toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: 1}); err != nil || !won {
		t.Fatal(won, err)
	}
	if ok, err := mem.AcquireLease(ctx, "r1", "worker-1", time.Minute); err != nil || !ok {
		t.Fatal(ok, err)
	}
	ref := HaltRef{RunID: "r1", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}
	err := ResolveHalt(ctx, j, ref, Outcome{Result: "charged"})
	if _, ok := errors.AsType[*HaltInFlight](err); !ok {
		t.Fatalf("resolving through a Journal under a live lease = %v; want *HaltInFlight", err)
	}
	if err := mem.ReleaseLease(ctx, "r1", "worker-1"); err != nil {
		t.Fatal(err)
	}
	if err := ResolveHalt(ctx, j, ref, Outcome{Result: "charged"}); err != nil {
		t.Fatalf("resolving once the lease is released = %v", err)
	}

	hidden := newJournal(plainStore{NewMemStore()}) // no Unwrap: its Leaser is not exposed
	if won, _, err := ClaimAttempt(ctx, hidden, "r1", toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: 1}); err != nil || !won {
		t.Fatal(won, err)
	}
	if err := ResolveHalt(ctx, hidden, ref, Outcome{Result: "x"}); !errors.Is(err, ErrConfig) {
		t.Fatalf("a Journal over a store with no Leaser, no minimum age = %v; want ErrConfig", err)
	}
	if err := ResolveHalt(ctx, hidden, ref, Outcome{Result: "x"}, WithMinHaltAge(time.Second)); err != nil {
		t.Fatalf("with an old enough attempt = %v", err)
	}
}

// A resolution claims the attempt after the live one, on the lease path as on the min-age path,
// and journals that claim as an attempt marker of the operation; WithoutLiveDriverCheck skips it
// with the other checks.
func TestResolveHalt_ClaimsTheNextAttemptUnlessOptedOut(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		opts  []ResolveOption
		store func() *Journal
		claim bool
	}{
		"lease":   {nil, func() *Journal { return memJournal() }, true},
		"min age": {[]ResolveOption{WithMinHaltAge(time.Second)}, func() *Journal { return newJournal(plainStore{NewMemStore()}) }, true},
		"opt-out": {[]ResolveOption{WithoutLiveDriverCheck()}, func() *Journal { return memJournal() }, false},
	} {
		t.Run(name, func(t *testing.T) {
			d := tc.store()
			if won, _, err := ClaimAttempt(ctx, d, "r1", toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: 1}); err != nil || !won {
				t.Fatal(won, err)
			}
			ref := HaltRef{RunID: "r1", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}
			if err := ResolveHalt(ctx, d, ref, Outcome{Result: "charged"}, tc.opts...); err != nil {
				t.Fatal(err)
			}
			rec, ok, err := d.Get(ctx, "r1", retryAttemptStep(toolAttemptStep("c1"), 1))
			if err != nil {
				t.Fatal(err)
			}
			if ok != tc.claim || ok && (rec.Kind != StepAttempt || !isToolAttempt(rec)) {
				t.Fatalf("the resolution's claim of the next attempt: present %v (%+v), want %v", ok, rec, tc.claim)
			}
		})
	}
}
