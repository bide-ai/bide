package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"iter"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The remembered claims of one marker key are a set: remembering adds, the resume gate takes only
// the live marker's own id, and a claim takes them all.
func TestClaimMemo_IsASetPerKey(t *testing.T) {
	ctx := context.Background()
	j := newJournal(NewMemStore())
	if err := j.ensureHeader(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	key := stepAttemptStep("s")
	k := flightKey{j.id, "r", key}
	pendingClaims.remember(k, "b1")
	pendingClaims.remember(k, "a1")
	pendingClaims.remember(k, "a1") // twice: held once
	marker := Record{Kind: StepAttempt, ToolUseID: "s", claim: "a1"}
	if !j.retryNotStarted(ctx, "r", key, marker) {
		t.Fatal("the resume gate did not record the live marker's remembered claim as not started")
	}
	if j.retryNotStarted(ctx, "r", key, marker) {
		t.Fatal("the live marker's id was still remembered after the resume gate took it")
	}
	if got := pendingClaims.takeAll(k); !slices.Equal(got, []string{"b1"}) {
		t.Fatalf("after the resume gate took a1, the key remembers %v; want [b1] (another claim's id stays)", got)
	}
	if got := pendingClaims.takeAll(k); len(got) != 0 {
		t.Fatalf("takeAll left %v", got)
	}
}

// Past maxPendingClaims ids, whole keys are dropped, oldest first, and the count stays exact.
func TestClaimMemo_DropsWholeKeysPastTheBound(t *testing.T) {
	c := &claimMemo{m: map[flightKey]map[string]struct{}{}}
	first := flightKey{nil, "r", "first"}
	c.remember(first, "x")
	c.remember(first, "y")
	for i := range maxPendingClaims - 1 {
		c.remember(flightKey{nil, "r", string(rune('a'+i%26)) + string(rune(i))}, "id")
	}
	if _, ok := c.m[first]; ok || c.n != maxPendingClaims-1 {
		t.Fatalf("held %d ids, first key kept = %v; want the oldest key dropped whole and %d ids", c.n, ok, maxPendingClaims-1)
	}
	total := 0
	for _, ids := range c.m {
		total += len(ids)
	}
	if total != c.n {
		t.Fatalf("count %d, ids held %d", c.n, total)
	}
}

// failNames fails, not committed, every Insert whose name contains one of its substrings.
type failNames struct {
	Store
	subs []string
}

func (f failNames) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	for _, s := range f.subs {
		if strings.Contains(name, s) {
			return Entry{}, false, errors.New("injected store fault")
		}
	}
	return f.Store.Insert(ctx, runID, name, data)
}

