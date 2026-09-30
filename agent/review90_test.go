package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// F1. A Waker failure in one parallel call must not cut off a sibling side effect in flight.
// The loop holds a pause (and a lost answer, ErrToolOutcomeUnknown) until every sibling has
// finished; a *wakeError is returned as a plain group error, so errgroup cancels gctx and a
// non-retriable sibling mid-effect is left with a marker and no result: the resume halts on it.
func TestR90_WakeFailureCutsOffSiblingInFlight(t *testing.T) {
	store := NewMemStore()
	bIn := make(chan struct{})
	var bOnce sync.Once
	var bFired atomic.Int32
	nap := Func("nap", "", Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
		<-bIn // the sibling's effect is in flight
		if err := Sleep(ctx, "nap", time.Hour); err != nil {
			return "", err
		}
		return "rested", nil
	})
	send := Func("send", "", Safety{}, func(ctx context.Context, _ struct{}) (string, error) {
		bOnce.Do(func() { close(bIn) })
		select {
		case <-ctx.Done():
			return "", ctx.Err() // cut off mid-call: outcome unknown
		case <-time.After(300 * time.Millisecond):
		}
		bFired.Add(1)
		return "sent", nil
	})
	turn := multiToolTurn([2]string{"c1", "nap"}, [2]string{"c2", "send"})
	ctx := WithWaker(context.Background(), &failingWaker{fail: 1})
	_, err := New(&greedyModel{script: [][]Emit{turn, textTurn("done")}}, store, nap, send).Run(ctx, "r1", "go")
	if !errors.Is(err, ErrStorage) {
		t.Fatalf("first drive = %v; want the wake failure (ErrStorage)", err)
	}
	if bFired.Load() != 1 {
		t.Errorf("send fired %d times; want 1: a wake failure must not cut off a sibling in flight", bFired.Load())
	}
	_, err = New(&greedyModel{script: [][]Emit{textTurn("done")}}, store, nap, send).Run(ctx, "r1", "go")
	if h, ok := errors.AsType[*OutcomeUnknown](err); ok {
		t.Errorf("re-drive = %v (op %s); a transient scheduler failure turned send into an unknown-outcome halt in a run that never crashed", err, h.Op.ID)
	}
}

// F2. A tool call a LIVE driver is running is reported to a second driver as HaltCrashed (the
// second driver cannot know the first is live). ResolveHaltRef must not resolve it blind: the
// live driver's own result must be the one recorded.
func TestR90_LiveToolClaimIsClassifiedCrashed(t *testing.T) {
	ctx := context.Background()
	store := newXprocStore() // separate processes: no in-process singleflight
	inEffect, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	charge := Func("charge", "", Safety{}, func(ctx context.Context, _ struct{}) (string, error) {
		once.Do(func() { close(inEffect) })
		<-release
		return "charged:txn-1", nil
	})
	errA := make(chan error, 1)
	go func() {
		_, e := New(&greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}, store, charge).Run(ctx, "r1", "pay")
		errA <- e
	}()
	<-inEffect // driver A holds the claim and is inside the effect

	_, err := New(&greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}, store, charge).Run(ctx, "r1", "pay")
	halt, ok := errors.AsType[*OutcomeUnknown](err)
	if !ok {
		close(release)
		t.Fatalf("second driver = %v; want *OutcomeUnknown", err)
	}
	// The reconciler does what the docs show: resolve halt.Ref(). The store cannot say whether a
	// driver is live (no Leaser), so without a minimum age the resolution must be refused.
	rerr := ResolveHaltRef(ctx, store, halt.Ref(), Outcome{Result: "not charged", IsError: true})
	close(release)
	if aerr := <-errA; aerr != nil {
		t.Logf("driver A: %v", aerr)
	}
	if rerr == nil {
		t.Errorf("ResolveHaltRef(halt.Ref()) resolved a call still in flight")
	}
	if rec, ok := hasStep(t, store, "r1", ToolResultStep("c1")); !ok || string(rec.Result) != `"charged:txn-1"` || rec.IsError {
		t.Errorf("journal says %s (IsError=%v); want the live driver's own result charged:txn-1", rec.Result, rec.IsError)
	}
}

// F2b. The same for a Step: a lost claim to a live driver must not be resolved blind.
func TestR90_LiveStepClaimIsClassifiedCrashed(t *testing.T) {
	ctx := context.Background()
	store := newXprocStore() // separate processes: no in-process singleflight
	inEffect, release := make(chan struct{}), make(chan struct{})
	type res struct {
		v   string
		err error
	}
	resA := make(chan res, 1)
	go func() {
		v, e := Step(ctx, store, "r1", "charge", func(context.Context) (string, error) {
			close(inEffect)
			<-release
			return "charged:txn-1", nil
		})
		resA <- res{v, e}
	}()
	<-inEffect
	_, err := Step(ctx, store, "r1", "charge", func(context.Context) (string, error) { return "second", nil })
	halt, ok := errors.AsType[*OutcomeUnknown](err)
	if !ok {
		close(release)
		t.Fatalf("second driver = %v; want *OutcomeUnknown", err)
	}
	rerr := ResolveHaltRef(ctx, store, halt.Ref(), Outcome{Result: "operator-guess"})
	close(release)
	a := <-resA
	if rerr == nil {
		t.Errorf("resolved a live step")
	}
	if a.v != "charged:txn-1" || a.err != nil {
		t.Errorf("the live driver's Step returned (%q, %v); want its own charged:txn-1", a.v, a.err)
	}
}

