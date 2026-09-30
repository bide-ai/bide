package agent

// rev104e: adversarial review of #104 round 4 (D2 claims behind a Durable wrapper).

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hookWrap is a Durable wrapper with Unwrap() Durable (audit.AuditedStore's shape). before, if
// set, runs before a Do of the name it is given is forwarded; writes counts every Do forwarded.
type hookWrap struct {
	inner  Durable
	before func(ctx context.Context, runID, name string)
	names  []string
	mu     sync.Mutex
}

func (w *hookWrap) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	w.mu.Lock()
	w.names = append(w.names, name)
	b := w.before
	w.mu.Unlock()
	if b != nil {
		b(ctx, runID, name)
	}
	return w.inner.Do(ctx, runID, name, fn)
}
func (w *hookWrap) History(ctx context.Context, runID string) ([]Record, error) {
	return w.inner.History(ctx, runID)
}
func (w *hookWrap) Unwrap() Durable { return w.inner }

// ---------------------------------------------------------------------------------------------
// The survivor mutant: ClaimAttempt's errNoRecord guard on the wrapper path. A claim whose Do
// joined a probe's in-flight call wrote no marker; without the guard it writes a not-started
// record for a claim id no marker ever carried. The planted flight is exactly what a joined probe
// hands the claim (shareFlight returns the leader's val and err).
func TestRev104e_ClaimJoiningProbeWritesNothing(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	j, _ := NewJournal(mem)
	w := &hookWrap{inner: j}
	key := toolAttemptStep("c1")
	k := flightKey{j.id, "r", key}
	f := &flight{err: errNoRecord}
	flights.mu.Lock()
	flights.m[k] = f
	flights.mu.Unlock()
	_, _, err := ClaimAttempt(ctx, w, "r", key, Record{Kind: StepAttempt, ToolUseID: "c1"})
	flights.mu.Lock()
	delete(flights.m, k)
	flights.mu.Unlock()
	if !errors.Is(err, errNoRecord) {
		t.Fatalf("err = %v, want errNoRecord", err)
	}
	var stray []string
	for e, err := range mem.Load(ctx, "r", -1) {
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(e.Name, notStartedPrefix) {
			stray = append(stray, e.Name)
		}
	}
	if len(stray) > 0 {
		t.Fatalf("a claim that wrote no marker wrote %v", stray)
	}
	// The retry claimAttempt makes then claims with a fresh id and wins.
	won, _, err := claimAttempt(ctx, w, "r", key, Record{Kind: StepAttempt, ToolUseID: "c1"})
	if err != nil || !won {
		t.Fatalf("retry: won %v err %v", won, err)
	}
}

