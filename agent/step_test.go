package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A Step driver that loses the claim to a call of the step in flight in this process has seen its
// claimant live: that driver owns the effect and may be running it now, so the halt is
// HaltContended, which ResolveHalt does not resolve without WithMinHaltAge. A driver that loses
// the claim with no call in flight knows of no live claimant (the owner may have died), so its
// halt is HaltCrashed, as for a marker found on resume.
//
// The flight is placed by hand so the interleaving is exact: it is what the loser finds when the
// owner's call is in flight and fails (its effect may have fired) while the loser waits on it.
func TestStep_LoserOfALiveClaimHaltsContended(t *testing.T) {
	ctx := context.Background()
	j := newJournal(NewMemStore())
	won, _, _, err := j.claimNext(ctx, "r", stepAttemptStep("s"),
		Record{Kind: StepAttempt, ToolUseID: "s", AttemptedAt: time.Now().UnixMilli()})
	if err != nil || !won {
		t.Fatalf("the owner's claim = %v, %v", won, err)
	}
	k := flightKey{j.id, "r", "s"}
	flights.mu.Lock()
	flights.m[k] = &flight{err: errors.New("provider timeout")}
	flights.mu.Unlock()
	ran := 0
	fn := func(context.Context) (int, error) { ran++; return 1, nil }
	_, err = j.Step(ctx, "r", "s", fn)
	flights.mu.Lock()
	delete(flights.m, k)
	flights.mu.Unlock()
	halt, ok := errors.AsType[*OutcomeUnknown](err)
	if !ok || halt.Op != (OpRef{Kind: OpStep, ID: "s"}) || halt.Cause != HaltContended {
		t.Fatalf("the loser of a live claim = %v (%+v); want an OpStep halt on s with Cause %q", err, halt, HaltContended)
	}
	if ran != 0 {
		t.Fatalf("the loser ran fn %d times, want 0", ran)
	}
	ref := halt.Ref()
	if err := ResolveHalt(ctx, j, ref, Outcome{Result: 1}); !errors.Is(err, ErrConfig) {
		t.Errorf("resolving the contended halt without WithMinHaltAge = %v; want ErrConfig", err)
	}

	// No call in flight: the owner may have died, and nothing says it is live.
	_, err = j.Step(ctx, "r", "s", fn)
	halt, ok = errors.AsType[*OutcomeUnknown](err)
	if !ok || halt.Cause != HaltCrashed {
		t.Fatalf("a loser with no call in flight = %v (%+v); want Cause %q", err, halt, HaltCrashed)
	}
	if ran != 0 {
		t.Fatalf("the loser ran fn %d times, want 0", ran)
	}
}

// P10's rule (#90, F3) for a tool whose error chain holds a halt or a pending approval: it
// propagates as itself, recorded as nothing, whatever the tool's safety. It holds when the chain
// also holds a side-effect Step's refusal to pause (*stepPauseError): the halt comes back, and the
// call records nothing, so the model is not told the call failed.
func TestStepPauseGuard_JoinedWithAHaltPropagatesTheHalt(t *testing.T) {
	store := memJournal()
	mixed := MustFunc("mixed", "", func(ctx context.Context, _ struct{}) (string, error) {
		d, runID, _ := runContext(ctx)
		_, guard := d.Step(ctx, runID, "confirm", func(ctx context.Context) (int, error) {
			return Interrupt[int](ctx, "q", nil)
		})
		_, _, _ = ClaimAttempt(ctx, d, runID, stepAttemptStep("inner"), Record{Kind: StepAttempt, ToolUseID: "inner", AttemptedAt: 1})
		_, halt := d.Step(ctx, runID, "inner", func(context.Context) (int, error) { return 1, nil })
		return "", errors.Join(guard, halt)
	}, WithSafety(Safety{Idempotent: true}))
	_, err := mustNew(
		&greedyModel{script: [][]Emit{toolTurn("c1", "mixed", `{}`), textTurn("done")}},
		store,
		WithTools(mixed),
	).Run(context.Background(), "r1", UserText("go"))
	halt, ok := errors.AsType[*OutcomeUnknown](err)
	if !ok || halt.Op.ID != "inner" {
		t.Fatalf("run = %v; want the inner step's halt", err)
	}
	if _, ok := errors.AsType[*stepPauseError](err); !ok {
		t.Errorf("run = %v; want the guard's error kept in the chain beside the halt", err)
	}
	recs, err := store.History(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Name == ToolResultStep("c1") {
			t.Fatalf("the call recorded %s; want nothing recorded for it", r.Result)
		}
	}
}

