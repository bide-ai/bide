package capledger

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"testing"
)

// A request the ledger refuses reads and writes nothing, and cannot be turned into room: a negative
// amount against a window holding 90 must not let 90 more in.
func TestReserve_RefusesInvalidRequests(t *testing.T) {
	ctx := context.Background()
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	o := mustReserve(t, l, req("a0", 90, 100))
	before := st.total()
	set := func(f func(*Request)) Request { r := req("x", 1, 100); f(&r); return r }
	for name, r := range map[string]Request{
		"zero amount":         set(func(r *Request) { r.Amount = 0 }),
		"negative amount":     set(func(r *Request) { r.Amount = -80 }),
		"min int64":           set(func(r *Request) { r.Amount = math.MinInt64 }),
		"negative limit":      set(func(r *Request) { r.Limit = -1 }),
		"lower-case currency": set(func(r *Request) { r.Rule.Currency = "usd" }),
		"short currency":      set(func(r *Request) { r.Rule.Currency = "US" }),
		"long currency":       set(func(r *Request) { r.Rule.Currency = "USDX" }),
		"padded currency":     set(func(r *Request) { r.Rule.Currency = " USD" }),
		"non-ASCII currency":  set(func(r *Request) { r.Rule.Currency = "USĐ" }),
		"empty currency":      set(func(r *Request) { r.Rule.Currency = "" }),
		"empty namespace":     set(func(r *Request) { r.Rule.Namespace = "" }),
		"empty rule":          set(func(r *Request) { r.Rule.Name = "" }),
		"empty tool-use ID":   set(func(r *Request) { r.ToolUseID = "" }),
		"empty attempt ID":    set(func(r *Request) { r.AttemptID = "" }),
		"empty run":           set(func(r *Request) { r.RunID = "" }),
		"empty root run":      set(func(r *Request) { r.RootRunID = "" }),
		"unknown kind":        set(func(r *Request) { r.Rule.Kind = "week" }),
		"run window, no root": set(func(r *Request) { r.Rule.Kind = Run; r.RootRunID = "" }),
	} {
		if _, err := l.Reserve(ctx, r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if st.total() != before {
		t.Fatalf("refused requests wrote %d entries", st.total()-before)
	}
	// The window still has 10 of room, not 180.
	if _, err := l.Reserve(ctx, req("a1", 90, 100)); !errors.Is(err, ErrOverLimit) {
		t.Fatalf("after the refused negative amount: err = %v, want ErrOverLimit", err)
	}
	checkShape(t, slotsOf(t, st, o.Ledger), 100)
}

// The limit is inclusive: exactly at it is allowed, one over is refused.
func TestReserve_ExactlyAtTheLimitIsAllowedOneOverIsRefused(t *testing.T) {
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	mustReserve(t, l, req("a1", 6, 10))
	if o := mustReserve(t, l, req("a2", 4, 10)); o.Slot.CumAfter != 10 || o.Slot.Over {
		t.Fatalf("a reservation reaching the limit exactly = %+v, want cum_after 10 and not over", o.Slot)
	}
	if _, err := l.Reserve(context.Background(), req("a3", 1, 10)); !errors.Is(err, ErrOverLimit) {
		t.Fatalf("one over the limit: err = %v, want ErrOverLimit", err)
	}
	if o := mustReserve(t, newTestLedger(newMemStore(), clk), req("b1", 10, 10)); o.Slot.CumAfter != 10 {
		t.Fatalf("one reservation equal to the limit = %+v", o.Slot)
	}
}

// Attempt identity: the three parts are encoded and joined by the ledger, so no choice of them makes one
// attempt's key another's.
func TestAttemptKey_NoTwoDistinctTriplesCollide(t *testing.T) {
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	triples := [][3]string{
		{"a/b", "c", "d"}, {"a", "b/c", "d"}, {"a", "b", "c/d"}, {"a/b/c", "d", "e"}, {"a", "b", "c"},
		{"a:b", "c", "d"}, {"a", "b:c", "d"}, {"a%2Fb", "c", "d"}, {"a", "b", "c%2Fd"},
	}
	keys := map[string]bool{}
	for i, tr := range triples {
		k := l.AttemptKey(tr[0], tr[1], tr[2])
		if keys[k] {
			t.Fatalf("triple %v shares the key %q", tr, k)
		}
		keys[k] = true
		r := req("x", 1, 100)
		r.RootRunID, r.ToolUseID, r.AttemptID, r.RunID = tr[0], tr[1], tr[2], fmt.Sprintf("run-%d", i)
		if o := mustReserve(t, l, r); o.Resumed {
			t.Fatalf("triple %v resumed another attempt's slot", tr)
		}
	}
	run := l.LedgerRun(dayRule(), "2026-10-22", KeyDigest(""))
	if live, _ := l.Live(context.Background(), run); len(live) != len(triples) {
		t.Fatalf("%d live slots for %d distinct attempts", len(live), len(triples))
	}
}

// A slot a writer other than this package planted cannot move the window's total: read refuses the
// ledger (ErrCorrupt) on each way a slot can be wrong.
func TestRead_RefusesForeignSlots(t *testing.T) {
	good := func(t *testing.T) (*memStore, *Ledger, string, Slot) {
		st, clk := newMemStore(), &testClock{t: day1}
		l := newTestLedger(st, clk)
		o := mustReserve(t, l, req("a1", 40, 100))
		return st, l, o.Ledger, o.Slot
	}
	rel := func(s Slot) Slot {
		return Slot{N: 1, Kind: kindRelease, Attempt: s.Attempt, Amount: s.Amount, Currency: "USD", Ref: 0, CumAfter: 0, Limit: 100, Marked: true}
	}
	reserve2 := func(s Slot) Slot {
		return Slot{N: 1, Kind: kindReserve, Attempt: "root/a2/1", Amount: 10, Currency: "USD", CumAfter: 50, Limit: 100, Marked: true}
	}
	cases := map[string]func(s Slot) any{
		"a valid release under another kind": func(s Slot) any { x := rel(s); x.Kind = "void"; return x },
		"unknown kind resets the total":      func(s Slot) any { x := reserve2(s); x.Kind = "bogus"; x.CumAfter = 0; return x },
		"wrong cum_after":                    func(s Slot) any { x := reserve2(s); x.CumAfter = 0; return x },
		"zero amount":                        func(s Slot) any { x := reserve2(s); x.Amount = 0; x.CumAfter = 40; return x },
		"negative amount":                    func(s Slot) any { x := reserve2(s); x.Amount = -30; x.CumAfter = 10; return x },
		"another currency":                   func(s Slot) any { x := reserve2(s); x.Currency = "EUR"; return x },
		"lower-case currency":                func(s Slot) any { x := reserve2(s); x.Currency = "usd"; return x },
		"empty attempt":                      func(s Slot) any { x := reserve2(s); x.Attempt = ""; return x },
		"over flag against the limit":        func(s Slot) any { x := reserve2(s); x.Over = true; return x },
		"a reserve that carries a ref":       func(s Slot) any { x := reserve2(s); x.Ref = 1; return x },
		"slot number out of order":           func(s Slot) any { x := reserve2(s); x.N = 2; return x },
		"release of no slot":                 func(s Slot) any { x := rel(s); x.Ref = 7; return x },
		"release of itself":                  func(s Slot) any { x := rel(s); x.Ref = 1; return x },
		"release of another attempt":         func(s Slot) any { x := rel(s); x.Attempt = "root/other/1"; return x },
		"release of another amount":          func(s Slot) any { x := rel(s); x.Amount = 5; x.CumAfter = 35; return x },
		"release of an unmarked":             func(s Slot) any { x := rel(s); x.Marked = false; return x },
		"release with the wrong total":       func(s Slot) any { x := rel(s); x.CumAfter = 40; return x },
		"release that carries the over":      func(s Slot) any { x := rel(s); x.Over = true; return x },
		"unknown member": func(s Slot) any {
			return `{"n":1,"kind":"reserve","attempt":"x","amount":1,"currency":"USD","cum_after":41,"limit":100,"extra":1}`
		},
		"data after the record": func(s Slot) any {
			return `{"n":1,"kind":"reserve","attempt":"x","amount":1,"currency":"USD","cum_after":41,"limit":100} {}`
		},
		"not JSON": func(s Slot) any { return `[]` },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			st, l, run, s := good(t)
			var data []byte
			switch v := mk(s).(type) {
			case Slot:
				data, _ = encodeSlot(v)
			case string:
				data = []byte(v)
			}
			st.Insert(context.Background(), run, "slot:"+fmt.Sprint(slotNumber(data)), data)
			before := len(st.names(run))
			if _, err := newTestLedger(st, &testClock{t: day1}).Reserve(context.Background(), req("a9", 1, 100)); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("a reservation over a ledger holding %s: err = %v, want ErrCorrupt", data, err)
			}
			if n := len(st.names(run)); n != before {
				t.Fatalf("the refused reservation wrote %d entries to the ledger", n-before)
			}
			if _, err := l.Live(context.Background(), run); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Live over the same ledger: err = %v, want ErrCorrupt", err)
			}
		})
	}
}

