package agent

// rev104e: adversarial review of #104 round 4 (D2 claims behind a store wrapper; written against
// the removed Durable wrapper and converted to Store wrappers).

import (
	"bytes"
	"context"
	"errors"
	"iter"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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
	if _, err := mustNew(m, mustJournal(through(st)), WithTools(charge)).Run(ctx, "r", "go"); err == nil {
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
			_, raced = mustNew(m2, mustJournal(through(st)), WithTools(charge)).Run(ctx, "r", "go")
		})
	}
	later := func() time.Time { return time.Now().Add(time.Hour) }
	err := ResolveHaltRef(ctx, mustJournal(rw), HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "c1", ToolName: "charge"}, Cause: HaltCrashed},
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
	if _, err := Step(c1, mustJournal(&hookWrap{Store: st}), "r", "pay", pay); err == nil {
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
	_, err := Step(ctx, mustJournal(&hookWrap{Store: st}), "r", "pay", pay)
	t.Logf("drive 2: %v", err)
	got, err := Step(ctx, mustJournal(&hookWrap{Store: st}), "r", "pay", pay)
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
	if _, err := mustNew(&scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`)}}, a, WithTools(charge)).Run(ctx, "r", "go"); err == nil {
		t.Fatal("want tenant A's claim write failure")
	}
	if _, err := mustNew(
		&scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}},
		b,
		WithTools(charge),
	).Run(ctx, "r", "go"); err != nil {
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
	out, errA := mustNew(&scriptModel{turns: [][]Emit{textTurn("done")}}, a, WithTools(charge)).Run(ctx, "r", "go")
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
	if _, err := mustNew(&scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}, a, WithMiddleware(arm)).RunResult(context.Background(), "r", "go"); err == nil {
		t.Fatal("want the write failure")
	}
	fail.Store(false)
	b := mustJournal(&tenantWrap{inner: st, tenant: "B"})
	resB, err := mustNew(&scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}, b).RunResult(context.Background(), "r", "go")
	if err != nil {
		t.Fatal(err)
	}
	if resB.Spend != billed {
		t.Fatalf("tenant B's run spent %+v, want only its own %+v: tenant A's kept spend was journaled into B's run", resB.Spend, billed)
	}
	resA, err := mustNew(&scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}, a).RunResult(context.Background(), "r", "go")
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
		_, wErr = Step(context.WithValue(ctx, who{}, "W"), jw, "r", "pay", func(context.Context) (string, error) {
			ranW.Add(1)
			return "paid", nil
		})
	}()
	<-winnerClaimed // the winner holds attempt 0 and is about to send "pay"
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, lErr = Step(context.WithValue(ctx, who{}, "L"), jl, "r", "pay", func(context.Context) (string, error) {
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