// A claim writes the not-started record of every id remembered for its key, one at a time: one
// that cannot be written is remembered again and does not stop the others, and the claim then
// goes on with a fresh id of its own.
func TestClaimMemo_ClaimRetriesEveryRememberedID(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	j := newJournal(failNames{mem, []string{"aaaa"}})
	if err := j.ensureHeader(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	key := stepAttemptStep("s")
	k := flightKey{j.id, "r", key}
	pendingClaims.remember(k, "aaaa")
	pendingClaims.remember(k, "bbbb")
	won, _, err := j.claim(ctx, "r", key, Record{Kind: StepAttempt, ToolUseID: "s", AttemptedAt: 1})
	if err != nil || !won {
		t.Fatalf("claim = %v, %v; want a won claim under a fresh id (no remembered marker was written)", won, err)
	}
	if rec, ok, err := j.Get(ctx, "r", notStartedStep(key, "bbbb")); err != nil || !ok || rec.Kind != StepNotStarted {
		t.Fatalf("bbbb's not-started record = %v, %v; want it written though aaaa's failed", ok, err)
	}
	if got := pendingClaims.takeAll(k); !slices.Equal(got, []string{"aaaa"}) {
		t.Fatalf("remembered after the claim: %v; want [aaaa], whose record could not be written", got)
	}
}

func TestNextAttemptStep(t *testing.T) {
	for _, base := range []string{toolAttemptStep("c:1"), stepAttemptStep("charge")} {
		for gen := range 12 {
			if got, want := nextAttemptStep(retryAttemptStep(base, gen)), retryAttemptStep(base, gen+1); got != want {
				t.Errorf("nextAttemptStep(%q) = %q, want %q", retryAttemptStep(base, gen), got, want)
			}
		}
	}
}

// drvKey tags a context with the driver it belongs to, for the hooks below.
type drvKey struct{}

func drvOf(ctx context.Context) int { d, _ := ctx.Value(drvKey{}).(int); return d }

// getHookStore is a Store whose Get of a name calls get first.
type getHookStore struct {
	Store
	get func(ctx context.Context, name string)
}

func (s *getHookStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	if s.get != nil {
		s.get(ctx, name)
	}
	return s.Store.Get(ctx, runID, name)
}

// claimHookStore is a Store wrapper (audit.AuditedStore's shape: it passes run IDs and names
// through and implements Unwrap) whose Insert of a name calls inserted once the inner Insert
// returns. The Insert of a step's attempt marker is its claim, so a hook on it runs when a driver
// has just won (or lost) the claim and is about to send the step.
type claimHookStore struct {
	Store
	inserted func(ctx context.Context, name string)
}

func (w *claimHookStore) Unwrap() Store { return w.Store }

func (w *claimHookStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	e, ok, err := w.Store.Insert(ctx, runID, name, data)
	if w.inserted != nil {
		w.inserted(ctx, name)
	}
	return e, ok, err
}

// The claim model's regress/loser-leads schedule (spec/tla/claims/regress/loser-leads.cfg,
// WinnerNeverHalts), through a store wrapper: two drivers of one Step in one process. d1 wins
// the attempt's claim; d2 loses it and reads the step's result; d1 then sends the step. The
// loser's read must not be a call in flight that the winner joins: the winner would take the
// loser's halt as its own outcome, record that it never started, and halt though it owns the
// attempt. The winner must run the step, and the loser gets its value or a halt.
func TestLoserLeads_WinnerNeverHaltsThroughWrapper(t *testing.T) {
	ctx := context.Background()
	d1At := make(chan struct{})       // d1 has won its claim and is about to send the step
	d2Reading := make(chan struct{})  // d2, the loser, is reading the step's result
	joined := make(chan struct{})     // a caller joined a call of the step in flight
	winnerDone := make(chan struct{}) // d1's Step returned
	var once sync.Once
	h := func(k flightKey) {
		if k.name == "pay" {
			once.Do(func() { close(joined) })
		}
	}
	flightJoinHook.Store(&h)
	t.Cleanup(func() { flightJoinHook.Store(nil) })

	var readOnce sync.Once
	st := &getHookStore{Store: NewMemStore(), get: func(ctx context.Context, name string) {
		if name == "pay" && drvOf(ctx) == 2 {
			readOnce.Do(func() {
				close(d2Reading)
				select { // hold the loser's read until the winner joins it, or finishes on its own
				case <-joined:
				case <-winnerDone:
				}
			})
		}
	}}
	var atOnce sync.Once
	w, err := NewJournal(&claimHookStore{Store: st, inserted: func(ctx context.Context, name string) {
		if name == stepAttemptStep("pay") && drvOf(ctx) == 1 {
			atOnce.Do(func() {
				close(d1At)
				<-d2Reading
			})
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	var runs int
	pay := func(context.Context) (string, error) { runs++; return "paid", nil }

	var got1 string
	var err1 error
	go func() {
		defer close(winnerDone)
		got1, err1 = w.Step(context.WithValue(ctx, drvKey{}, 1), "r", "pay", pay)
	}()
	<-d1At
	got2, err2 := w.Step(context.WithValue(ctx, drvKey{}, 2), "r", "pay", pay)
	<-winnerDone

	if err1 != nil || got1 != "paid" || runs != 1 {
		t.Fatalf("the winner got %q, %v, and the step ran %d times; want paid, nil, once (WinnerNeverHalts)", got1, err1, runs)
	}
	var halt *OutcomeUnknown
	if err2 != nil && !errors.As(err2, &halt) {
		t.Fatalf("the loser got %v, want the value or a halt", err2)
	}
	if err2 == nil && got2 != "paid" {
		t.Fatalf("the loser got %q, want paid", got2)
	}
}

// A loser that finds the winner's call of the step in flight, through a store wrapper, and sees
// it fail, halts with HaltContended, as through the Journal: the winner was live after the claim
// and owns the effect.
func TestLoserJoinsFailedFlight_HaltContendedThroughWrapper(t *testing.T) {
	ctx := context.Background()
	inBody := make(chan struct{})
	release := make(chan struct{})
	joined := make(chan struct{})
	var once sync.Once
	h := func(k flightKey) {
		if k.name == "pay" {
			once.Do(func() { close(joined) })
		}
	}
	flightJoinHook.Store(&h)
	t.Cleanup(func() { flightJoinHook.Store(nil) })
	w, err := NewJournal(&claimHookStore{Store: NewMemStore()})
	if err != nil {
		t.Fatal(err)
	}
	winnerDone := make(chan error, 1)
	go func() {
		_, err := w.Step(ctx, "r", "pay", func(context.Context) (string, error) {
			close(inBody)
			<-release
			return "", errors.New("provider down")
		})
		winnerDone <- err
	}()
	<-inBody
	go func() {
		select { // release the winner once the loser waits on its call (or, failing that, in the end)
		case <-joined:
		case <-time.After(10 * time.Second):
		}
		close(release)
	}()
	_, err2 := w.Step(ctx, "r", "pay", func(context.Context) (string, error) { return "", nil })
	<-winnerDone
	var halt *OutcomeUnknown
	if !errors.As(err2, &halt) || halt.Cause != HaltContended {
		t.Fatalf("the loser got %v, want a halt with cause HaltContended", err2)
	}
}

// The same rule when the loser's read finds nothing at once: the loser halts without starting a
// call of the step, so a winner that sends the step afterwards leads its own call. Were the loser
// to go on and read through a call in flight (the historical LoserLeads), the winner would join it.
func TestLoserLeads_LoserStartsNoCallAfterItsRead(t *testing.T) {
	ctx := context.Background()
	d1At := make(chan struct{})
	d2InFlight := make(chan struct{}) // d2 reads the step inside a call in flight
	d2Done := make(chan struct{})
	joined := make(chan struct{})
	winnerDone := make(chan struct{})
	var once sync.Once
	h := func(k flightKey) {
		if k.name == "pay" {
			once.Do(func() { close(joined) })
		}
	}
	flightJoinHook.Store(&h)
	t.Cleanup(func() { flightJoinHook.Store(nil) })
	var mu sync.Mutex
	reads := 0
	st := &getHookStore{Store: NewMemStore(), get: func(ctx context.Context, name string) {
		if name != "pay" || drvOf(ctx) != 2 {
			return
		}
		mu.Lock()
		reads++
		n := reads
		mu.Unlock()
		if n == 2 { // a second read of the step by the loser: a call in flight it leads
			close(d2InFlight)
			select {
			case <-joined:
			case <-winnerDone:
			}
		}
	}}
	var atOnce sync.Once
	w, err := NewJournal(&claimHookStore{Store: st, inserted: func(ctx context.Context, name string) {
		if name == stepAttemptStep("pay") && drvOf(ctx) == 1 {
			atOnce.Do(func() {
				close(d1At)
				select {
				case <-d2InFlight:
				case <-d2Done:
				}
			})
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	var runs int
	pay := func(context.Context) (string, error) { runs++; return "paid", nil }
	var got1 string
	var err1 error
	go func() {
		defer close(winnerDone)
		got1, err1 = w.Step(context.WithValue(ctx, drvKey{}, 1), "r", "pay", pay)
	}()
	<-d1At
	_, _ = w.Step(context.WithValue(ctx, drvKey{}, 2), "r", "pay", pay)
	close(d2Done)
	<-winnerDone
	if err1 != nil || got1 != "paid" || runs != 1 {
		t.Fatalf("the winner got %q, %v, and the step ran %d times; want paid, nil, once (WinnerNeverHalts)", got1, err1, runs)
	}
}

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
	if _, err := mustNew(m, mustJournal(w1), WithMiddleware(arm)).Run(context.Background(), "r", UserText("go")); err == nil {
		t.Fatal("want the write failure")
	}
	fail.Store(false)
	w2 := &wrapStore{Store: st, failLoad: &fail}
	m2 := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	res, err := mustNew(m2, mustJournal(w2)).Run(context.Background(), "r", UserText("go"))
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
	if _, err := mustNew(m, w1, WithMiddleware(arm)).Run(context.Background(), "r", UserText("go")); err == nil {
		t.Fatal("want the write failure")
	}
	fail.Store(false)
	m2 := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	res, err := mustNew(m2, w1).Run(context.Background(), "r", UserText("go"))
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
	if _, err := mustJournal(&wrapStore{Store: st, failLoad: &fail}).Step(ctx, "r", "pay", pay); err == nil {
		t.Fatal("want the claim's write failure")
	}
	got, err := mustJournal(&wrapStore{Store: st, failLoad: &fail}).Step(ctx, "r", "pay", pay)
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
	if _, err := j.Step(ctx, "r", "pay", pay); err == nil {
		t.Fatal("want the claim's write failure")
	}
	got, err := j.Step(ctx, "r", "pay", pay)
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
	if _, err := mustNew(m, mustJournal(&wrapStore{Store: st, failLoad: &fail}), WithTools(charge)).Run(ctx, "r", UserText("go")); err == nil {
		t.Fatal("want the claim's write failure")
	}
	m2 := &scriptModel{turns: [][]Emit{textTurn("done")}}
	res, err := mustNew(m2, mustJournal(&wrapStore{Store: st, failLoad: &fail}), WithTools(charge)).Run(ctx, "r", UserText("go"))
	var out Message
	if res != nil {
		out = res.Message
	}
	if err != nil || out.Text() != "done" || charged != 1 {
		t.Fatalf("second drive = %q, %v, tool ran %d times; want done, nil, once", out.Text(), err, charged)
	}
}

// D2 claims behind a store wrapper: a claim made through a Store wrapper is remembered and shared
// as through the bare store.

// hookWrap is a Store wrapper with Unwrap() Store (audit.AuditedStore's shape). before, if set,
// runs before an Insert of the name it is given is forwarded.
type hookWrap struct {
	Store
	before func(ctx context.Context, runID, name string)
}

func (w *hookWrap) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if w.before != nil {
		w.before(ctx, runID, name)
	}
	return w.Store.Insert(ctx, runID, name, data)
}

func (w *hookWrap) Unwrap() Store { return w.Store }

// ---------------------------------------------------------------------------------------------
// The resolver through a wrapper (min-age path, F2): a process remembers the live attempt's claim
// (its not-started write failed). Between the resolver's claim of the next attempt and its verdict
// (the Insert of the tool's result), a plain Run in the same process voids the live attempt; it
// must lose the next attempt to the resolver and not fire. Control on the store itself below.
func rev104eResolverRace(t *testing.T, through func(s Store) Store) {
	ctx := context.Background()
	st := newFaultStore()
	st.commitThenErrPrefix = "attempt:tool:"
	st.failNoCommitPrefix = "attempt:not-started:"
	var charged int
	charge := &countingTool{name: "charge", safety: Safety{}, calls: &charged}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`)}}
	if _, err := mustNew(m, mustJournal(through(st)), WithTools(charge)).Run(ctx, "r", UserText("go")); err == nil {
		t.Fatal("want the claim's write failure")
	}
	var raced error
	var once sync.Once
	rw := &hookWrap{Store: through(st)}
	rw.before = func(ctx context.Context, runID, name string) {
		if name != ToolResultStep("c1") {
			return
		}
		once.Do(func() {
			m2 := &scriptModel{turns: [][]Emit{textTurn("done")}}
			_, raced = mustNew(m2, mustJournal(through(st)), WithTools(charge)).Run(ctx, "r", UserText("go"))
		})
	}
	later := func() time.Time { return time.Now().Add(time.Hour) }
	err := ResolveHalt(ctx, mustJournal(rw), HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "c1", ToolName: "charge"}, Cause: HaltCrashed},
		Outcome{Result: "charged"}, WithMinHaltAge(time.Minute), WithClock(later))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if charged != 0 {
		t.Fatalf("the racing Run fired the effect beside the verdict (%d calls); it returned %v", charged, raced)
	}
	var hl *OutcomeUnknown
	if !errors.As(raced, &hl) {
		t.Fatalf("racing Run = %v, want a halt", raced)
	}
}

func TestRev104e_ResolverClaimsNextThroughWrapper(t *testing.T) {
	rev104eResolverRace(t, func(s Store) Store { return &hookWrap{Store: s} })
}

func TestRev104e_ResolverClaimsNextThroughJournal(t *testing.T) {
	rev104eResolverRace(t, func(s Store) Store { return s })
}

// ---------------------------------------------------------------------------------------------
// F1 set semantics on the wrapper path: two claims of one marker key each fail to record that
// they did not start (one won its marker and was cancelled before the call; one whose marker
// Insert committed and errored). Both ids are remembered; the next drive in the process re-attempts
// instead of halting.
func TestRev104e_TwoRememberedIdsThroughWrapper(t *testing.T) {
	ctx := context.Background()
	st := newFaultStore()
	j := mustJournal(st) // its identity is st's, as each wrapper's Journal's is
	var runs int
	pay := func(context.Context) (string, error) { runs++; return "paid", nil }
	// Drive 1: claim wins attempt 0, then is cancelled before fn; its not-started write fails.
	c1, cancel := context.WithCancel(ctx)
	st.afterInsert = func(name string) {
		if strings.HasPrefix(name, "attempt:step:") {
			cancel()
		}
	}
	st.failNoCommitPrefix = "attempt:not-started:"
	if _, err := mustJournal(&hookWrap{Store: st}).Step(c1, "r", "pay", pay); err == nil {
		t.Fatal("drive 1: want the cancellation")
	}
	st.afterInsert = nil
	if n := len(pendingClaims.m[flightKey{j.id, "r", stepAttemptStep("pay")}]); n != 1 {
		t.Fatalf("after drive 1, %d ids remembered, want 1", n)
	}
	// Drive 2: remembered id's not-started write fails again, and the fresh claim's marker
	// Insert commits and errors, whose not-started write fails too: two ids for attempt 0?
	// (attempt 0 holds drive 1's marker, so drive 2's fresh claim loses to it; the claimNext
	// loop reads it as live and halts.)
	st.failNoCommitPrefix = "attempt:not-started:"
	_, err := mustJournal(&hookWrap{Store: st}).Step(ctx, "r", "pay", pay)
	t.Logf("drive 2: %v", err)
	got, err := mustJournal(&hookWrap{Store: st}).Step(ctx, "r", "pay", pay)
	if err != nil || got != "paid" || runs != 1 {
		t.Fatalf("drive 3 = %q, %v, ran %d; want paid, nil, once", got, err, runs)
	}
}

// ---------------------------------------------------------------------------------------------
// Identity: a wrapper that rewrites run IDs (a tenant prefix). The contract (Store) says such a
// wrapper must not implement Unwrap() Store: what the process keeps for a run (remembered claims,
// kept spend) is keyed by the identity of the store beneath an unwrapping wrapper, with the run ID
// as given, so two tenants' runs named "r" would share it. storetest.CheckWrapper refuses a
// rewriting wrapper that unwraps. A tenant wrapper that keeps to the contract, as here, is its own
// identity: each tenant's wrapper keeps its own.
type tenantWrap struct {
	inner  Store
	tenant string
}

func (w *tenantWrap) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	return w.inner.Insert(ctx, w.tenant+"/"+runID, name, data)
}
func (w *tenantWrap) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	return w.inner.Get(ctx, w.tenant+"/"+runID, name)
}
func (w *tenantWrap) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	return w.inner.Load(ctx, w.tenant+"/"+runID, after)
}

// A tenant's remembered claim stays with that tenant: tenant B's drive of its own run "r" does not
// take tenant A's remembered claim id, and tenant A's next drive records it and re-attempts.
func TestRev104e_TenantWrapperSharesRememberedClaims(t *testing.T) {
	ctx := context.Background()
	st := newFaultStore()
	st.commitThenErrPrefix = "attempt:tool:"
	st.failNoCommitPrefix = "attempt:not-started:"
	var charged int
	charge := &countingTool{name: "charge", safety: Safety{}, calls: &charged}
	a := mustJournal(&tenantWrap{inner: st, tenant: "A"})
	b := mustJournal(&tenantWrap{inner: st, tenant: "B"})
	if _, err := mustNew(&scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`)}}, a, WithTools(charge)).Run(ctx, "r", UserText("go")); err == nil {
		t.Fatal("want tenant A's claim write failure")
	}
	if _, err := mustNew(
		&scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}},
		b,
		WithTools(charge),
	).Run(ctx, "r", UserText("go")); err != nil {
		t.Fatalf("tenant B: %v", err)
	}
	var foreign []string
	for e, err := range st.Load(ctx, "B/r", -1) {
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(e.Name, notStartedPrefix) {
			foreign = append(foreign, e.Name)
		}
	}
	if len(foreign) > 0 {
		t.Fatalf("tenant B's journal holds %v, a not-started record of tenant A's claim, written through B's wrapper", foreign)
	}
	res, errA := mustNew(&scriptModel{turns: [][]Emit{textTurn("done")}}, a, WithTools(charge)).Run(ctx, "r", UserText("go"))
	var out Message
	if res != nil {
		out = res.Message
	}
	if errA != nil || out.Text() != "done" || charged != 2 {
		t.Fatalf("tenant A's next drive = %q, %v, tool calls %d; want done, nil, 2 (one per tenant)", out.Text(), errA, charged)
	}
}

// Kept spend stays with its tenant the same way: tenant A's billed turn whose record could not be
// written is journaled into tenant A's run, not into tenant B's run "r".
func TestRev104e_TenantWrapperSharesKeptSpend(t *testing.T) {
	st := newFaultStore()
	var fail atomic.Bool
	a := mustJournal(&wrapStore{Store: &tenantWrap{inner: st, tenant: "A"}, failLoad: &fail})
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
	if _, err := mustNew(&scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}, a, WithMiddleware(arm)).Run(context.Background(), "r", UserText("go")); err == nil {
		t.Fatal("want the write failure")
	}
	fail.Store(false)
	b := mustJournal(&tenantWrap{inner: st, tenant: "B"})
	resB, err := mustNew(&scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}, b).Run(context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	if resB.Spend != billed {
		t.Fatalf("tenant B's run spent %+v, want only its own %+v: tenant A's kept spend was journaled into B's run", resB.Spend, billed)
	}
	resA, err := mustNew(&scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}, a).Run(context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	if resA.Spend != twice(billed) {
		t.Fatalf("tenant A's run spent %+v, want both its requests' %+v", resA.Spend, twice(billed))
	}
}