// F3. Classification change for a joined error: a non-retriable tool whose error chain holds an
// Interrupt ahead of a sub-operation's halt. main propagated the halt (errors.As found it anywhere
// in the chain); the PR's AsPause switch looks only at the FIRST pause and returns ErrConfig.
func TestR90_JoinedInterruptThenHaltKeepsTheHalt(t *testing.T) {
	store := NewMemStore()
	mixed := Func("mixed", "", Safety{}, func(ctx context.Context, _ struct{}) (string, error) {
		d, runID, _ := runContext(ctx)
		_, e1 := Interrupt[string](ctx, "q", nil)
		_, _, _ = ClaimAttempt(ctx, d, runID, stepAttemptStep("inner"), Record{Kind: StepAttempt, ToolUseID: "inner", AttemptedAt: 1})
		_, e2 := Step(ctx, d, runID, "inner", func(context.Context) (int, error) { return 1, nil })
		return "", errors.Join(e1, e2)
	})
	_, err := New(&greedyModel{script: [][]Emit{toolTurn("c1", "mixed", `{}`), textTurn("done")}}, store, mixed).Run(context.Background(), "r1", "go")
	var halt *ResumeHalt
	if !errors.As(err, &halt) || errors.Is(err, ErrConfig) {
		t.Errorf("run = %v; want the inner halt propagated (as on main), not ErrConfig", err)
	}
}

// F4. Resolving an already-resolved halt with a different verdict must not report success.
func TestR90_SecondConflictingResolutionIsSilent(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	if won, _, err := ClaimAttempt(ctx, store, "r1", toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: 1}); err != nil || !won {
		t.Fatal(won, err)
	}
	ref := HaltRef{RunID: "r1", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}
	if err := ResolveHaltRef(ctx, store, ref, Outcome{Result: "charged"}); err != nil {
		t.Fatal(err)
	}
	if err := ResolveHaltRef(ctx, store, ref, Outcome{Result: "charged"}); err != nil {
		t.Errorf("an identical repeat = %v; want nil", err)
	}
	err := ResolveHaltRef(ctx, store, ref, Outcome{Result: "refunded", IsError: true})
	rec, _ := hasStep(t, store, "r1", ToolResultStep("c1"))
	if string(rec.Result) != `"charged"` || rec.IsError {
		t.Fatalf("the first resolution was overwritten: %s", rec.Result)
	}
	if !errors.Is(err, ErrConfig) {
		t.Errorf("second, conflicting resolution = %v; want an error wrapping ErrConfig (the journal says %s)", err, rec.Result)
	}
	ar, ok := errors.AsType[*HaltAlreadyResolved](err)
	if !ok || !errors.Is(err, ErrAlreadyResolved) || string(ar.Result) != `"charged"` || ar.IsError || ar.Op != ref.Op {
		t.Errorf("second, conflicting resolution = %#v; want *HaltAlreadyResolved carrying the recorded \"charged\"", err)
	}
}

// With a store that leases runs, a resolution is refused while any driver holds the root run's
// lease (a sub-run's halt checks its root), holds the lease itself while it writes, and gives it
// back after.
func TestResolveHaltRef_RefusesWhileTheRunIsLeased(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	sub := SubRunID("r1", "c9")
	if won, _, err := ClaimAttempt(ctx, store, sub, toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: 1}); err != nil || !won {
		t.Fatal(won, err)
	}
	if ok, err := store.AcquireLease(ctx, "r1", "worker-1", time.Minute); err != nil || !ok {
		t.Fatal(ok, err)
	}
	ref := HaltRef{RunID: sub, Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}
	err := ResolveHaltRef(ctx, store, ref, Outcome{Result: "charged"})
	if inFlight, ok := errors.AsType[*HaltInFlight](err); !ok || inFlight.RootRunID != "r1" {
		t.Fatalf("resolving under a live lease = %v; want *HaltInFlight on r1", err)
	}
	if _, ok := hasStep(t, store, sub, ToolResultStep("c1")); ok {
		t.Fatal("a refused resolution recorded a result")
	}
	if err := store.ReleaseLease(ctx, "r1", "worker-1"); err != nil {
		t.Fatal(err)
	}
	if err := ResolveHaltRef(ctx, store, ref, Outcome{Result: "charged"}); err != nil {
		t.Fatalf("resolving once the lease is released = %v", err)
	}
	if ok, err := store.AcquireLease(ctx, "r1", "worker-2", time.Minute); err != nil || !ok {
		t.Fatalf("after the resolution the run's lease is still held (%v, %v); it must be given back", ok, err)
	}
}