// crashOnce is a store whose first Insert of crashName fails without storing it. The engine
// inserts a step's record after the step's fn ran, so the side effect happens and then the record
// never commits, as when the process dies between the two. It does not implement Unwrap, so it
// hides its inner store's Leaser.
type crashOnce struct {
	Store
	crashName string
	crashed   atomic.Bool
}

var errDied = errors.New("process died")

func (c *crashOnce) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if name == c.crashName && c.crashed.CompareAndSwap(false, true) {
		return Entry{}, false, errDied
	}
	return c.Store.Insert(ctx, runID, name, data)
}

// The README's plain-Go order flow: "reserve" is non-idempotent. The process dies after the
// reservation is made and before it is journaled; on resume the step must not reserve again.
// It should halt for confirmation, as a tool call or a plan node in the same position does.
func TestStep_CrashAfterEffectDoesNotRepeatIt(t *testing.T) {
	store := mustJournal(&crashOnce{Store: NewMemStore(), crashName: "reserve"})
	var reserved int
	reserve := func(context.Context) (string, error) { reserved++; return "res-1", nil }

	if _, err := store.Step(context.Background(), "order-42", "reserve", reserve); !errors.Is(err, errDied) {
		t.Fatalf("setup: %v", err)
	}
	_, err := store.Step(context.Background(), "order-42", "reserve", reserve) // resume
	if reserved != 1 {
		t.Fatalf("reserved %d times across a crash and resume, want 1 (resume err = %v)", reserved, err)
	}
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) {
		t.Fatalf("resume err = %v, want *OutcomeUnknown for the unconfirmed reservation", err)
	}
}

// A halted step is cleared the way a halted tool call is: ResolveHalt records the confirmed
// outcome under the step's name (ResolveHalt (OpStep)), and the resumed step returns it without running fn.
func TestStep_HaltResolvedByResolveHalt(t *testing.T) {
	for _, failed := range []bool{false, true} {
		store := mustJournal(&crashOnce{Store: NewMemStore(), crashName: "reserve"})
		var reserved int
		reserve := func(context.Context) (string, error) { reserved++; return "res-1", nil }
		_, _ = store.Step(context.Background(), "order-42", "reserve", reserve)
		if err := ResolveHalt(context.Background(), store, HaltRef{RunID: "order-42", Op: OpRef{Kind: OpStep, ID: "reserve"}, Cause: HaltCrashed}, Outcome{Result: "res-1", IsError: failed}, WithoutLiveDriverCheck()); err != nil { // the crash wrapper hides MemStore's Leaser; no driver is running
			t.Fatal(err)
		}
		got, err := store.Step(context.Background(), "order-42", "reserve", reserve)
		if reserved != 1 {
			t.Fatalf("failed=%v: reserved %d times, want 1", failed, reserved)
		}
		if failed && !errors.Is(err, ErrTool) {
			t.Fatalf("resolved as failed: err = %v, want an ErrTool", err)
		}
		if !failed && (err != nil || got != "res-1") {
			t.Fatalf("resolved: got %q, %v; want res-1", got, err)
		}
	}
}

// A step declared retry-safe skips the marker and simply re-runs after a crash.
func TestStep_RetrySafeStepReRunsAfterCrash(t *testing.T) {
	store := mustJournal(&crashOnce{Store: NewMemStore(), crashName: "classify"})
	var runs int
	classify := func(context.Context) (string, error) { runs++; return "rush", nil }
	ro := WithSafety(Safety{ReadOnly: true})
	_, _ = store.Step(context.Background(), "order-42", "classify", classify, ro)
	got, err := store.Step(context.Background(), "order-42", "classify", classify, ro)
	if err != nil || got != "rush" || runs != 2 {
		t.Fatalf("got %q, %v after %d runs; want rush after 2", got, err, runs)
	}
}