// ---------------------------------------------------------------------------------------------
// The resolver through a wrapper (min-age path, F2): a process remembers the live attempt's claim
// (its not-started write failed). Between the resolver's claim of the next attempt and its verdict,
// a plain Run in the same process voids the live attempt; it must lose the next attempt to the
// resolver and not fire. Journal control below.
func rev104eResolverRace(t *testing.T, through func(j *Journal) Durable) {
	ctx := context.Background()
	st := newFaultStore()
	j, _ := NewJournal(st)
	st.commitThenErrPrefix = "attempt:tool:"
	st.failNoCommitPrefix = "attempt:not-started:"
	var charged int
	charge := &countingTool{name: "charge", safety: Safety{}, calls: &charged}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`)}}
	if _, err := New(m, through(j), charge).Run(ctx, "r", "go"); err == nil {
		t.Fatal("want the claim's write failure")
	}
	var raced error
	var once sync.Once
	rw := &hookWrap{inner: through(j)}
	rw.before = func(ctx context.Context, runID, name string) {
		if name != ToolResultStep("c1") {
			return
		}
		once.Do(func() {
			m2 := &scriptModel{turns: [][]Emit{textTurn("done")}}
			_, raced = New(m2, through(j), charge).Run(ctx, "r", "go")
		})
	}
	later := func() time.Time { return time.Now().Add(time.Hour) }
	err := ResolveHaltRef(ctx, rw, HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "c1", ToolName: "charge"}, Cause: HaltCrashed},
		Outcome{Result: "charged"}, WithMinHaltAge(time.Minute), WithNow(later))
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
	rev104eResolverRace(t, func(j *Journal) Durable { return &hookWrap{inner: j} })
}

func TestRev104e_ResolverClaimsNextThroughJournal(t *testing.T) {
	rev104eResolverRace(t, func(j *Journal) Durable { return j })
}

// ---------------------------------------------------------------------------------------------
// F1 set semantics on the wrapper path: two claims of one marker key each fail to record that
// they did not start (one won its marker and was cancelled before the call; one whose marker
// Insert committed and errored). Both ids are remembered; the next drive in the process re-attempts
// instead of halting.
func TestRev104e_TwoRememberedIdsThroughWrapper(t *testing.T) {
	ctx := context.Background()
	st := newFaultStore()
	j, _ := NewJournal(st)
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
	if _, err := Step(c1, &hookWrap{inner: j}, "r", "pay", pay); err == nil {
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
	_, err := Step(ctx, &hookWrap{inner: j}, "r", "pay", pay)
	t.Logf("drive 2: %v", err)
	got, err := Step(ctx, &hookWrap{inner: j}, "r", "pay", pay)
	if err != nil || got != "paid" || runs != 1 {
		t.Fatalf("drive 3 = %q, %v, ran %d; want paid, nil, once", got, err, runs)
	}
}

// ---------------------------------------------------------------------------------------------
// Identity: a wrapper that rewrites run IDs (a tenant prefix). The contract (Durable) says such a
// wrapper must not implement Unwrap() Durable: what the process keeps for a run (remembered claims,
// kept spend) is keyed by the identity of the store beneath an unwrapping wrapper, with the run ID
// as given, so two tenants' runs named "r" would share it. storetest.CheckDurableWrapper refuses
// a rewriting wrapper that unwraps (its tests hold one). A tenant wrapper that keeps to the
// contract, as here, is its own identity: each tenant's wrapper keeps its own.
type tenantWrap struct {
	inner  Durable
	tenant string
}

func (w *tenantWrap) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	return w.inner.Do(ctx, w.tenant+"/"+runID, name, fn)
}
func (w *tenantWrap) History(ctx context.Context, runID string) ([]Record, error) {
	return w.inner.History(ctx, w.tenant+"/"+runID)
}

// A tenant's remembered claim stays with that tenant: tenant B's drive of its own run "r" does not
// take tenant A's remembered claim id, and tenant A's next drive records it and re-attempts.
func TestRev104e_TenantWrapperSharesRememberedClaims(t *testing.T) {
	ctx := context.Background()
	st := newFaultStore()
	j, _ := NewJournal(st)
	st.commitThenErrPrefix = "attempt:tool:"
	st.failNoCommitPrefix = "attempt:not-started:"
	var charged int
	charge := &countingTool{name: "charge", safety: Safety{}, calls: &charged}
	a := &tenantWrap{inner: j, tenant: "A"}
	b := &tenantWrap{inner: j, tenant: "B"}
	if _, err := New(&scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`)}}, a, charge).Run(ctx, "r", "go"); err == nil {
		t.Fatal("want tenant A's claim write failure")
	}
	if _, err := New(&scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}, b, charge).Run(ctx, "r", "go"); err != nil {
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
	out, errA := New(&scriptModel{turns: [][]Emit{textTurn("done")}}, a, charge).Run(ctx, "r", "go")
	if errA != nil || out.Text() != "done" || charged != 2 {
		t.Fatalf("tenant A's next drive = %q, %v, tool calls %d; want done, nil, 2 (one per tenant)", out.Text(), errA, charged)
	}
}

