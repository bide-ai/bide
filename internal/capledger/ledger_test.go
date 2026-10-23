package capledger

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReserve_SlotsAreDenseAndCarryCumAfter(t *testing.T) {
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	var run string
	for i := range 5 {
		o := mustReserve(t, l, req(fmt.Sprintf("a%d", i), 10, 100))
		if o.Slot.N != i || o.Slot.CumAfter != int64(10*(i+1)) || o.Resumed {
			t.Fatalf("reservation %d: slot %+v resumed %v", i, o.Slot, o.Resumed)
		}
		run = o.Ledger
	}
	checkShape(t, slotsOf(t, st, run), 100)
	if !strings.HasPrefix(run, RunPrefix) || strings.Count(run, "/") != 5 {
		t.Fatalf("ledger run %q: want @cap/ and five segments", run)
	}
	if w := "2026-10-22"; !strings.Contains(run, "/"+w+"/") {
		t.Fatalf("ledger run %q lacks window %s", run, w)
	}
}

func TestReserve_OverLimitWritesNoSlotAndWaiverWritesOne(t *testing.T) {
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	mustReserve(t, l, req("a1", 70, 100))
	before := st.total()
	_, err := l.Reserve(context.Background(), req("a2", 40, 100))
	var ol *OverLimitError
	if !errors.Is(err, ErrOverLimit) || !errors.As(err, &ol) || ol.Cum != 70 || ol.Amount != 40 || ol.Limit != 100 {
		t.Fatalf("over the limit: err = %v", err)
	}
	// The refused call wrote its cap-intent and the index entry, never a slot.
	slots := slotsOf(t, st, New(Config{Store: st, Encode: testEncode}).LedgerRun(dayRule(), "2026-10-22", KeyDigest("")))
	if len(slots) != 1 {
		t.Fatalf("refused reservation left %d slots, want 1 (total entries %d, before %d)", len(slots), st.total(), before)
	}
	w := req("a3", 40, 100)
	w.Waiver = true
	o := mustReserve(t, l, w)
	if !o.Slot.Over || o.Slot.CumAfter != 110 {
		t.Fatalf("waived slot = %+v, want over with cum_after 110", o.Slot)
	}
	checkShape(t, slotsOf(t, st, o.Ledger), 100)
	// A later reservation sees the window over its limit and is refused again.
	if _, err := l.Reserve(context.Background(), req("a4", 1, 100)); !errors.Is(err, ErrOverLimit) {
		t.Fatalf("after a waived slot: err = %v, want ErrOverLimit", err)
	}
}

func TestReserve_TotalOverflowIsARefusalNotAWrap(t *testing.T) {
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	mustReserve(t, l, req("a1", math.MaxInt64-5, math.MaxInt64))
	_, err := l.Reserve(context.Background(), req("a2", 10, math.MaxInt64))
	if err == nil || errors.Is(err, ErrOverLimit) {
		t.Fatalf("overflowing total: err = %v, want an overflow error", err)
	}
}

// C3: the same attempt reserves once, under a retry, a second driver and a resume.
func TestReserve_SameAttemptReservesOnce(t *testing.T) {
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	first := mustReserve(t, l, req("a1", 10, 100))
	again := mustReserve(t, l, req("a1", 10, 100))
	other := mustReserve(t, newTestLedger(st, clk), req("a1", 10, 100)) // another process
	if first.Slot.N != again.Slot.N || first.Slot.N != other.Slot.N || !again.Resumed || !other.Resumed {
		t.Fatalf("slots %d %d %d, resumed %v %v: want one slot found again", first.Slot.N, again.Slot.N, other.Slot.N, again.Resumed, other.Resumed)
	}
	checkShape(t, slotsOf(t, st, first.Ledger), 100)
	if n := len(slotsOf(t, st, first.Ledger)); n != 1 {
		t.Fatalf("%d slots, want 1", n)
	}
}

// C2: N goroutines reserve against one window: the total never exceeds the limit, and exactly the
// reservations that fit succeed.
func TestReserve_ConcurrentGoroutinesNeverOvershoot(t *testing.T) {
	st, clk := newMemStore(), &testClock{t: day1}
	const n, limit = 64, 20
	var wg sync.WaitGroup
	var ok, refused atomic.Int32
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := newTestLedger(st, clk) // each its own process: no shared cache
			_, err := l.Reserve(context.Background(), req(fmt.Sprintf("a%d", i), 1, limit))
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrOverLimit):
				refused.Add(1)
			default:
				t.Errorf("a%d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != limit || refused.Load() != n-limit {
		t.Fatalf("%d reserved and %d refused, want %d and %d", ok.Load(), refused.Load(), limit, n-limit)
	}
	run := New(Config{Store: st, Encode: testEncode}).LedgerRun(dayRule(), "2026-10-22", KeyDigest(""))
	slots := slotsOf(t, st, run)
	checkShape(t, slots, limit)
	if len(slots) != limit {
		t.Fatalf("%d slots, want %d", len(slots), limit)
	}
}

