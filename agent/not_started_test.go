package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

var errCrashed = errors.New("the process died")

// markerHookStore is a MemStore that acts as soon as it has recorded a new attempt marker: it
// cancels the driver's context, as a shutdown or a lost lease landing at that moment would, and
// when crash is set it also fails every later write, as a process that died right there would.
type markerHookStore struct {
	*MemStore
	cancel func()
	crash  bool
	dead   atomic.Bool
}

func (s *markerHookStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if s.dead.Load() {
		return Entry{}, false, errCrashed
	}
	e, inserted, err := s.MemStore.Insert(ctx, runID, name, data)
	if rec, derr := DecodeRecord(data); err == nil && inserted && derr == nil && rec.Kind == StepAttempt { // a marker this call wrote, not one it read
		s.cancel()
		if s.crash {
			s.dead.Store(true)
		}
	}
	return e, inserted, err
}

// A tool call whose run is cancelled after it won its attempt claim but before the tool was
// called provably did not start. The run records that, so a resume calls the tool instead of
// halting for a human over an effect that never fired.
func TestTool_CancelledBeforeItStartsIsReattempted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &markerHookStore{MemStore: NewMemStore(), cancel: cancel}
	j := mustJournal(store)
	var calls atomic.Int32
	charge := MustFunc("charge", "charge the card", func(context.Context, struct{}) (string, error) {
		calls.Add(1)
		return "charged", nil
	})
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	_, err := mustNew(m, j, WithTools(charge)).Run(ctx, "r1", UserText("pay"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run err = %v, want context.Canceled", err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("the tool was called %d times after its run was cancelled, want 0", n)
	}

	resume := &greedyModel{script: [][]Emit{textTurn("done")}}
	res, err := mustNew(resume, mustJournal(store.MemStore), WithTools(charge)).Run(context.Background(), "r1", UserText("pay"))
	if err != nil {
		t.Fatalf("resume err = %v, want the run to call the tool that never started", err)
	}
	out := res.Message
	if out.Text() != "done" || calls.Load() != 1 {
		t.Fatalf("resume = %q with %d charges, want \"done\" and exactly 1", out.Text(), calls.Load())
	}
}

// A process that dies in the same window leaves no record that the call did not start, so the
// resume halts: the claim alone cannot say whether the effect fired.
func TestTool_CrashBeforeItStartsStillHalts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &markerHookStore{MemStore: NewMemStore(), cancel: cancel, crash: true}
	j := mustJournal(store)
	var calls atomic.Int32
	charge := MustFunc("charge", "charge the card", func(context.Context, struct{}) (string, error) {
		calls.Add(1)
		return "charged", nil
	})
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	if _, err := mustNew(m, j, WithTools(charge)).Run(ctx, "r1", UserText("pay")); err == nil {
		t.Fatal("the run whose process died succeeded")
	}
	resume := &greedyModel{script: [][]Emit{textTurn("done")}}
	_, err := mustNew(resume, mustJournal(store.MemStore), WithTools(charge)).Run(context.Background(), "r1", UserText("pay"))
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) || halt.Op.ID != "c1" {
		t.Fatalf("resume err = %v after %d charges, want *OutcomeUnknown for c1", err, calls.Load())
	}
}

// Step keeps the same rule: cancelled after its claim and before fn, it records that fn did not
// start, and the next attempt runs fn.
func TestStep_CancelledBeforeItStartsIsReattempted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &markerHookStore{MemStore: NewMemStore(), cancel: cancel}
	j := mustJournal(store)
	var entered, ran atomic.Int32
	reserve := reserveFn(&entered, &ran)
	if _, err := j.Step(ctx, "r1", "reserve", reserve); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled step err = %v, want context.Canceled", err)
	}
	if n := entered.Load(); n != 0 {
		t.Fatalf("fn was called %d times after the step was cancelled, want 0", n)
	}
	got, err := mustJournal(store.MemStore).Step(context.Background(), "r1", "reserve", reserve)
	if err != nil || got != "reserved" || ran.Load() != 1 {
		t.Fatalf("second attempt = (%q, %v) with fn run %d times, want (\"reserved\", nil) and exactly 1", got, err, ran.Load())
	}
}

// A retry-safe Step that finds an earlier attempt marker (the step was a side effect then) halts
// on it, unless that attempt is recorded as not started: then there is nothing to halt for.
func TestStep_RetrySafeAfterNotStartedRuns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &markerHookStore{MemStore: NewMemStore(), cancel: cancel}
	j := mustJournal(store)
	var entered, ran atomic.Int32
	reserve := reserveFn(&entered, &ran)
	_, _ = j.Step(ctx, "r1", "reserve", reserve)
	got, err := mustJournal(store.MemStore).Step(context.Background(), "r1", "reserve", reserve, WithSafety(Safety{Idempotent: true}))
	if err != nil || got != "reserved" || ran.Load() != 1 {
		t.Fatalf("retry-safe step after an attempt that never started = (%q, %v), fn run %d times; want (\"reserved\", nil) and exactly 1", got, err, ran.Load())
	}
}

// Drivers that re-attempt a step recorded as not started claim the new attempt exclusively: fn
// runs once however many of them race.
func TestStep_ReattemptIsClaimedOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &markerHookStore{MemStore: NewMemStore(), cancel: cancel}
	j := mustJournal(store)
	var entered, ran atomic.Int32
	reserve := reserveFn(&entered, &ran)
	_, _ = j.Step(ctx, "r1", "reserve", reserve)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := mustJournal(store.MemStore).Step(context.Background(), "r1", "reserve", reserve)
			var halt *OutcomeUnknown
			if err != nil && !errors.As(err, &halt) {
				t.Errorf("a racing re-attempt failed with %v, want the result or a OutcomeUnknown", err)
			}
		}()
	}
	wg.Wait()
	if n := ran.Load(); n != 1 {
		t.Fatalf("fn ran %d times across racing re-attempts, want exactly 1", n)
	}
	if got, err := mustJournal(store.MemStore).Step(context.Background(), "r1", "reserve", reserve); err != nil || got != "reserved" {
		t.Fatalf("the step after the race = (%q, %v), want its recorded result", got, err)
	}
}

// reserveFn is a step's side effect that, like a well-behaved client, does nothing once its
// context is cancelled. entered counts every call, ran every reservation made.
func reserveFn(entered, ran *atomic.Int32) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		entered.Add(1)
		if err := ctx.Err(); err != nil {
			return "", err
		}
		ran.Add(1)
		return "reserved", nil
	}
}
