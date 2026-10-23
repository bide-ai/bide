package capledger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A call's began word, as the engine has it: none, begun or sealed.
const (
	wordNone int32 = iota
	wordBegun
	wordSealed
)

// tgt is the Target of req(attempt, ...).
func tgt(attempt string) Target {
	return Target{Rule: dayRule(), KeyDigest: KeyDigest(""), RunID: "run-" + attempt, RootRunID: "root", ToolUseID: attempt, AttemptID: "1"}
}

// sealOf seals attempt's began word, as the loop does once the chain returned.
func sealOf(attempt string, w *atomic.Int32) (Seal, bool) {
	return NewSeal(ak(attempt), w, wordNone, wordSealed)
}

func TestRelease_NeedsASeal(t *testing.T) {
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	o := mustReserve(t, l, req("a1", 10, 100))
	if _, err := l.Release(context.Background(), tgt("a1"), Seal{}); !errors.Is(err, ErrNotSealed) {
		t.Fatalf("a release without a seal: err = %v, want ErrNotSealed", err)
	}
	var w atomic.Int32
	for name, f := range map[string]func() (Seal, bool){
		"no attempt":     func() (Seal, bool) { return NewSeal("", &w, wordNone, wordSealed) },
		"no word":        func() (Seal, bool) { return NewSeal(ak("a1"), nil, wordNone, wordSealed) },
		"none == sealed": func() (Seal, bool) { return NewSeal(ak("a1"), &w, wordNone, wordNone) },
	} {
		if _, ok := f(); ok {
			t.Fatalf("NewSeal with %s made a seal", name)
		}
	}
	if w.Load() != wordNone {
		t.Fatalf("a refused NewSeal moved the word to %d", w.Load())
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
	if _, ok := sealOf("a1", &w); ok {
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
	seal, ok := sealOf("a1", &w)
	if !ok {
		t.Fatal("no seal for a call that did not begin")
	}
	released, err := l.Release(ctx, tgt("a1"), seal)
	if err != nil || !released {
		t.Fatalf("Release = %v, %v", released, err)
	}
	// The dedupe scan: a second release, from the same loop or another, writes nothing.
	again, err := newTestLedger(st, clk).Release(ctx, tgt("a1"), seal)
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
	seal, _ := sealOf("a1", &w)
	if _, err := l.Release(ctx, tgt("a1"), seal); !errors.Is(err, ErrUnmarked) {
		t.Fatalf("Release of an unmarked attempt: err = %v, want ErrUnmarked", err)
	}
	// Reconcile does not touch it either, voided or not.
	got, err := l.Reconcile(ctx, o.Ledger, func(context.Context, Slot) (bool, error) { return true, nil })
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
	got, err := l.Reconcile(ctx, run, func(_ context.Context, s Slot) (bool, error) { return voided[s.Tool], nil })
	if err != nil || len(got) != 2 || got[0] != ak("a1") || got[1] != ak("a3") {
		t.Fatalf("Reconcile = %v, %v, want a1 and a3", got, err)
	}
	checkShape(t, slotsOf(t, st, run), 100)
	live, _ := l.Live(ctx, run)
	if len(live) != 2 || live[0].Tool != "a0" || live[1].Tool != "a2" {
		t.Fatalf("live = %+v, want a0 and a2", live)
	}
	// Run again: nothing left to release.
	if got, err := l.Reconcile(ctx, run, func(_ context.Context, s Slot) (bool, error) { return voided[s.Tool], nil }); err != nil || len(got) != 0 {
		t.Fatalf("second Reconcile = %v, %v", got, err)
	}
	// A reader that fails fails the pass: nothing is guessed.
	if _, err := l.Reconcile(ctx, run, func(context.Context, Slot) (bool, error) { return false, errCrash }); !errors.Is(err, errCrash) {
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
			got, err := newTestLedger(st, clk).Reconcile(ctx, run, func(context.Context, Slot) (bool, error) { return true, nil })
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
	seal, _ := sealOf("a1", &w)
	if released, err := l.Release(ctx, tgt("a1"), seal); err != nil || released {
		t.Fatalf("Release with nothing reserved = %v, %v", released, err)
	}
	o := mustReserve(t, l, req("a1", 10, 100)) // the late reserve
	got, err := l.Reconcile(ctx, o.Ledger, func(context.Context, Slot) (bool, error) { return true, nil })
	if err != nil || len(got) != 1 {
		t.Fatalf("Reconcile = %v, %v, want the late slot released", got, err)
	}
	if live, _ := l.Live(ctx, o.Ledger); len(live) != 0 {
		t.Fatalf("live = %+v after reconcile", live)
	}
}

// A Seal is bound to its attempt: a copy of a1's seal releases nothing of a2's, and a seal that was never
// made (the zero Seal) releases nothing.
func TestRelease_SealIsBoundToItsAttempt(t *testing.T) {
	ctx := context.Background()
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	o1 := mustReserve(t, l, req("a1", 10, 100))
	mustReserve(t, l, req("a2", 10, 100))
	var w1 atomic.Int32
	seal1, ok := sealOf("a1", &w1)
	if !ok {
		t.Fatal("no seal")
	}
	copied := seal1 // a copy is the same proof for the same attempt, and for no other
	if _, err := l.Release(ctx, tgt("a2"), copied); !errors.Is(err, ErrNotSealed) {
		t.Fatalf("a1's seal released a2: err = %v, want ErrNotSealed", err)
	}
	if _, err := l.Release(ctx, tgt("a2"), Seal{}); !errors.Is(err, ErrNotSealed) {
		t.Fatalf("the zero Seal: err = %v, want ErrNotSealed", err)
	}
	live, _ := l.Live(ctx, o1.Ledger)
	if len(live) != 2 {
		t.Fatalf("%d live slots after the refused releases, want 2", len(live))
	}
	if ok, err := l.Release(ctx, tgt("a1"), copied); err != nil || !ok {
		t.Fatalf("the seal's own attempt: %v, %v", ok, err)
	}
	// A word that says the tool began yields no seal, however it is asked for: no forging from the call site's word.
	var begun atomic.Int32
	begun.Store(wordBegun)
	if _, ok := sealOf("a2", &begun); ok || begun.Load() != wordBegun {
		t.Fatalf("a seal was made from a word that says the tool began (word now %d)", begun.Load())
	}
}

// Release reads the window from the attempt's cap-intent: with the clock days later, and for a target that
// names no intent, it releases in the right ledger, or nothing.
func TestRelease_ReadsTheWindowFromTheIntent(t *testing.T) {
	ctx := context.Background()
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	o := mustReserve(t, l, req("a1", 10, 100))
	clk.Set(day1.Add(72 * time.Hour))
	var w atomic.Int32
	seal, _ := sealOf("a1", &w)
	if ok, err := newTestLedger(st, clk).Release(ctx, tgt("a1"), seal); err != nil || !ok {
		t.Fatalf("Release three days later = %v, %v", ok, err)
	}
	if live, _ := l.Live(ctx, o.Ledger); len(live) != 0 {
		t.Fatalf("the slot is still live in %s", o.Ledger)
	}
	// A target in another run is another attempt: a1's seal does not release it, and with a seal of its own
	// (no intent was written there) there is nothing to release.
	other := tgt("a1")
	other.RunID = "some-other-run"
	if _, err := l.Release(ctx, other, seal); !errors.Is(err, ErrNotSealed) {
		t.Fatalf("Release naming another run with a1's seal: err = %v, want ErrNotSealed", err)
	}
	var w2 atomic.Int32
	seal2, _ := NewSeal(l.AttemptKey(other.RunID, other.RootRunID, other.ToolUseID, other.AttemptID), &w2, wordNone, wordSealed)
	if ok, err := l.Release(ctx, other, seal2); err != nil || ok {
		t.Fatalf("Release naming a run with no intent = %v, %v, want false, nil", ok, err)
	}
	// A target that names another rule than the intent's is refused.
	for name, mutate := range map[string]func(*Target){
		"rule":     func(x *Target) { x.Rule.Name = "other_rule" },
		"kind":     func(x *Target) { x.Rule.Kind = Hour },
		"currency": func(x *Target) { x.Rule.Currency = "EUR" },
		"key":      func(x *Target) { x.KeyDigest = KeyDigest("another key") },
	} {
		wrong := tgt("a1")
		mutate(&wrong)
		if _, err := l.Release(ctx, wrong, seal); !errors.Is(err, ErrIntentMismatch) {
			t.Fatalf("Release naming another %s: err = %v, want ErrIntentMismatch", name, err)
		}
	}
}

// Reconcile refuses a live slot that sits in a ledger its cap-intent does not name: it is not released.
func TestReconcile_RefusesASlotOutsideItsIntentsLedger(t *testing.T) {
	ctx := context.Background()
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	o := mustReserve(t, l, req("a1", 10, 100))
	// The same slot, copied into another ledger of the rule (another key's) by a writer that is not this package.
	data, _, _ := st.Get(ctx, o.Ledger, "slot:0")
	other := l.LedgerRun(dayRule(), "2026-10-22", KeyDigest("another key"))
	st.Insert(ctx, other, "slot:0", data)
	_, err := l.Reconcile(ctx, other, func(context.Context, Slot) (bool, error) { return true, nil })
	if !errors.Is(err, ErrIntentMismatch) {
		t.Fatalf("Reconcile of a slot in another ledger: err = %v, want ErrIntentMismatch", err)
	}
	if live, _ := l.Live(ctx, other); len(live) != 1 {
		t.Fatalf("%d live slots in the other ledger, want the copy untouched", len(live))
	}
	// A live slot with no intent at all is corrupt.
	st2 := newMemStore()
	st2.Insert(ctx, other, "slot:0", data)
	if _, err := newTestLedger(st2, clk).Reconcile(ctx, other, func(context.Context, Slot) (bool, error) { return true, nil }); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Reconcile of a slot with no intent: err = %v, want ErrCorrupt", err)
	}
}

// raceStore lets a test act between a writer's read and its insert: before the first slot insert whose
// name is at, it runs hook (once), as another writer would have.
type raceStore struct {
	*memStore
	at   string
	hook func()
	once sync.Once
}

func (r *raceStore) Insert(ctx context.Context, run, name string, data []byte) ([]byte, bool, error) {
	if name == r.at {
		r.once.Do(r.hook)
	}
	return r.memStore.Insert(ctx, run, name, data)
}

// A release whose insert loses (a concurrent reserve took slot n+1 between its read and its insert) does
// not report success: it reads again and writes its release at the next name.
func TestRelease_LostInsertReadsAgainAndRetries(t *testing.T) {
	ctx := context.Background()
	st, clk := newMemStore(), &testClock{t: day1}
	rs := &raceStore{memStore: st, at: "slot:1"}
	l := New(Config{Store: rs, Encode: testEncode, Clock: clk.Now})
	o := mustReserve(t, l, req("a1", 10, 100))
	rs.hook = func() { mustReserve(t, newTestLedger(st, clk), req("a2", 5, 100)) } // takes slot 1 first
	var w atomic.Int32
	seal, _ := sealOf("a1", &w)
	released, err := l.Release(ctx, tgt("a1"), seal)
	if err != nil || !released {
		t.Fatalf("Release = %v, %v", released, err)
	}
	slots := slotsOf(t, st, o.Ledger)
	checkShape(t, slots, 100)
	if len(slots) != 3 || slots[1].Kind != kindReserve || slots[1].Tool != "a2" || slots[2].Kind != kindRelease || slots[2].Ref != 0 || slots[2].CumAfter != 5 {
		t.Fatalf("slots = %+v, want a1, a2 (the concurrent reserve), then the release of a1 with cum_after 5", slots)
	}
	if live, _ := l.Live(ctx, o.Ledger); len(live) != 1 || live[0].Tool != "a2" {
		t.Fatalf("live = %+v, want only a2", live)
	}
}

// WindowOf reads the window from the structured cap-intent of the slot's attempt, not from any name.
func TestWindowOf_ReadsTheIntent(t *testing.T) {
	ctx := context.Background()
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	o := mustReserve(t, l, req("a1", 10, 100))
	kind, window, ok, err := l.WindowOf(ctx, o.Slot)
	if err != nil || !ok || kind != Day || window != "2026-10-22" {
		t.Fatalf("WindowOf = %v %q %v %v, want day 2026-10-22", kind, window, ok, err)
	}
	r := req("a2", 10, 100)
	r.Rule.Kind, r.RootRunID = Run, "2026-07-15" // a root run named like a date
	o2 := mustReserve(t, l, r)
	if kind, window, ok, err = l.WindowOf(ctx, o2.Slot); err != nil || !ok || kind != Run || window != "2026-07-15" {
		t.Fatalf("WindowOf = %v %q %v %v, want run 2026-07-15", kind, window, ok, err)
	}
	other := o.Slot
	other.Run = "nowhere"
	if _, _, ok, err := l.WindowOf(ctx, other); ok || err != nil {
		t.Fatalf("WindowOf of a slot whose run holds no intent = %v, %v, want not found", ok, err)
	}
}