// chargeOnce is a side effect that fires and then loses its answer, leaving an attempt marker
// with no result in the journal of run r1, as a crash between the two would.
func chargeOnce(t *testing.T, store *Journal, charged *int) {
	t.Helper()
	charge := MustFunc("charge", "charge the card", func(context.Context, struct{}) (string, error) {
		*charged++
		return "", fmt.Errorf("gateway connection reset (%w)", ErrToolOutcomeUnknown)
	})
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	if _, err := mustNew(m, store, WithTools(charge)).Run(context.Background(), "r1", UserText("pay")); !errors.Is(err, ErrToolOutcomeUnknown) {
		t.Fatalf("first run err = %v, want ErrToolOutcomeUnknown", err)
	}
}

// A call's safety is the one it fired under. A side effect that fired with no recorded result
// must halt the resume even if the tool is now declared retry-safe (a trusted MCP server that
// relabels it read-only, or a code change), or the resume would run it a second time.
func TestResume_RelabelledRetrySafeStillHalts(t *testing.T) {
	store := memJournal()
	var charged int
	chargeOnce(t, store, &charged)

	relabelled := MustFunc("charge", "charge the card", func(context.Context, struct{}) (string, error) {
		charged++
		return "charged", nil
	}, WithSafety(Safety{ReadOnly: true}))
	m := &greedyModel{script: [][]Emit{textTurn("done")}} // the charge turn replays from the journal
	_, err := mustNew(m, store, WithTools(relabelled)).Run(context.Background(), "r1", UserText("pay"))
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) || halt.Op.ID != "c1" {
		t.Fatalf("resume err = %v after %d charges, want *OutcomeUnknown for c1", err, charged)
	}
	if charged != 1 {
		t.Fatalf("charged %d times, want 1", charged)
	}
}

// The halt does not depend on the tool still being registered: the call fired, and its outcome
// is unknown whatever tools the resuming agent has.
func TestResume_AttemptedToolNoLongerRegisteredHalts(t *testing.T) {
	store := memJournal()
	var charged int
	chargeOnce(t, store, &charged)

	m := &greedyModel{script: [][]Emit{textTurn("done")}}
	_, err := mustNew(m, store).Run(context.Background(), "r1", UserText("pay"))
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) || halt.Op.ID != "c1" || halt.Op.ToolName != "charge" {
		t.Fatalf("resume err = %v, want *OutcomeUnknown for charge (c1)", err)
	}
}

// Step keeps the same rule: a step attempted as a side effect halts on resume even when the
// resuming code declares it retry-safe.
func TestStep_RelabelledRetrySafeStillHalts(t *testing.T) {
	store := memJournal()
	var ran int
	reserve := func(context.Context) (string, error) {
		ran++
		return "", errors.New("connection reset after the reservation was sent")
	}
	if _, err := store.Step(context.Background(), "r1", "reserve", reserve); err == nil {
		t.Fatal("first attempt succeeded, want its error")
	}
	_, err := store.Step(context.Background(), "r1", "reserve", reserve, WithSafety(Safety{Idempotent: true}))
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) || halt.Op.ID != "reserve" {
		t.Fatalf("resume err = %v after %d runs, want *OutcomeUnknown for reserve", err, ran)
	}
	if ran != 1 {
		t.Fatalf("the step ran %d times, want 1", ran)
	}
}

// markerLookupFails is a store whose lookups (and writes) of step attempt markers fail.
type markerLookupFails struct{ *MemStore }

func (s markerLookupFails) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	if strings.HasPrefix(name, "attempt:step:") {
		return Entry{}, false, fmt.Errorf("disk on fire (%w)", ErrStorage)
	}
	return s.MemStore.Get(ctx, runID, name)
}

func (s markerLookupFails) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if strings.HasPrefix(name, "attempt:step:") {
		return Entry{}, false, fmt.Errorf("disk on fire (%w)", ErrStorage)
	}
	return s.MemStore.Insert(ctx, runID, name, data)
}

