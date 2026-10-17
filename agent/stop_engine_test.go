package agent

import (
	"context"
	"errors"
	"iter"
	"sync/atomic"
	"testing"
	"time"
)

// seqModel replays turns by the number of assistant turns in the request (so a resume lines up), and
// counts its calls.
type seqModel struct {
	turns [][]Emit
	calls atomic.Int32
}

func (m *seqModel) Stream(_ context.Context, req Request) (*Stream, error) {
	m.calls.Add(1)
	idx := 0
	for _, msg := range req.Messages {
		if msg.Role == RoleAssistant {
			idx++
		}
	}
	idx = min(idx, len(m.turns)-1)
	turn := m.turns[idx]
	ch := make(chan Emit, len(turn))
	for _, e := range turn {
		ch <- e
	}
	close(ch)
	return NewStream(ch), nil
}

// stopRig is a store, a basic stop control and a switch over it, and an agent factory.
type stopRig struct {
	t     *testing.T
	store Store
	j     *Journal
	ctl   *StopControl
	sw    *StopSwitch
}

func newStopRig(t *testing.T, store Store, opts ...StopSwitchOption) *stopRig {
	t.Helper()
	j := mustJournal(store)
	ctl, err := NewStopControl(j, WithStopMode(StopBasic))
	if err != nil {
		t.Fatal(err)
	}
	sw, err := NewStopSwitch(ctl, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return &stopRig{t: t, store: store, j: j, ctl: ctl, sw: sw}
}

func (r *stopRig) stop(scope StopScope, opts ...func(*StopRequest)) StopEntry {
	r.t.Helper()
	req := StopRequest{Scope: scope, By: "dayna", Reason: "test"}
	for _, o := range opts {
		o(&req)
	}
	e, err := r.ctl.SetStop(context.Background(), req)
	if err != nil {
		r.t.Fatal(err)
	}
	return e
}

func (r *stopRig) lift(e StopEntry) {
	r.t.Helper()
	if _, err := r.ctl.LiftStop(context.Background(), e.ID, LiftRequest{By: "dayna"}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *stopRig) agent(m Model, tools ...Tool) *Agent {
	return mustNew(m, r.j, WithTools(tools...), WithStopSwitch(r.sw))
}

func asStopped(t *testing.T, err error) *Stopped {
	t.Helper()
	var st *Stopped
	if !errors.As(err, &st) || !IsPause(err) {
		t.Fatalf("err = %v, want a *Stopped pause", err)
	}
	return st
}

func (r *stopRig) hasKey(runID, name string) bool {
	r.t.Helper()
	_, ok, err := r.j.Get(context.Background(), runID, name)
	if err != nil {
		r.t.Fatal(err)
	}
	return ok
}

// A global stop holds a run before the model is asked, spending nothing; once lifted, the same run
// carries on and completes.
func TestStopEngine_GlobalHoldsBeforeTheModel(t *testing.T) {
	ctx := context.Background()
	r := newStopRig(t, NewMemStore(), WithStopPoll(10*time.Millisecond))
	var calls int
	tool := &countingTool{name: "charge", calls: &calls}
	m := &seqModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	e := r.stop(StopScope{Kind: StopGlobal})
	_, err := r.agent(m, tool).Run(ctx, "run1", UserText("go"))
	st := asStopped(t, err)
	if st.Cause != StopCauseStop || st.StopID != e.ID || st.RunID != "run1" || m.calls.Load() != 0 || calls != 0 {
		t.Fatalf("%+v model calls %d tool calls %d", st, m.calls.Load(), calls)
	}
	r.lift(e)
	time.Sleep(15 * time.Millisecond) // past the poll interval: a cached check reads again
	res, err := r.agent(m, tool).Run(ctx, "run1", UserText("go"))
	if err != nil || textOf(res.Message) != "done" || calls != 1 {
		t.Fatalf("after the lift: %v %v calls %d", res, err, calls)
	}
}

// A tool stop holds that tool's call before its claim: no attempt marker is written, the model's other
// turns are not stopped, and after the lift the call runs once.
func TestStopEngine_ToolStopHoldsTheCallBeforeTheClaim(t *testing.T) {
	ctx := context.Background()
	r := newStopRig(t, NewMemStore(), WithStopPoll(10*time.Millisecond))
	var charges, lookups int
	charge := &countingTool{name: "charge", calls: &charges}
	lookup := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &lookups}
	m := &seqModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	e := r.stop(StopScope{Kind: StopTool, Name: "charge"})
	_, err := r.agent(m, charge, lookup).Run(ctx, "run1", UserText("go"))
	st := asStopped(t, err)
	if st.Scope.Name != "charge" || charges != 0 || m.calls.Load() != 1 || r.hasKey("run1", toolAttemptStep("c1")) {
		t.Fatalf("%+v charges %d model calls %d marker %v", st, charges, m.calls.Load(), r.hasKey("run1", toolAttemptStep("c1")))
	}
	r.lift(e)
	time.Sleep(15 * time.Millisecond)
	if _, err := r.agent(m, charge, lookup).Run(ctx, "run1", UserText("go")); err != nil || charges != 1 {
		t.Fatalf("after the lift: %v charges %d", err, charges)
	}
	// A stop on another tool leaves this one alone.
	r2 := newStopRig(t, NewMemStore(), WithStopPoll(10*time.Millisecond))
	var c2 int
	r2.stop(StopScope{Kind: StopTool, Name: "other"})
	if _, err := r2.agent(&seqModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}, &countingTool{name: "charge", calls: &c2}).Run(ctx, "run1", UserText("go")); err != nil || c2 != 1 {
		t.Fatalf("a stop on another tool: %v %d", err, c2)
	}
}

