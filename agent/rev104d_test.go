package agent

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"sync/atomic"
	"testing"
)

// D1: pendingSpends.order is not bounded. A run whose kept spend is taken and kept again (a store
// that keeps failing the spend write, drive after drive) appends its key to order on every keep,
// and the compaction keeps every copy, since the run is held when it runs. The entry count stays
// at 1 while order grows without limit.
func TestRev104d_OrderGrowsUnbounded(t *testing.T) {
	defer func(n int) { maxPendingSpends = n }(maxPendingSpends)
	maxPendingSpends = 4
	a := mustNew(&scriptModel{}, memJournal())
	const cycles = 200
	for range cycles {
		a.keepSpend("r", pendingSpend{name: spendStep("x"), spent: billed})
		_ = a.takeSpend("r")
	}
	a.keepSpend("r", pendingSpend{name: spendStep("x"), spent: billed})
	pendingSpends.Lock()
	n, orderLen := pendingSpends.n, len(pendingSpends.order)
	pendingSpends.Unlock()
	_ = a.takeSpend("r")
	if orderLen > 2*maxPendingSpends+1 {
		t.Fatalf("entries held %d, but order holds %d keys (bound %d): order grows by one per take and keep", n, orderLen, 2*maxPendingSpends)
	}
}

// wrapStore is a Store wrapper (the shape of audit.AuditedStore: it passes run IDs and names
// through and implements Unwrap() Store) whose Load fails while failLoad is set, so reading a
// run's history through it fails.
type wrapStore struct {
	Store
	failLoad *atomic.Bool
}

// Unwrap returns the wrapped Store, as audit.AuditedStore does.
func (w *wrapStore) Unwrap() Store { return w.Store }

func (w *wrapStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	if w.failLoad.Load() {
		return func(yield func(Entry, error) bool) { yield(Entry{}, errors.New("injected: history read failed")) }
	}
	return w.Store.Load(ctx, runID, after)
}

// D2: kept spend through a store wrapper (audit.AuditedStore's shape) must be keyed by the
// identity of the store under it, not by the wrapper pointer. Were it keyed by the wrapper, the
// run's next drive through another wrapper over the same store (a wrapper and a Journal per
// request) would never see it, and the failed turn's billed spend would never be journaled.
func TestRev104d_KeptSpendOtherWrapperSameStore(t *testing.T) {
	st := newFaultStore()
	var fail atomic.Bool
	w1 := &wrapStore{Store: st, failLoad: &fail}
	st.mu.Lock()
	st.failNoCommit[modelStep(0)] = true
	st.mu.Unlock()
	arm := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			resp, err := next(ctx, call)
			fail.Store(true)
			return resp, err
		}
	}
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	if _, err := mustNew(m, mustJournal(w1), WithMiddleware(arm)).RunResult(context.Background(), "r", "go"); err == nil {
		t.Fatal("want the write failure")
	}
	fail.Store(false)
	w2 := &wrapStore{Store: st, failLoad: &fail}
	m2 := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	res, err := mustNew(m2, mustJournal(w2)).RunResult(context.Background(), "r", "go")
	if err != nil {
		t.Fatal(err)
	}
	pendingSpends.Lock()
	left := len(pendingSpends.m[pendingKey{w1, "r"}])
	delete(pendingSpends.m, pendingKey{w1, "r"})
	pendingSpends.n -= left
	pendingSpends.Unlock()
	if res.Spend != twice(billed) {
		t.Fatalf("Spend = %+v, want both requests' %+v; %d entries still kept under the first wrapper", res.Spend, twice(billed), left)
	}
}

// Control for the cross-PR check: ownRecord's no-salt fallback on content #103 cares about (HTML
// characters, escaped and literal U+2028, pre-escaped < in tool args, whitespace in args,
// invalid UTF-8 in text) takes the record read back in the stored form as the one built.
func TestRev104d_OwnRecordFallbackTrickyContent(t *testing.T) {
	texts := []string{"<a&b>", "line sep ", "bad \xff\xfe", `lit �`, "<"}
	args := []string{`{"q" : "<&>"}`, `{"q":"<&"}`, "{\"q\":\" \"}", `{"q":" "}`, `{ }`, `[1.0, -0, 1e400]`}
	for _, tx := range texts {
		for _, ar := range args {
			msg := Message{Role: RoleAssistant, Parts: []Part{Text{Text: tx}, ToolUse{ID: "c", Name: "t", Args: json.RawMessage(ar)}}}
			u := Usage{}
			built := Record{Kind: StepModel, Message: &msg, Usage: &u, ModelTurn: &ModelTurn{Finish: FinishToolUse}}
			if err := stampSalt(&built); err != nil {
				t.Fatal(err)
			}
			data, err := JournalEntry("@llm/0", built)
			if err != nil {
				t.Fatalf("%q %q: %v", tx, ar, err)
			}
			held, err := DecodeRecord(data)
			if err != nil {
				t.Fatal(err)
			}
			if !ownRecord(held, built) {
				t.Fatalf("salted: %q %q not own", tx, ar)
			}
			// A store that drops the salt and re-encodes.
			held.salt = nil
			b2, err := EncodeRecord(held)
			if err != nil {
				t.Fatal(err)
			}
			held2, _ := DecodeRecord(b2)
			if !ownRecord(held2, built) {
				t.Fatalf("no salt: %q %q not own:\n%s", tx, ar, b2)
			}
		}
	}
}