// A store that cannot lease runs needs WithMinHaltAge, or the explicit WithoutLiveDriverCheck.
func TestResolveHaltRef_WithoutALeaserNeedsAnAgeOrAnOptOut(t *testing.T) {
	ctx := context.Background()
	store := newXprocStore()
	if won, _, err := ClaimAttempt(ctx, store, "r1", toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: 1}); err != nil || !won {
		t.Fatal(won, err)
	}
	ref := HaltRef{RunID: "r1", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}
	if err := ResolveHaltRef(ctx, store, ref, Outcome{Result: "x"}); !errors.Is(err, ErrConfig) {
		t.Fatalf("no Leaser, no minimum age = %v; want ErrConfig", err)
	}
	if err := ResolveHalt(ctx, store, "r1", "c1", "x", false); !errors.Is(err, ErrConfig) {
		t.Fatalf("the ResolveHalt wrapper, no Leaser, no minimum age = %v; want ErrConfig (the same rule)", err)
	}
	if err := ResolveHaltRef(ctx, store, ref, Outcome{Result: "x"}, WithMinHaltAge(time.Second)); err != nil {
		t.Fatalf("with an old enough attempt = %v", err)
	}
	store2 := newXprocStore()
	_, _, _ = ClaimAttempt(ctx, store2, "r1", toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: time.Now().UnixMilli()})
	if err := ResolveHaltRef(ctx, store2, ref, Outcome{Result: "x"}, WithoutLiveDriverCheck()); err != nil {
		t.Fatalf("with the explicit opt-out = %v", err)
	}
}

// F5. The resolve wrappers write what ResolveHaltRef writes (expected to pass).
func TestR90_ResolveWrappersSameRecords(t *testing.T) {
	ctx := context.Background()
	type w struct {
		Name, ToolUseID string
		Kind            StepKind
		Result          json.RawMessage
		IsError         bool
		Reconciled      bool
		Evidence        json.RawMessage
	}
	proj := func(d Durable, name string) w {
		recs, _ := d.History(ctx, "r1")
		for _, r := range recs {
			if r.Name == name {
				return w{r.Name, r.ToolUseID, r.Kind, r.Result, r.IsError, r.Reconciled, r.Evidence}
			}
		}
		return w{}
	}
	seed := func(key, id string) Durable {
		s := NewMemStore()
		if won, _, err := ClaimAttempt(ctx, s, "r1", key, Record{Kind: StepAttempt, ToolUseID: id, AttemptedAt: 1}); err != nil || !won {
			t.Fatal(won, err)
		}
		return s
	}
	a, b := seed(toolAttemptStep("c1"), "c1"), seed(toolAttemptStep("c1"), "c1")
	if err := ResolveHalt(ctx, a, "r1", "c1", map[string]int{"x": 1}, true, WithEvidence("ev")); err != nil {
		t.Fatal(err)
	}
	if err := ResolveHaltRef(ctx, b, HaltRef{RunID: "r1", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}, Outcome{Result: map[string]int{"x": 1}, IsError: true, Evidence: "ev"}); err != nil {
		t.Fatal(err)
	}
	if x, y := proj(a, ToolResultStep("c1")), proj(b, ToolResultStep("c1")); !jsonEq(x, y) {
		t.Errorf("tool: %+v vs %+v", x, y)
	}
	a, b = seed(stepAttemptStep("s"), "s"), seed(stepAttemptStep("s"), "s")
	if err := ResolveStepHalt(ctx, a, "r1", "s", "v", false); err != nil {
		t.Fatal(err)
	}
	if err := ResolveHaltRef(ctx, b, HaltRef{RunID: "r1", Op: OpRef{Kind: OpStep, ID: "s"}, Cause: HaltCrashed}, Outcome{Result: "v"}); err != nil {
		t.Fatal(err)
	}
	if x, y := proj(a, "s"), proj(b, "s"); !jsonEq(x, y) || x.Name == "" {
		t.Errorf("step: %+v vs %+v", x, y)
	}
}

func jsonEq(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// A wake failure beside an ordinary pause in the same turn fails the run: the run is not merely
// waiting, since the failed wake was never registered and must be scheduled again.
func TestWaker_FailureBesideAPauseFailsTheRun(t *testing.T) {
	ask := Func("ask", "", Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
		_, err := Interrupt[string](ctx, "q", nil)
		return "", err
	})
	turn := multiToolTurn([2]string{"c1", "ask"}, [2]string{"c2", "nap"})
	ctx := WithWaker(context.Background(), &failingWaker{fail: 1})
	_, err := New(&greedyModel{script: [][]Emit{turn, textTurn("done")}}, NewMemStore(), ask, napTool()).Run(ctx, "r1", "go")
	if !errors.Is(err, ErrStorage) || IsPause(err) {
		t.Fatalf("run = %v; want the wake failure (ErrStorage), not the pause", err)
	}
}