func TestReserve_ConcurrentSameAttemptHoldsOneSlot(t *testing.T) {
	st, clk := newMemStore(), &testClock{t: day1}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := newTestLedger(st, clk).Reserve(context.Background(), req("a1", 5, 100)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	run := New(Config{Store: st, Encode: testEncode}).LedgerRun(dayRule(), "2026-10-22", KeyDigest(""))
	if s := slotsOf(t, st, run); len(s) != 1 {
		t.Fatalf("%d slots for one attempt, want 1", len(s))
	}
}

// C4: a crash at every round trip of a reservation, before the operation or after it took effect with
// its reply lost, then a fresh process reserves the same attempt: the window counts the attempt once
// whatever the crash point was, and the ledger keeps its shape.
func TestReserve_CrashAtEveryStep(t *testing.T) {
	steps := 0
	{ // how many round trips a clean reservation takes
		st, clk := newMemStore(), &testClock{t: day1}
		fs := &faultStore{inner: st}
		mustReserve(t, newTestLedger(fs, clk), req("a1", 10, 100))
		steps = fs.n
	}
	if steps < 6 {
		t.Fatalf("a reservation took %d round trips; the crash sweep expects the intent, the index, the slot and the audit record", steps)
	}
	for at := 1; at <= steps; at++ {
		for _, after := range []bool{false, true} {
			t.Run(fmt.Sprintf("crash-%d-after-%v", at, after), func(t *testing.T) {
				st, clk := newMemStore(), &testClock{t: day1}
				other := newTestLedger(st, clk)
				mustReserve(t, other, req("b1", 10, 100)) // a neighbour already holds a slot
				fs := &faultStore{inner: st, failAt: at, after: after}
				if _, err := newTestLedger(fs, clk).Reserve(context.Background(), req("a1", 10, 100)); err == nil {
					t.Fatal("the crashed reservation returned no error")
				}
				// The fresh process finds the same ledger through the intent, whether or not a slot was written.
				o := mustReserve(t, newTestLedger(st, clk), req("a1", 10, 100))
				slots := slotsOf(t, st, o.Ledger)
				checkShape(t, slots, 100)
				live := 0
				for _, s := range slots {
					if s.Kind == kindReserve && s.Attempt == "a1" {
						live++
					}
				}
				if live != 1 || o.Slot.CumAfter < 20 {
					t.Fatalf("a1 holds %d slots, cum_after %d: want one slot after the neighbour", live, o.Slot.CumAfter)
				}
			})
		}
	}
}

// C5b and C8: a retry after the window rolled over reads the window from the cap-intent, so it finds the
// same ledger and the same slot; a replay (a fresh store) starts its own ledger.
func TestIntent_FixesTheWindowAcrossMidnight(t *testing.T) {
	st, clk := newMemStore(), &testClock{t: day1}
	fs := &faultStore{inner: st}
	l := newTestLedger(fs, clk)
	o1 := mustReserve(t, l, req("a1", 10, 100))
	clk.Set(day1.Add(24 * time.Hour)) // the window rolled over
	o2 := mustReserve(t, newTestLedger(st, clk), req("a1", 10, 100))
	if o2.Ledger != o1.Ledger || o2.Slot.N != o1.Slot.N || !o2.Resumed {
		t.Fatalf("the retry after midnight reserved in %s slot %d, want %s slot %d", o2.Ledger, o2.Slot.N, o1.Ledger, o1.Slot.N)
	}
	// A new attempt on the new day gets the new window, and the index holds both.
	o3 := mustReserve(t, newTestLedger(st, clk), req("a2", 10, 100))
	if !strings.Contains(o3.Ledger, "/2026-10-23/") || o3.Slot.CumAfter != 10 {
		t.Fatalf("a new attempt after midnight: %s %+v", o3.Ledger, o3.Slot)
	}
	// A replay on a fresh store reserves in a fresh ledger with its own intent.
	st2 := newMemStore()
	o4 := mustReserve(t, newTestLedger(st2, clk), req("a1", 10, 100))
	if !strings.Contains(o4.Ledger, "/2026-10-23/") {
		t.Fatalf("a replay reserved in %s, want the current window", o4.Ledger)
	}
}

// C5b: a crash between the slot insert and any later record, then a retry after midnight, finds the slot.
func TestIntent_CrashAfterSlotThenRetryAfterMidnight(t *testing.T) {
	st, clk := newMemStore(), &testClock{t: day1}
	// Fail the audit record (the last round trip): the slot is in, the call has not been recorded.
	clean := &faultStore{inner: newMemStore()}
	mustReserve(t, newTestLedger(clean, &testClock{t: day1}), req("a1", 10, 100))
	fs := &faultStore{inner: st, failAt: clean.n}
	if _, err := newTestLedger(fs, clk).Reserve(context.Background(), req("a1", 10, 100)); err == nil {
		t.Fatal("the crashed reservation returned no error")
	}
	clk.Set(day1.Add(36 * time.Hour))
	o := mustReserve(t, newTestLedger(st, clk), req("a1", 10, 100))
	if !o.Resumed || !strings.Contains(o.Ledger, "/2026-10-22/") || o.Slot.N != 0 {
		t.Fatalf("retry = %+v in %s, want slot 0 of the first day's ledger", o.Slot, o.Ledger)
	}
}

// C5d: the clock bound, at a first intent only.
func TestClockBound(t *testing.T) {
	ctx := context.Background()
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	mustReserve(t, l, req("a1", 1, 100)) // index: 2026-10-22

	clk.Set(day1.Add(-24 * time.Hour)) // a driver a window behind
	if _, err := l.Reserve(ctx, req("a2", 1, 100)); !errors.Is(err, ErrClock) || !strings.Contains(err.Error(), "2026-10-21") || !strings.Contains(err.Error(), "2026-10-22") {
		t.Fatalf("an earlier window: err = %v, want ErrClock naming both windows", err)
	}
	clk.Set(day1.Add(48 * time.Hour)) // two windows ahead, bound one
	if _, err := l.Reserve(ctx, req("a3", 1, 100)); !errors.Is(err, ErrClock) {
		t.Fatalf("two windows later: err = %v, want ErrClock", err)
	}
	clk.Set(day1.Add(24 * time.Hour)) // one window ahead: within the bound
	mustReserve(t, l, req("a4", 1, 100))

	// A resume reads its window from its intent and is never checked again, though the clock is far off.
	clk.Set(day1.Add(24 * 30 * time.Hour))
	o := mustReserve(t, newTestLedger(st, clk), req("a1", 1, 100))
	if !o.Resumed || !strings.Contains(o.Ledger, "/2026-10-22/") {
		t.Fatalf("a resume far in the future: %+v in %s", o.Slot, o.Ledger)
	}
	// A refused first intent writes no intent, so its attempt is not stuck in the bad window.
	clk.Set(day1.Add(24 * time.Hour))
	mustReserve(t, l, req("a2", 1, 100))
}

// The index entry is written before the intent: a crash between them leaves a window with no intent,
// which costs nothing.
func TestClockBound_IndexEntryBeforeIntent(t *testing.T) {
	st, clk := newMemStore(), &testClock{t: day1}
	// Round trips of a first reservation: intent Get, index Scan, index Insert, intent Insert, ...
	fs := &faultStore{inner: st, failAt: 4}
	if _, err := newTestLedger(fs, clk).Reserve(context.Background(), req("a1", 1, 100)); err == nil {
		t.Fatal("expected the crash")
	}
	rule := dayRule()
	idx := New(Config{Store: st, Encode: testEncode}).IndexRun(rule)
	if got := st.names(idx); len(got) != 1 || got[0] != "win:2026-10-22" {
		t.Fatalf("index after the crash = %v, want win:2026-10-22", got)
	}
	if _, ok, _ := st.Get(context.Background(), "run-a1", intentPrefix+"a1"); ok {
		t.Fatal("the crashed first intent wrote an intent")
	}
	mustReserve(t, newTestLedger(st, clk), req("a1", 1, 100))
}

func TestRunWindowHasNoClockAndNoIndex(t *testing.T) {
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	r := req("a1", 5, 10)
	r.Rule.Kind = Run
	r.RootRunID = "root-7"
	o := mustReserve(t, l, r)
	if !strings.Contains(o.Ledger, "/root-7/") {
		t.Fatalf("run window ledger %q lacks the root run id", o.Ledger)
	}
	if got := st.names(l.IndexRun(r.Rule)); len(got) != 0 {
		t.Fatalf("a run window wrote index entries %v", got)
	}
	clk.Set(day1.Add(1000 * time.Hour))
	if _, err := l.Reserve(context.Background(), req("a2", 5, 10)); err != nil {
		// a2 is a Day rule: the far clock is a first intent in its own rule, which has an empty index
		t.Fatalf("an unrelated rule: %v", err)
	}
}

func TestIntentMismatchFailsClosed(t *testing.T) {
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	mustReserve(t, l, req("a1", 10, 100))
	if _, err := l.Reserve(context.Background(), req("a1", 99, 100)); !errors.Is(err, ErrIntentMismatch) {
		t.Fatalf("a changed amount: err = %v, want ErrIntentMismatch", err)
	}
}

// C6: a store that fails means the capped call does not run: Reserve returns the error whichever round
// trip fails, and no slot is written by a reservation that failed before its slot insert.
func TestReserve_FailsClosedOnStoreErrors(t *testing.T) {
	if _, err := New(Config{Store: failStore{}, Encode: testEncode, Clock: time.Now}).Reserve(context.Background(), req("a1", 1, 10)); !errors.Is(err, errCrash) {
		t.Fatalf("a store that is down: err = %v", err)
	}
	clean := &faultStore{inner: newMemStore()}
	mustReserve(t, newTestLedger(clean, &testClock{t: day1}), req("a1", 1, 10))
	for at := 1; at <= len(clean.log); at++ {
		st, clk := newMemStore(), &testClock{t: day1}
		fs := &faultStore{inner: st, failAt: at}
		if _, err := newTestLedger(fs, clk).Reserve(context.Background(), req("a1", 1, 10)); !errors.Is(err, errCrash) {
			t.Fatalf("round trip %d (%s) failing: Reserve returned %v, want the store's error", at, clean.log[at-1], err)
		}
		wroteSlot := false
		for _, op := range clean.log[:at-1] {
			wroteSlot = wroteSlot || strings.HasPrefix(op, "insert "+slotPrefix)
		}
		for run := range st.runs {
			for _, n := range st.names(run) {
				if strings.HasPrefix(n, slotPrefix) && !wroteSlot {
					t.Fatalf("round trip %d (%s) failing left %s in %s", at, clean.log[at-1], n, run)
				}
			}
		}
	}
}

func TestLedgerThatBreaksItsShapeFailsClosed(t *testing.T) {
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	o := mustReserve(t, l, req("a1", 1, 100))
	// A writer that is not this package leaves a gap: slot 2 with no slot 1.
	_, _, _ = st.Insert(context.Background(), o.Ledger, "slot:2", []byte(`{"n":2,"kind":"reserve","attempt":"x","amount":1,"cum_after":2}`))
	if _, err := newTestLedger(st, clk).Reserve(context.Background(), req("a2", 1, 100)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a gap: err = %v, want ErrCorrupt", err)
	}
}

func TestContentionBoundFailsClosed(t *testing.T) {
	st, clk := newMemStore(), &testClock{t: day1}
	l := New(Config{Store: &alwaysLoses{memStore: st}, Encode: testEncode, Clock: clk.Now, MaxRetries: 5})
	if _, err := l.Reserve(context.Background(), req("a1", 1, 100)); !errors.Is(err, ErrContention) {
		t.Fatalf("err = %v, want ErrContention", err)
	}
}

// alwaysLoses reports every slot insert as lost to another writer, without storing it.
type alwaysLoses struct{ *memStore }

func (a *alwaysLoses) Insert(ctx context.Context, run, name string, data []byte) ([]byte, bool, error) {
	if strings.HasPrefix(name, slotPrefix) {
		return data, false, nil
	}
	return a.memStore.Insert(ctx, run, name, data)
}

// scanFails is a store whose reads of the ledger runs fail while its writes work: a reservation that
// cannot read the ledger must not guess the total and insert.
type scanFails struct{ *memStore }

func (s scanFails) Scan(ctx context.Context, run string, after int64) iter.Seq2[Item, error] {
	if strings.Count(run, "/") == 5 {
		return func(yield func(Item, error) bool) { yield(Item{}, errCrash) }
	}
	return s.memStore.Scan(ctx, run, after)
}

func TestReserve_UnreadableLedgerFailsClosedWithoutWriting(t *testing.T) {
	st, clk := newMemStore(), &testClock{t: day1}
	l := New(Config{Store: scanFails{st}, Encode: testEncode, Clock: clk.Now})
	if _, err := l.Reserve(context.Background(), req("a1", 1, 10)); !errors.Is(err, errCrash) {
		t.Fatalf("an unreadable ledger: err = %v, want the store's error", err)
	}
	for run := range st.runs {
		for _, n := range st.names(run) {
			if strings.HasPrefix(n, slotPrefix) {
				t.Fatalf("an unreadable ledger still got %s in %s", n, run)
			}
		}
	}
}