// Kept spend stays with its tenant the same way: tenant A's billed turn whose record could not be
// written is journaled into tenant A's run, not into tenant B's run "r".
func TestRev104e_TenantWrapperSharesKeptSpend(t *testing.T) {
	st := newFaultStore()
	j, _ := NewJournal(st)
	var fail atomic.Bool
	a := &wrapDurable{Durable: &tenantWrap{inner: j, tenant: "A"}, failHist: &fail}
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
	if _, err := New(&scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}, a).Use(arm).RunResult(context.Background(), "r", "go"); err == nil {
		t.Fatal("want the write failure")
	}
	fail.Store(false)
	b := &tenantWrap{inner: j, tenant: "B"}
	resB, err := New(&scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}, b).RunResult(context.Background(), "r", "go")
	if err != nil {
		t.Fatal(err)
	}
	if resB.Spend != billed {
		t.Fatalf("tenant B's run spent %+v, want only its own %+v: tenant A's kept spend was journaled into B's run", resB.Spend, billed)
	}
	resA, err := New(&scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}, a).RunResult(context.Background(), "r", "go")
	if err != nil {
		t.Fatal(err)
	}
	if resA.Spend != twice(billed) {
		t.Fatalf("tenant A's run spent %+v, want both its requests' %+v", resA.Spend, twice(billed))
	}
}

// ---------------------------------------------------------------------------------------------
// "Losers never lead flights" (Claims.tla WinnerNeverHalts; journalStep's loser only joins). In
// durableStep the loser runs d.Do on the step's name, so through a wrapper over a Journal in one
// process it leads a flight; the winner joins it and takes the loser's halt as its own outcome.
// Pre-existing on main (not introduced by round 4); fixed: the loser only joins, then reads.
func TestRev104e_WrapperStepLoserLeadsFlight(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	j, _ := NewJournal(mem)
	type who struct{}
	var ranW atomic.Int32
	release := make(chan struct{})
	winnerAtResult := make(chan struct{})
	w := &hookWrap{inner: j}
	w.before = func(ctx context.Context, runID, name string) {
		if name != "pay" {
			return
		}
		switch ctx.Value(who{}) {
		case "W":
			close(winnerAtResult)
		}
	}
	// A Get hook would need a custom store; instead hold the loser inside its flight by
	// blocking in its fn is not possible (fn is the engine's). Use a store whose Get of "pay"
	// blocks while the loser leads.
	bs := &blockGetStore{Store: mem, name: "pay", in: make(chan struct{}), release: release}
	jb, _ := NewJournal(bs)
	w.inner = jb
	// Winner claims attempt 0 first, then waits.
	var wg sync.WaitGroup
	var wErr, lErr error
	winnerClaimed := make(chan struct{})
	goWinner := make(chan struct{})
	w2 := &hookWrap{inner: jb, before: func(ctx context.Context, runID, name string) {
		if ctx.Value(who{}) == "W" && name == "pay" {
			close(winnerClaimed)
			<-goWinner
			close(winnerAtResult)
		}
	}}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, wErr = Step(context.WithValue(ctx, who{}, "W"), w2, "r", "pay", func(context.Context) (string, error) {
			ranW.Add(1)
			return "paid", nil
		})
	}()
	<-winnerClaimed // the winner holds attempt 0 and is about to Do "pay"
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, lErr = Step(context.WithValue(ctx, who{}, "L"), w, "r", "pay", func(context.Context) (string, error) {
			return "loser-ran", nil
		})
	}()
	<-bs.in // the loser reads "pay" (through a call in flight it leads, before the fix) and is held
	close(goWinner)
	<-winnerAtResult
	// Before the fix the winner joins the loser's call and waits on it; with it, the winner leads
	// its own call and runs the step. Either way, release the loser's read then.
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

type blockGetStore struct {
	Store
	name    string
	in      chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockGetStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	if name == s.name {
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