// slotNumber is the n a planted slot claims, for naming it (2 for the out-of-order case, else 1).
func slotNumber(data []byte) int {
	if strings.Contains(string(data), `"n":2`) {
		return 2
	}
	return 1
}

// A duplicate release (a second release of one slot) is refused too.
func TestRead_RefusesADuplicateRelease(t *testing.T) {
	ctx := context.Background()
	st, clk := newMemStore(), &testClock{t: day1}
	l := newTestLedger(st, clk)
	o := mustReserve(t, l, req("a1", 40, 100))
	var w atomic.Int32
	seal, _ := sealOf("a1", &w)
	if _, err := l.Release(ctx, tgt("a1"), seal); err != nil {
		t.Fatal(err)
	}
	dup, _ := encodeSlot(Slot{N: 2, Kind: kindRelease, Attempt: ak("a1"), Amount: 40, Currency: "USD", Ref: 0, CumAfter: -40, Limit: 100, Marked: true})
	st.Insert(ctx, o.Ledger, "slot:2", dup)
	if _, err := newTestLedger(st, clk).Reserve(ctx, req("a2", 1, 100)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

// The view cache is bounded: reading many ledgers keeps at most MaxViews, and a dropped view is read again.
func TestViewCacheIsBounded(t *testing.T) {
	ctx := context.Background()
	st, clk := newMemStore(), &testClock{t: day1}
	l := New(Config{Store: st, Encode: testEncode, Clock: clk.Now, MaxViews: 4})
	var runs []string
	for i := range 40 {
		r := req(fmt.Sprintf("a%d", i), 1, 100)
		r.KeyDigest = KeyDigest(fmt.Sprintf("customer-%d", i)) // a ledger of its own per key
		runs = append(runs, mustReserve(t, l, r).Ledger)
		if len(l.views) > 4 || len(l.lru) > 4 {
			t.Fatalf("after %d ledgers the cache holds %d views (lru %d), want at most 4", i+1, len(l.views), len(l.lru))
		}
	}
	// The first ledger's view was dropped: reading it again loads it from the store.
	live, err := l.Live(ctx, runs[0])
	if err != nil || len(live) != 1 || live[0].Tool != "a0" {
		t.Fatalf("Live of an evicted ledger = %+v, %v", live, err)
	}
	// And a reservation on an evicted ledger sees what it holds.
	r := req("a0", 1, 100)
	r.KeyDigest = KeyDigest("customer-0")
	if o := mustReserve(t, l, r); !o.Resumed {
		t.Fatalf("a retry on an evicted ledger reserved again: %+v", o)
	}
}