// If the store cannot say whether a retry-safe step was attempted before as a side effect, the
// step does not run: running it could be the second run of that side effect.
func TestStep_MarkerLookupFailureStopsTheStep(t *testing.T) {
	ran := 0
	_, err := mustJournal(markerLookupFails{NewMemStore()}).Step(context.Background(), "r1", "read", func(context.Context) (int, error) {
		ran++
		return 1, nil
	}, WithSafety(Safety{ReadOnly: true}))
	if !errors.Is(err, ErrStorage) || ran != 0 {
		t.Fatalf("err = %v after %d runs, want the store's error and no run", err, ran)
	}
}

// markerAt journals, for run "r", the model turn that called "charge" as c1 and an attempt
// marker for c1 stamped ms (a tampered or hand-written row when ms is not positive).
func markerAt(t *testing.T, ms int64) *MemStore {
	t.Helper()
	ctx := context.Background()
	s := NewMemStore()
	j := mustJournal(s)
	turn := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "charge", Args: []byte(`{}`)}}}
	if _, err := j.do(ctx, "r", modelStep(0), func(context.Context) (Record, error) {
		return Record{Kind: StepModel, Message: &turn}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.do(ctx, "r", toolAttemptStep("c1"), func(context.Context) (Record, error) {
		return Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: ms}, nil
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

// A marker's AttemptedAt that is not a positive Unix-millis time is no timestamp at all: it must
// not pass for an effect attempted in 1969, which would make any grace period look long elapsed.
// WithMinHaltAge then refuses to resolve blind, exactly as for a marker without a timestamp.
func TestResolveHalt_NonPositiveAttemptedAtIsNoTimestamp(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, ms := range []int64{0, -1, -now.UnixMilli()} {
		s := mustJournal(markerAt(t, ms))
		err := ResolveHalt(ctx, s, HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}, Outcome{Result: "charged", IsError: false}, WithMinHaltAge(time.Hour), WithClock(func() time.Time { return now }))
		if !errors.Is(err, ErrConfig) {
			t.Errorf("AttemptedAt %d: ResolveHalt = %v, want ErrConfig (no usable timestamp)", ms, err)
		}
		if h, _ := s.History(ctx, "r"); hasResult(h, "c1") {
			t.Errorf("AttemptedAt %d: a result was recorded despite the min halt age", ms)
		}
	}
}

// A marker stamped in the future (clock skew between nodes, or a tampered row) is younger than
// any grace period: the resolution waits.
func TestResolveHalt_FutureAttemptedAtIsTooYoung(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := mustJournal(markerAt(t, now.Add(24*time.Hour).UnixMilli()))
	err := ResolveHalt(ctx, s, HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}, Outcome{Result: "charged", IsError: false}, WithMinHaltAge(time.Minute), WithClock(func() time.Time { return now }))
	var young *HaltTooYoung
	if !errors.As(err, &young) {
		t.Fatalf("ResolveHalt = %v, want *HaltTooYoung", err)
	}
}

// OutcomeUnknown.AttemptedAt reports an unknown time as zero, never a 1969 instant from a
// non-positive marker.
func TestOutcomeUnknown_NonPositiveAttemptedAtIsZero(t *testing.T) {
	ctx := context.Background()
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) { return "ok", nil })
	a := mustNew(NewScriptedModel(), mustJournal(markerAt(t, -1)), WithTools(charge))
	_, err := a.Run(ctx, "r", UserText("go"))
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) {
		t.Fatalf("Run = %v, want *OutcomeUnknown", err)
	}
	if !halt.AttemptedAt.IsZero() {
		t.Errorf("OutcomeUnknown.AttemptedAt = %v, want zero for a marker stamped -1", halt.AttemptedAt)
	}

	// A Step's marker too.
	s := memJournal()
	if _, err := s.do(ctx, "r", stepAttemptStep("reserve"), func(context.Context) (Record, error) {
		return Record{Kind: StepAttempt, ToolUseID: "reserve", AttemptedAt: -1}, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Step(ctx, "r", "reserve", func(context.Context) (int, error) { return 1, nil })
	if !errors.As(err, &halt) {
		t.Fatalf("Step = %v, want *OutcomeUnknown", err)
	}
	if !halt.AttemptedAt.IsZero() {
		t.Errorf("Step OutcomeUnknown.AttemptedAt = %v, want zero for a marker stamped -1", halt.AttemptedAt)
	}
}

// A retry-safe Step that finds an earlier attempt's marker halts; the marker's non-positive
// timestamp is reported as unknown.
func TestStepHalt_RetrySafeFindsNonPositiveMarker(t *testing.T) {
	ctx := context.Background()
	s := memJournal()
	if _, err := s.do(ctx, "r", stepAttemptStep("reserve"), func(context.Context) (Record, error) {
		return Record{Kind: StepAttempt, ToolUseID: "reserve", AttemptedAt: -1}, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Step(ctx, "r", "reserve", func(context.Context) (int, error) { return 1, nil },
		WithSafety(Safety{ReadOnly: true}))
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) {
		t.Fatalf("Step = %v, want *OutcomeUnknown", err)
	}
	if !halt.AttemptedAt.IsZero() {
		t.Errorf("OutcomeUnknown.AttemptedAt = %v, want zero", halt.AttemptedAt)
	}
}

// claimRacer is a MemStore where another driver writes the attempt marker of call c1, stamped -1,
// just before this driver's claim, so this driver loses the claim.
type claimRacer struct{ *MemStore }

func (s claimRacer) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if name == toolAttemptStep("c1") {
		other, err := JournalEntry(name, Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: -1, claim: "other"})
		if err != nil {
			return Entry{}, false, err
		}
		if _, _, err := s.MemStore.Insert(ctx, runID, name, other); err != nil {
			return Entry{}, false, err
		}
	}
	return s.MemStore.Insert(ctx, runID, name, data)
}