// Control for D2: the same wrapper on the next drive journals the kept spend.
func TestRev104d_KeptSpendSameWrapperControl(t *testing.T) {
	st := newFaultStore()
	var fail atomic.Bool
	w1 := mustJournal(&wrapStore{Store: st, failLoad: &fail})
	st.mu.Lock()
	st.failNoCommit[modelStep(0)] = true
	st.mu.Unlock()
	arm := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			resp, err := next(ctx, call)
			fail.Store(true)
			return resp, err
		}
	}
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	if _, err := mustNew(m, w1, WithMiddleware(arm)).RunResult(context.Background(), "r", "go"); err == nil {
		t.Fatal("want the write failure")
	}
	fail.Store(false)
	m2 := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	res, err := mustNew(m2, w1).RunResult(context.Background(), "r", "go")
	if err != nil {
		t.Fatal(err)
	}
	if res.Spend != twice(billed) {
		t.Fatalf("Spend = %+v, want %+v", res.Spend, twice(billed))
	}
}

// D2, claims: behind a store wrapper (audit.AuditedStore's shape) a claim whose marker write
// fails, and whose record that it did not start fails too, is remembered as through the store
// itself: the next drive in the process records that the attempt did not start and re-attempts
// the effect, rather than halting over an effect that never ran. The next drive goes through
// another wrapper and Journal over the same store.
func TestRev104d_ClaimBehindStoreWrapperIsRemembered(t *testing.T) {
	ctx := context.Background()
	st := newFaultStore()
	var fail atomic.Bool
	st.commitThenErrPrefix = "attempt:step:"
	st.failNoCommitPrefix = "attempt:not-started:"
	var runs int
	pay := func(context.Context) (string, error) { runs++; return "paid", nil }
	if _, err := Step(ctx, mustJournal(&wrapStore{Store: st, failLoad: &fail}), "r", "pay", pay); err == nil {
		t.Fatal("want the claim's write failure")
	}
	got, err := Step(ctx, mustJournal(&wrapStore{Store: st, failLoad: &fail}), "r", "pay", pay)
	if err != nil || got != "paid" || runs != 1 {
		t.Fatalf("second drive = %q, %v, effect ran %d times; want paid, nil, once (the first claim never ran it)", got, err, runs)
	}
}

// Control: the same through a Journal on the store itself.
func TestRev104d_ClaimThroughJournalIsRemembered(t *testing.T) {
	ctx := context.Background()
	st := newFaultStore()
	j, _ := NewJournal(st)
	st.commitThenErrPrefix = "attempt:step:"
	st.failNoCommitPrefix = "attempt:not-started:"
	var runs int
	pay := func(context.Context) (string, error) { runs++; return "paid", nil }
	if _, err := Step(ctx, j, "r", "pay", pay); err == nil {
		t.Fatal("want the claim's write failure")
	}
	got, err := Step(ctx, j, "r", "pay", pay)
	if err != nil || got != "paid" || runs != 1 {
		t.Fatalf("second drive = %q, %v, effect ran %d times; want paid, nil, once", got, err, runs)
	}
}

// D2, claims on resume: a side-effect tool's claim behind a store wrapper whose marker and
// not-started writes fail is remembered; the run's next drive, through another wrapper and Journal
// over the same store, records that the attempt did not start and runs the tool, rather than
// halting.
func TestRev104d_ToolClaimBehindWrapperResumes(t *testing.T) {
	ctx := context.Background()
	st := newFaultStore()
	var fail atomic.Bool
	st.commitThenErrPrefix = "attempt:tool:"
	st.failNoCommitPrefix = "attempt:not-started:"
	var charged int
	charge := &countingTool{name: "charge", safety: Safety{}, calls: &charged}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`)}}
	if _, err := mustNew(m, mustJournal(&wrapStore{Store: st, failLoad: &fail}), WithTools(charge)).Run(ctx, "r", "go"); err == nil {
		t.Fatal("want the claim's write failure")
	}
	m2 := &scriptModel{turns: [][]Emit{textTurn("done")}}
	out, err := mustNew(m2, mustJournal(&wrapStore{Store: st, failLoad: &fail}), WithTools(charge)).Run(ctx, "r", "go")
	if err != nil || out.Text() != "done" || charged != 1 {
		t.Fatalf("second drive = %q, %v, tool ran %d times; want done, nil, once", out.Text(), err, charged)
	}
}