// The check after the claim: a stop committed after the cached check but before the call begins is
// seen by the synchronous read; the won claim is recorded as not started, the tool is not called, and a
// resume after the lift claims the call afresh and runs it once.
func TestStopEngine_StopAfterTheClaimVoidsTheAttempt(t *testing.T) {
	ctx := context.Background()
	r := newStopRig(t, NewMemStore(), WithStopPoll(time.Hour)) // a cached check never reads again
	var calls int
	tool := &countingTool{name: "charge", calls: &calls}
	m := &seqModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	// Prime the cache with no stop, then set one: only a synchronous read can see it.
	if h := r.sw.Hold(ctx, RunRef{RunID: "x"}, StopQuery{Tool: "charge"}, true, true); h != nil {
		t.Fatal(h)
	}
	e := r.stop(StopScope{Kind: StopTool, Name: "charge"})
	_, err := r.agent(m, tool).Run(ctx, "run1", UserText("go"))
	asStopped(t, err)
	recs, _ := r.j.History(ctx, "run1")
	if calls != 0 || !r.hasKey("run1", toolAttemptStep("c1")) || len(liveAttempts(recs)) != 0 {
		t.Fatalf("calls %d, marker %v, live attempts %d (the marker must stand, voided by a not-started record)", calls, r.hasKey("run1", toolAttemptStep("c1")), len(liveAttempts(recs)))
	}
	r.lift(e)
	r.sw.Hold(ctx, RunRef{RunID: "x"}, StopQuery{}, false, true) // a poll: the cache sees the lift
	if _, err := r.agent(m, tool).Run(ctx, "run1", UserText("go")); err != nil || calls != 1 {
		t.Fatalf("after the lift: %v calls %d", err, calls)
	}
}

// A call that has begun finishes and is journaled; the stop then holds the run at the next turn, and the
// resume does not run the call again.
func TestStopEngine_InFlightCallFinishes(t *testing.T) {
	ctx := context.Background()
	r := newStopRig(t, NewMemStore(), WithStopPoll(time.Hour))
	var calls int
	var global StopEntry
	tool := MustFunc("charge", "", func(context.Context, struct{}) (string, error) {
		calls++
		global = r.stop(StopScope{Kind: StopGlobal})                                  // set while the call is running
		r.sw.Hold(context.Background(), RunRef{RunID: "x"}, StopQuery{}, false, true) // a poll sees it
		return "ok", nil
	})
	m := &seqModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	_, err := r.agent(m, tool).Run(ctx, "run1", UserText("go"))
	st := asStopped(t, err)
	if calls != 1 || !r.hasKey("run1", ToolResultStep("c1")) || st.StopID != global.ID || m.calls.Load() != 1 {
		t.Fatalf("calls %d result recorded %v model calls %d %+v", calls, r.hasKey("run1", ToolResultStep("c1")), m.calls.Load(), st)
	}
	r.lift(global)
	r.sw.Hold(ctx, RunRef{RunID: "x"}, StopQuery{}, false, true)
	if _, err := r.agent(m, tool).Run(ctx, "run1", UserText("go")); err != nil || calls != 1 {
		t.Fatalf("after the lift: %v calls %d (the call must not run again)", err, calls)
	}
}