// A driver that loses the claim to a marker stamped -1 reports the attempt time as unknown.
func TestOutcomeUnknown_LostClaimToNonPositiveMarker(t *testing.T) {
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) { return "ok", nil })
	m := &sagaTurns{turns: [][][3]string{{{"c1", "charge", `{}`}}}}
	_, err := mustNew(m, mustJournal(claimRacer{NewMemStore()}), WithTools(charge)).Run(context.Background(), "r", UserText("go"))
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) {
		t.Fatalf("Run = %v, want *OutcomeUnknown from the lost claim", err)
	}
	if !halt.AttemptedAt.IsZero() {
		t.Errorf("OutcomeUnknown.AttemptedAt = %v, want zero", halt.AttemptedAt)
	}
}

// A saga rollback that stops on a started call reports a non-positive marker's time as unknown.
func TestSagaRollbackHalt_NonPositiveMarker(t *testing.T) {
	ctx := context.Background()
	pay := MustCompensatedFunc("pay", "", func(context.Context, struct{}) (string, error) { return "p", nil },
		func(context.Context, struct{}, string) error { return nil })
	book := MustFunc("book", "", func(context.Context, struct{}) (string, error) { return "", errors.New("no seats") })
	s := memJournal()
	turn := Message{Role: RoleAssistant, Parts: []Part{
		ToolUse{ID: "p1", Name: "pay", Args: []byte(`{}`)},
		ToolUse{ID: "b1", Name: "book", Args: []byte(`{}`)},
	}}
	for name, rec := range map[string]Record{
		modelStep(0):          {Kind: StepModel, Message: &turn},
		toolAttemptStep("p1"): {Kind: StepAttempt, ToolUseID: "p1", AttemptedAt: -1},
		ToolResultStep("b1"):  {Kind: StepSagaFail, ToolUseID: "b1", Result: []byte(`"no seats"`)},
	} {
		if _, err := s.do(ctx, "r", name, func(context.Context) (Record, error) { return rec, nil }); err != nil {
			t.Fatal(err)
		}
	}
	_, err := mustNew(&sagaTurns{}, s, WithTools(pay, book)).Run(ctx, "r", UserText("trip"), WithSaga())
	var aborted *SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("saga Run = %v, want *SagaAborted", err)
	}
	var halt *OutcomeUnknown
	if !errors.As(aborted.CompensateErr, &halt) || halt.Op.ID != "p1" {
		t.Fatalf("CompensateErr = %v, want a OutcomeUnknown on p1", aborted.CompensateErr)
	}
	if !halt.AttemptedAt.IsZero() {
		t.Errorf("OutcomeUnknown.AttemptedAt = %v, want zero", halt.AttemptedAt)
	}
}