// ---------------------------------------------------------------------------------------------
// "Losers never lead flights" (Claims.tla WinnerNeverHalts; journalStep's loser only joins). The
// removed durableStep path had the loser run d.Do on the step's name, so through a wrapper in one
// process it led a flight; the winner joined it and took the loser's halt as its own outcome.
// Here the winner (through a store wrapper) has won its claim and is about to send the step while
// the loser's read of the step (through the store beneath, the same identity) is held: the winner
// must lead its own call and run the step.
func TestRev104e_WrapperStepLoserLeadsFlight(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	type who struct{}
	var ranW atomic.Int32
	release := make(chan struct{})
	winnerAtResult := make(chan struct{})
	// The loser's first read of "pay" blocks until released.
	bs := &blockGetStore{Store: mem, name: "pay", who: "L", in: make(chan struct{}), release: release}
	bs.whoOf = func(ctx context.Context) any { return ctx.Value(who{}) }
	jl := mustJournal(bs)
	// The winner claims attempt 0 first, then waits.
	var wg sync.WaitGroup
	var wErr, lErr error
	winnerClaimed := make(chan struct{})
	goWinner := make(chan struct{})
	var claimedOnce sync.Once
	jw := mustJournal(&claimHookStore{Store: bs, inserted: func(ctx context.Context, name string) {
		if ctx.Value(who{}) == "W" && name == stepAttemptStep("pay") {
			claimedOnce.Do(func() {
				close(winnerClaimed)
				<-goWinner
				close(winnerAtResult)
			})
		}
	}})
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, wErr = jw.Step(context.WithValue(ctx, who{}, "W"), "r", "pay", func(context.Context) (string, error) {
			ranW.Add(1)
			return "paid", nil
		})
	}()
	<-winnerClaimed // the winner holds attempt 0 and is about to send "pay"
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, lErr = jl.Step(context.WithValue(ctx, who{}, "L"), "r", "pay", func(context.Context) (string, error) {
			return "loser-ran", nil
		})
	}()
	<-bs.in // the loser reads "pay" and is held
	close(goWinner)
	<-winnerAtResult
	// Were the loser's read a call in flight, the winner would join it and wait on it; as it is
	// not, the winner leads its own call and runs the step. Either way, release the loser's read then.
	for ranW.Load() == 0 && !joinedFlight() {
		runtime.Gosched()
	}
	close(release)
	wg.Wait()
	var hl *OutcomeUnknown
	if errors.As(wErr, &hl) || ranW.Load() != 1 {
		t.Fatalf("winner = %v, ran %d; loser = %v: the claim winner took the loser's halt", wErr, ranW.Load(), lErr)
	}
}

// blockGetStore holds the first Get of name by the driver who (whoOf its context) until release.
type blockGetStore struct {
	Store
	name    string
	who     any
	whoOf   func(context.Context) any
	in      chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockGetStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	if name == s.name && s.whoOf(ctx) == s.who {
		first := false
		s.once.Do(func() { first = true })
		if first {
			close(s.in)
			<-s.release
		}
	}
	return s.Store.Get(ctx, runID, name)
}

// joinedFlight reports whether a goroutine is waiting in shareFlight on another's flight.
func joinedFlight() bool {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	for g := range bytes.SplitSeq(buf[:n], []byte("\n\n")) {
		if bytes.Contains(g, []byte("sync.(*WaitGroup).Wait")) && bytes.Contains(g, []byte("agent.shareFlight")) {
			return true
		}
	}
	return false
}