// A claimed attempt with no result is never re-fired by a stop or by its lift: it halts, as before.
func TestStopEngine_LiveMarkerIsNotRefired(t *testing.T) {
	ctx := context.Background()
	r := newStopRig(t, NewMemStore(), WithStopPoll(10*time.Millisecond))
	var calls int
	tool := &countingTool{name: "charge", calls: &calls}
	m := &seqModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	// A live marker: the claim is written and nothing after it (a crash).
	if _, err := r.agent(&seqModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`)}}, MustFunc("charge", "", func(context.Context, struct{}) (string, error) {
		return "", ErrToolOutcomeUnknown
	})).Run(ctx, "run1", UserText("go")); err == nil {
		t.Fatal("expected the call to leave an unknown outcome")
	}
	e := r.stop(StopScope{Kind: StopGlobal})
	r.lift(e)
	time.Sleep(15 * time.Millisecond)
	_, err := r.agent(m, tool).Run(ctx, "run1", UserText("go"))
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) || calls != 0 {
		t.Fatalf("err = %v calls %d, want an OutcomeUnknown halt and no call", err, calls)
	}
}

// failLoad fails every Load of the control run while on is set.
type failLoad struct {
	Store
	on *atomic.Bool
}

func (f failLoad) Unwrap() Store { return f.Store }
func (f failLoad) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	if runID == StopControlRun && f.on.Load() {
		return func(yield func(Entry, error) bool) { yield(Entry{}, errCovBlip) }
	}
	return f.Store.Load(ctx, runID, after)
}

// When the control run cannot be read and the last read is past the staleness bound, a side-effect call
// is held (fail closed); a read-only call and a model turn are not.
func TestStopEngine_FailsClosedWhenTheControlRunCannotBeRead(t *testing.T) {
	ctx := context.Background()
	var down atomic.Bool
	var seen []error
	r := newStopRig(t, failLoad{NewMemStore(), &down}, WithStopPoll(10*time.Millisecond), WithStopStaleness(0), WithStopErrors(func(err error) { seen = append(seen, err) }))
	var charges, lookups int
	charge := &countingTool{name: "charge", calls: &charges}
	lookup := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &lookups}
	// The control run exists (so the guard is satisfied) and was read once.
	r.stop(StopScope{Kind: StopTool, Name: "other"})
	if h := r.sw.Hold(ctx, RunRef{RunID: "x"}, StopQuery{}, false, true); h != nil {
		t.Fatal(h)
	}
	down.Store(true)
	time.Sleep(15 * time.Millisecond)
	m := &seqModel{turns: [][]Emit{toolTurn("c1", "lookup", `{}`), toolTurn("c2", "charge", `{}`), textTurn("done")}}
	_, err := r.agent(m, charge, lookup).Run(ctx, "run1", UserText("go"))
	st := asStopped(t, err)
	if st.Cause != StopCauseUnreadable || lookups != 1 || charges != 0 || len(seen) == 0 {
		t.Fatalf("%+v lookups %d charges %d errors %d", st, lookups, charges, len(seen))
	}
	down.Store(false)
	if _, err := r.agent(m, charge, lookup).Run(ctx, "run1", UserText("go")); err != nil || charges != 1 {
		t.Fatalf("after the control run is readable again: %v charges %d", err, charges)
	}
	// Within the staleness bound the last read stands for a side-effect call.
	r2 := newStopRig(t, failLoad{NewMemStore(), &down}, WithStopPoll(10*time.Millisecond), WithStopStaleness(time.Hour))
	r2.stop(StopScope{Kind: StopTool, Name: "other"})
	r2.sw.Hold(ctx, RunRef{RunID: "x"}, StopQuery{}, false, true)
	down.Store(true)
	var c2 int
	if _, err := r2.agent(&seqModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}, &countingTool{name: "charge", calls: &c2}).Run(ctx, "run2", UserText("go")); err != nil || c2 != 1 {
		t.Fatalf("within the staleness bound: %v %d", err, c2)
	}
}

// A process whose agent has no switch refuses to run on a store whose control run holds a record.
func TestStopEngine_AgentWithoutASwitchIsRefused(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	r := newStopRig(t, store)
	m := &seqModel{turns: [][]Emit{textTurn("done")}}
	if _, err := mustNew(m, mustJournal(NewMemStore())).Run(ctx, "ok", UserText("go")); err != nil {
		t.Fatalf("a store with no control run: %v", err)
	}
	r.stop(StopScope{Kind: StopTool, Name: "x"})
	if _, err := mustNew(m, r.j).Run(ctx, "run1", UserText("go")); !errors.Is(err, ErrConfig) {
		t.Fatalf("an agent without a switch on a store with a control run: %v", err)
	}
	if _, err := r.agent(m).Run(ctx, "run1", UserText("go")); err != nil {
		t.Fatalf("with a switch: %v", err)
	}
}

// A pass of Recover lists nothing while a global stop is active; a scoped stop is the drive's business, and
// a run it holds is a pause, not a failure.
func TestStopEngine_RecoverRespectsTheStop(t *testing.T) {
	ctx := context.Background()
	r := newStopRig(t, NewMemStore(), WithStopPoll(10*time.Millisecond))
	var calls int
	tool := &countingTool{name: "charge", calls: &calls}
	m := &seqModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	// An unfinished run: the first drive's model fails after the stop was set so it leaves run:start only.
	e := r.stop(StopScope{Kind: StopTool, Name: "charge"})
	if _, err := r.agent(m, tool).Run(ctx, "run1", UserText("go")); err == nil {
		t.Fatal("expected a hold")
	}
	a := r.agent(m, tool)
	resume := ResumeAgent(a)
	n, err := Recover(ctx, r.j, resume, WithRecoverStop(r.sw))
	if err != nil || calls != 0 {
		t.Fatalf("a scoped stop: %d %v calls %d", n, err, calls)
	}
	g := r.stop(StopScope{Kind: StopGlobal})
	time.Sleep(15 * time.Millisecond)
	driven := 0
	n, err = Recover(ctx, r.j, func(context.Context, string, RunStart) error { driven++; return nil }, WithRecoverStop(r.sw))
	if err != nil || n != 0 || driven != 0 {
		t.Fatalf("a global stop: %d %v driven %d", n, err, driven)
	}
	r.lift(g)
	r.lift(e)
	time.Sleep(15 * time.Millisecond)
	if _, err := Recover(ctx, r.j, resume, WithRecoverStop(r.sw)); err != nil || calls != 1 {
		t.Fatalf("after the lifts: %v calls %d", err, calls)
	}
}

// A stop that blocks compensation holds a saga's rollback before the compensation runs, and the rollback
// finishes after the lift; by default a stop does not hold compensation.
func TestStopEngine_CompensationIsHeldOnlyWhenBlocked(t *testing.T) {
	for _, block := range []bool{false, true} {
		ctx := context.Background()
		r := newStopRig(t, NewMemStore(), WithStopPoll(10*time.Millisecond))
		l := newLedger()
		m := &seqModel{turns: [][]Emit{toolTurn("a1", "A", `{}`), toolTurn("b1", "boom", `{}`)}}
		a := r.agent(m, l.write("A"), failTool("boom"))
		// The stop lands after the forward steps ran: set it from the failing tool's neighbour, at the
		// start of the rollback, by covering tool A (a forward call of A is already done).
		comp := CompensationAllow
		if block {
			comp = CompensationBlock
		}
		var e StopEntry
		tool := MustFunc("boom", "", func(context.Context, struct{}) (struct{}, error) {
			e = r.stop(StopScope{Kind: StopTool, Name: "A"}, func(q *StopRequest) { q.Compensation = comp })
			return struct{}{}, errors.New("boom failed")
		})
		a = r.agent(m, l.write("A"), tool)
		_, err := a.Run(ctx, "run1", UserText("go"), WithSaga())
		if !block {
			var ab *SagaAborted
			if !errors.As(err, &ab) || l.undoCount["A"] != 1 {
				t.Fatalf("a stop that allows compensation: %v undo %d", err, l.undoCount["A"])
			}
			continue
		}
		asStopped(t, err)
		if l.undoCount["A"] != 0 {
			t.Fatalf("a blocked compensation ran: %d", l.undoCount["A"])
		}
		r.lift(e)
		time.Sleep(15 * time.Millisecond)
		_, err = a.Run(ctx, "run1", UserText("go"), WithSaga())
		var ab *SagaAborted
		if !errors.As(err, &ab) || l.undoCount["A"] != 1 {
			t.Fatalf("after the lift: %v undo %d", err, l.undoCount["A"])
		}
	}
}

// Hold: a basic control that sees a stop set under a signed record cannot judge the stops: a side-effect
// call is held (StopCauseUnknowable), a read-only call, a model turn and a compensation are not.
func TestStopSwitch_UnknowableHoldsSideEffectsOnly(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	a := audited(t, store, auditedPolicy(1, false))
	if _, err := a.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "elsewhere"}, By: "ops1", Signer: sign("ops1")}); err != nil {
		t.Fatal(err)
	}
	r := newStopRig(t, store)
	if h := r.sw.Hold(ctx, RunRef{RunID: "x"}, StopQuery{Tool: "charge"}, true, true); h == nil || h.Cause != StopCauseUnknowable {
		t.Fatalf("a side-effect call: %+v", h)
	}
	for name, h := range map[string]*Stopped{
		"read-only":    r.sw.Hold(ctx, RunRef{RunID: "x"}, StopQuery{Tool: "lookup"}, false, true),
		"turn":         r.sw.Hold(ctx, RunRef{RunID: "x"}, StopQuery{}, false, false),
		"compensation": r.sw.Hold(ctx, RunRef{RunID: "x"}, StopQuery{Tool: "charge", Compensation: true}, true, true),
	} {
		if h != nil {
			t.Fatalf("%s held: %+v", name, h)
		}
	}
}

// A switch reads the control run incrementally: a second check reads nothing new, and a stop set later is
// folded without reading the earlier records again.
func TestStopSwitch_ReadsIncrementally(t *testing.T) {
	ctx := context.Background()
	counting := &countLoad{Store: NewMemStore()}
	r := newStopRig(t, counting, WithStopPoll(time.Hour))
	r.stop(StopScope{Kind: StopTool, Name: "a"})
	delta := func(f func()) int64 {
		before := counting.entries.Load()
		f()
		return counting.entries.Load() - before
	}
	hold := func(tool string) *Stopped {
		return r.sw.Hold(ctx, RunRef{}, StopQuery{Tool: tool}, true, true)
	}
	if n := delta(func() { hold("x") }); n < 2 {
		t.Fatalf("the first read loaded %d entries", n)
	}
	if n := delta(func() { hold("x") }); n != 0 {
		t.Fatalf("a read with nothing new loaded %d entries", n)
	}
	r.stop(StopScope{Kind: StopTool, Name: "b"})
	var h *Stopped
	if n := delta(func() { h = hold("b") }); h == nil || n != 1 {
		t.Fatalf("%+v: a new stop loaded %d entries, want 1", h, n)
	}
}

type countLoad struct {
	Store
	entries atomic.Int64
	loads   atomic.Int64 // Loads of the control run
}

func (c *countLoad) Unwrap() Store { return c.Store }
func (c *countLoad) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	return func(yield func(Entry, error) bool) {
		if runID == StopControlRun {
			c.loads.Add(1)
		}
		for e, err := range c.Store.Load(ctx, runID, after) {
			if runID == StopControlRun && err == nil {
				c.entries.Add(1)
			}
			if !yield(e, err) {
				return
			}
		}
	}
}

// RecoverLoop lists nothing while a global stop is active and drives the unfinished run after the lift.
func TestStopEngine_RecoverLoopRespectsAGlobalStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newStopRig(t, NewMemStore(), WithStopPoll(10*time.Millisecond))
	var calls atomic.Int32
	tool := MustFunc("charge", "", func(context.Context, struct{}) (string, error) { calls.Add(1); return "ok", nil })
	m := &seqModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	e := r.stop(StopScope{Kind: StopTool, Name: "charge"})
	if _, err := r.agent(m, tool).Run(ctx, "run1", UserText("go")); err == nil {
		t.Fatal("expected a hold")
	}
	g := r.stop(StopScope{Kind: StopGlobal})
	r.lift(e)
	time.Sleep(20 * time.Millisecond) // past the poll interval: the switch has read the global stop
	var driven atomic.Int32
	resume := ResumeAgent(r.agent(m, tool))
	done := make(chan error, 1)
	go func() {
		done <- RecoverLoop(ctx, r.j, func(c context.Context, id string, st RunStart) error {
			driven.Add(1)
			return resume(c, id, st)
		}, WithRecoverStop(r.sw), WithRecoverInterval(20*time.Millisecond))
	}()
	time.Sleep(150 * time.Millisecond)
	if driven.Load() != 0 || calls.Load() != 0 {
		t.Fatalf("a global stop: driven %d calls %d", driven.Load(), calls.Load())
	}
	r.lift(g)
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if calls.Load() != 1 {
		t.Fatalf("after the lift the run was not driven: calls %d", calls.Load())
	}
}

// The guard that refuses an agent with no switch costs one Load of the control run per store every few
// seconds, not one per drive: a steady-state drive adds nothing.
func TestStopEngine_GuardIsCachedPerStore(t *testing.T) {
	ctx := context.Background()
	counting := &countLoad{Store: NewMemStore()}
	j := mustJournal(counting)
	for _, id := range []string{"r1", "r2", "r3", "r4"} {
		if _, err := mustNew(&seqModel{turns: [][]Emit{textTurn("done")}}, j).Run(ctx, id, UserText("go")); err != nil {
			t.Fatal(err)
		}
	}
	if n := counting.loads.Load(); n != 1 {
		t.Fatalf("%d Loads of the control run over four drives, want 1", n)
	}
}

// partialLoad makes a Load of the control run yield its entries and then fail, while on is set: a
// connection reset at the end of a read.
type partialLoad struct {
	Store
	on *atomic.Bool
}

func (p partialLoad) Unwrap() Store { return p.Store }
func (p partialLoad) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	return func(yield func(Entry, error) bool) {
		for e, err := range p.Store.Load(ctx, runID, after) {
			if !yield(e, err) {
				return
			}
		}
		if runID == StopControlRun && p.on.Load() {
			yield(Entry{}, errCovBlip)
		}
	}
}

// A read that delivers a stop and then fails must not lose it: the entries are not folded and the cursor
// does not move, so the next read, which succeeds, finds the stop.
func TestStopSwitch_PartialReadDoesNotLoseAStop(t *testing.T) {
	ctx := context.Background()
	var broken atomic.Bool
	r := newStopRig(t, partialLoad{NewMemStore(), &broken}, WithStopPoll(time.Hour), WithStopStaleness(time.Hour))
	hold := func() *Stopped { return r.sw.Hold(ctx, RunRef{RunID: "x"}, StopQuery{Tool: "charge"}, true, true) }
	if h := hold(); h != nil {
		t.Fatal(h)
	}
	r.stop(StopScope{Kind: StopGlobal})
	broken.Store(true)
	if h := hold(); h != nil && h.Cause == StopCauseStop {
		t.Fatal("a stop was seen through a failed read; it is not folded until a read completes") // the cache is within the bound
	}
	broken.Store(false)
	if h := hold(); h == nil || h.Cause != StopCauseStop {
		t.Fatalf("the stop that arrived in a failed read was lost: %+v", h)
	}
}

// A record this process cannot decode, and a context that ends mid-read, do not advance the cursor either.
func TestStopSwitch_FailedReadsKeepTheCursor(t *testing.T) {
	r := newStopRig(t, NewMemStore(), WithStopPoll(time.Hour), WithStopStaleness(time.Hour))
	hold := func(ctx context.Context) *Stopped {
		return r.sw.Hold(ctx, RunRef{RunID: "x"}, StopQuery{Tool: "charge"}, true, true)
	}
	hold(context.Background())
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	r.stop(StopScope{Kind: StopGlobal})
	hold(cancelled) // a read that cannot complete
	if h := hold(context.Background()); h == nil || h.Cause != StopCauseStop {
		t.Fatalf("after a cancelled read: %+v", h)
	}
	// An undecodable entry after the header: reads fail until it is gone; the stop before it is kept as read.
	r2 := newStopRig(t, NewMemStore(), WithStopPoll(time.Hour), WithStopStaleness(0))
	r2.stop(StopScope{Kind: StopTool, Name: "a"})
	if _, _, err := r2.store.Insert(context.Background(), StopControlRun, "@stop/zz-bad", []byte("not a record")); err != nil {
		t.Fatal(err)
	}
	if h := r2.sw.Hold(context.Background(), RunRef{RunID: "x"}, StopQuery{Tool: "charge"}, true, true); h == nil || h.Cause != StopCauseUnreadable {
		t.Fatalf("an undecodable entry must make a side-effect call fail closed, not pass: %+v", h)
	}
	// And it keeps doing so: the cursor did not move past the entry the read could not decode.
	if h := r2.sw.Hold(context.Background(), RunRef{RunID: "x"}, StopQuery{Tool: "charge"}, true, true); h == nil || h.Cause != StopCauseUnreadable {
		t.Fatalf("the second read after an undecodable entry passed: %+v", h)
	}
}

// With nothing read yet and an unreadable control run, only side-effect calls are held: a read-only
// call, a turn and a drive start proceed.
func TestStopSwitch_NothingReadYetHoldsOnlySideEffects(t *testing.T) {
	ctx := context.Background()
	var down atomic.Bool
	down.Store(true)
	r := newStopRig(t, failLoad{NewMemStore(), &down}, WithStopPoll(10*time.Millisecond), WithStopStaleness(time.Hour))
	if h := r.sw.Hold(ctx, RunRef{RunID: "x"}, StopQuery{Tool: "charge"}, true, true); h == nil || h.Cause != StopCauseUnreadable {
		t.Fatalf("a side-effect call: %+v", h)
	}
	for name, h := range map[string]*Stopped{
		"read-only": r.sw.Hold(ctx, RunRef{RunID: "x"}, StopQuery{Tool: "lookup"}, false, true),
		"turn":      r.sw.Hold(ctx, RunRef{RunID: "x"}, StopQuery{}, false, false),
	} {
		if h != nil {
			t.Fatalf("%s held: %+v", name, h)
		}
	}
}

// slowLoad blocks a Load of the control run until released, signalling when one has started.
type slowLoad struct {
	Store
	started chan struct{}
	release chan struct{}
	slow    atomic.Bool
}

func (s *slowLoad) Unwrap() Store { return s.Store }
func (s *slowLoad) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	if runID == StopControlRun && s.slow.Load() {
		return func(yield func(Entry, error) bool) {
			select {
			case s.started <- struct{}{}:
			default:
			}
			select {
			case <-s.release:
			case <-ctx.Done():
				yield(Entry{}, ctx.Err())
				return
			}
			for e, err := range s.Store.Load(ctx, runID, after) {
				if !yield(e, err) {
					return
				}
			}
		}
	}
	return s.Store.Load(ctx, runID, after)
}

// One slow read does not hold up the cached checks, and a caller waiting for the reader gives up when its
// context ends.
func TestStopSwitch_SlowReadDoesNotBlockOthers(t *testing.T) {
	st := &slowLoad{Store: NewMemStore(), started: make(chan struct{}, 1), release: make(chan struct{})}
	r := newStopRig(t, st, WithStopPoll(time.Hour), WithStopStaleness(time.Hour))
	bg := context.Background()
	r.sw.Hold(bg, RunRef{RunID: "x"}, StopQuery{}, false, true) // primes the cache
	st.slow.Store(true)
	done := make(chan *Stopped, 1)
	go func() { done <- r.sw.Hold(bg, RunRef{RunID: "x"}, StopQuery{Tool: "charge"}, true, true) }() // the slow read
	<-st.started
	// A cached check is answered from the cache while the read is in flight.
	got := make(chan *Stopped, 1)
	go func() { got <- r.sw.Hold(bg, RunRef{RunID: "y"}, StopQuery{Tool: "charge"}, false, false) }()
	select {
	case h := <-got:
		if h != nil {
			t.Fatal(h)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cached check waited for a slow read")
	}
	// A synchronous caller waiting for the reader stops waiting when its context ends.
	wctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()
	if h := r.sw.Hold(wctx, RunRef{RunID: "z"}, StopQuery{Tool: "charge"}, true, true); h != nil && h.Cause != StopCauseUnreadable && h.Cause != StopCauseStop {
		t.Fatal(h)
	}
	close(st.release)
	<-done
}
