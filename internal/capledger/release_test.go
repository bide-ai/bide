package capledger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// A call's began word, as the engine has it: none, begun or sealed.
const (
	wordNone int32 = iota
	wordBegun
	wordSealed
)

func sealOf(w *atomic.Int32) func() bool {
	return func() bool { return w.CompareAndSwap(wordNone, wordSealed) }
}

func TestRelease_NeedsASeal(t *testing.T) {
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	o := mustReserve(t, l, req("a1", 10, 100))
	if _, err := l.Release(context.Background(), o.Ledger, "a1", Seal{}); !errors.Is(err, ErrNotSealed) {
		t.Fatalf("a release without a seal: err = %v, want ErrNotSealed", err)
	}
	if _, ok := NewSeal(nil); ok {
		t.Fatal("NewSeal(nil) made a seal")
	}
	checkShape(t, slotsOf(t, st, o.Ledger), 100)
	if live, _ := l.Live(context.Background(), o.Ledger); len(live) != 1 {
		t.Fatalf("%d live slots, want 1", len(live))
	}
}

// A call that began is never sealed, so never released: the began word says so, whoever asks (the loop
// after a second invocation of the handler, a saga's rollback).
func TestRelease_NoSealForACallThatBegan(t *testing.T) {
	var w atomic.Int32
	w.Store(wordBegun)
	if _, ok := NewSeal(sealOf(&w)); ok {
		t.Fatal("a seal was made for a call that began")
	}
}

func TestRelease_OnceAndTheCapacityReturns(t *testing.T) {
	ctx := context.Background()
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	o := mustReserve(t, l, req("a1", 60, 100))
	if _, err := l.Reserve(ctx, req("a2", 60, 100)); !errors.Is(err, ErrOverLimit) {
		t.Fatalf("a2 before the release: %v", err)
	}
	var w atomic.Int32
	seal, ok := NewSeal(sealOf(&w))
	if !ok {
		t.Fatal("no seal for a call that did not begin")
	}
	released, err := l.Release(ctx, o.Ledger, "a1", seal)
	if err != nil || !released {
		t.Fatalf("Release = %v, %v", released, err)
	}
	// The dedupe scan: a second release, from the same loop or another, writes nothing.
	again, err := newTestLedger(st, clk).Release(ctx, o.Ledger, "a1", seal)
	if err != nil || again {
		t.Fatalf("second Release = %v, %v, want false, nil", again, err)
	}
	slots := slotsOf(t, st, o.Ledger)
	checkShape(t, slots, 100)
	if len(slots) != 2 || slots[1].Kind != kindRelease || slots[1].Ref != 0 || slots[1].CumAfter != 0 {
		t.Fatalf("slots = %+v, want a reserve then one release of slot 0 with cum_after 0", slots)
	}
	mustReserve(t, l, req("a2", 60, 100))
}

func TestRelease_NeverAnUnmarkedAttempt(t *testing.T) {
	ctx := context.Background()
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	r := req("a1", 10, 100)
	r.Marked = false
	o := mustReserve(t, l, r)
	var w atomic.Int32
	seal, _ := NewSeal(sealOf(&w))
	if _, err := l.Release(ctx, o.Ledger, "a1", seal); !errors.Is(err, ErrUnmarked) {
		t.Fatalf("Release of an unmarked attempt: err = %v, want ErrUnmarked", err)
	}
	// Reconcile does not touch it either, voided or not.
	got, err := l.Reconcile(ctx, o.Ledger, func(context.Context, string) (bool, error) { return true, nil })
	if err != nil || len(got) != 0 {
		t.Fatalf("Reconcile = %v, %v, want nothing released", got, err)
	}
	if live, _ := l.Live(ctx, o.Ledger); len(live) != 1 {
		t.Fatalf("%d live slots, want the unmarked one still counted", len(live))
	}
}

func TestReconcile_ReleasesVoidedMarkedAttemptsOnly(t *testing.T) {
	ctx := context.Background()
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	var run string
	for i := range 4 {
		run = mustReserve(t, l, req(fmt.Sprintf("a%d", i), 10, 100)).Ledger
	}
	voided := map[string]bool{"a1": true, "a3": true}
	got, err := l.Reconcile(ctx, run, func(_ context.Context, a string) (bool, error) { return voided[a], nil })
	if err != nil || len(got) != 2 || got[0] != "a1" || got[1] != "a3" {
		t.Fatalf("Reconcile = %v, %v, want a1 and a3", got, err)
	}
	checkShape(t, slotsOf(t, st, run), 100)
	live, _ := l.Live(ctx, run)
	if len(live) != 2 || live[0].Attempt != "a0" || live[1].Attempt != "a2" {
		t.Fatalf("live = %+v, want a0 and a2", live)
	}
	// Run again: nothing left to release.
	if got, err := l.Reconcile(ctx, run, func(_ context.Context, a string) (bool, error) { return voided[a], nil }); err != nil || len(got) != 0 {
		t.Fatalf("second Reconcile = %v, %v", got, err)
	}
	// A reader that fails fails the pass: nothing is guessed.
	if _, err := l.Reconcile(ctx, run, func(context.Context, string) (bool, error) { return false, errCrash }); !errors.Is(err, errCrash) {
		t.Fatalf("a failing voided: err = %v", err)
	}
}

// Two reconcile passes over one voided attempt race: the slot is released once.
func TestReconcile_ConcurrentPassesReleaseOnce(t *testing.T) {
	ctx := context.Background()
	st, clk := newMemStore(), &testClock{t: day1}
	run := mustReserve(t, newTestLedger(st, clk), req("a1", 10, 100)).Ledger
	var wg sync.WaitGroup
	var total atomic.Int32
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := newTestLedger(st, clk).Reconcile(ctx, run, func(context.Context, string) (bool, error) { return true, nil })
			if err != nil {
				t.Error(err)
			}
			total.Add(int32(len(got)))
		}()
	}
	wg.Wait()
	if total.Load() != 1 {
		t.Fatalf("%d releases reported, want 1", total.Load())
	}
	checkShape(t, slotsOf(t, st, run), 100)
}

// A handler still running when the loop sealed and released reserves afterwards: its slot is a leak
// the reconcile pass releases (model 17: the late reserve).
func TestReconcile_ReleasesALateReserve(t *testing.T) {
	ctx := context.Background()
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	// The loop sealed the word and found nothing to release: the handler had not reserved yet.
	var w atomic.Int32
	seal, _ := NewSeal(sealOf(&w))
	run := l.LedgerRun(dayRule(), "2026-10-22", KeyDigest(""))
	if released, err := l.Release(ctx, run, "a1", seal); err != nil || released {
		t.Fatalf("Release with nothing reserved = %v, %v", released, err)
	}
	o := mustReserve(t, l, req("a1", 10, 100)) // the late reserve
	got, err := l.Reconcile(ctx, o.Ledger, func(context.Context, string) (bool, error) { return true, nil })
	if err != nil || len(got) != 1 {
		t.Fatalf("Reconcile = %v, %v, want the late slot released", got, err)
	}
	if live, _ := l.Live(ctx, o.Ledger); len(live) != 0 {
		t.Fatalf("live = %+v after reconcile", live)
	}
}
