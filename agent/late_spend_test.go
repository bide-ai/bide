package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Late spend: a model request still in flight when its turn fails is journaled and replayed.

// faultStore wraps a Store with deterministic faults.
type faultStore struct {
	Store
	mu sync.Mutex
	// commitThenErr: the Insert of this name lands, then reports an error (once). A3.
	commitThenErr map[string]bool
	// failNoCommit: the Insert of this name fails without landing (once).
	failNoCommit map[string]bool
	// afterInsert runs after an Insert of name lands (before any injected error).
	afterInsert func(name string)
	// failGet: while true for a name, Get of it errors.
	failGet map[string]bool
	// armGetOnFault: when the commitThenErr fault of this name fires, failGet[name] is set.
	armGetOnFault map[string]bool
	// commitThenErrPrefix: the first Insert of a name with this prefix lands, then reports an error.
	commitThenErrPrefix string
	// failNoCommitPrefix: the first Insert of a name with this prefix fails without landing.
	failNoCommitPrefix string
}

func newFaultStore() *faultStore {
	return &faultStore{Store: NewMemStore(), commitThenErr: map[string]bool{}, failNoCommit: map[string]bool{}, failGet: map[string]bool{}, armGetOnFault: map[string]bool{}}
}

func (s *faultStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	s.mu.Lock()
	if s.failNoCommitPrefix != "" && strings.HasPrefix(name, s.failNoCommitPrefix) {
		s.failNoCommitPrefix = ""
		s.mu.Unlock()
		return Entry{}, false, errors.New("injected: write failed, not committed")
	}
	if s.failNoCommit[name] {
		delete(s.failNoCommit, name)
		s.mu.Unlock()
		return Entry{}, false, errors.New("injected: write failed, not committed")
	}
	s.mu.Unlock()
	e, ok, err := s.Store.Insert(ctx, runID, name, data)
	if err == nil && s.afterInsert != nil {
		s.afterInsert(name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil && s.commitThenErrPrefix != "" && strings.HasPrefix(name, s.commitThenErrPrefix) {
		s.commitThenErrPrefix = ""
		return Entry{}, false, errors.New("injected: connection reset after commit")
	}
	if err == nil && s.commitThenErr[name] {
		delete(s.commitThenErr, name)
		if s.armGetOnFault[name] {
			s.failGet[name] = true
		}
		return Entry{}, false, errors.New("injected: connection reset after commit")
	}
	return e, ok, err
}

func (s *faultStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	s.mu.Lock()
	fail := s.failGet[name]
	s.mu.Unlock()
	if fail {
		return Entry{}, false, errors.New("injected: read failed")
	}
	return s.Store.Get(ctx, runID, name)
}

// answerAndLeave starts a background request to bg (a copy of the call with its Model set, as
// Hedge builds a backup), waits until it is in the model, then answers through next.
func answerAndLeave(bg *lateModel) Middleware {
	return func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			bgc := call
			bgc.Model = bg
			go func() { _, _ = next(context.WithoutCancel(ctx), bgc) }()
			<-bg.started
			return next(ctx, call)
		}
	}
}

func names(t *testing.T, d *Journal, runID string) []string {
	t.Helper()
	recs, err := d.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range recs {
		out = append(out, r.Name)
	}
	return out
}

var late = Usage{InputTokens: 7, OutputTokens: 3}

// BUG candidate (F4): the @llm/0 write reports failure but landed, and a request still in flight
// ends during the failure path. The landed lookup keeps the record's spend out of @spend/0, but
// the in-flight request's spend is still written as @spend/0, which Replay reads as a failed
// turn after turn 0. The resumed original finishes on the recorded final turn 0 and never asks
// for another, so the replayed run never consumes that failed turn: its Spend drops the late
// request, although Replay promises the replayed Result.Spend matches the original's.
func TestAdv104_F4LandedResidualSpendIsAFailedTurnInReplay(t *testing.T) {
	ctx := context.Background()
	bg := &lateModel{u: late, started: make(chan struct{}), gate: make(chan struct{})}
	st := newFaultStore()
	st.commitThenErr[modelStep(0)] = true
	st.afterInsert = func(name string) {
		if name == modelStep(0) {
			close(bg.gate) // the in-flight request ends after the record was built and written
		}
	}
	j, err := NewJournal(st)
	if err != nil {
		t.Fatal(err)
	}
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	if _, err := mustNew(m, j, WithMiddleware(answerAndLeave(bg))).Run(ctx, "r", UserText("go")); err == nil {
		t.Fatal("want the write failure")
	}
	res, err := mustNew(&scriptModel{}, j).Run(ctx, "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	want := billed
	addUsage(&want, late)
	if res.Spend != want {
		t.Fatalf("original Spend = %+v, want %+v", res.Spend, want)
	}
	rm, err := Replay(ctx, j, "r")
	if err != nil {
		t.Fatal(err)
	}
	rres, rerr := mustNew(rm, memJournal()).Run(ctx, "r", UserText("go"))
	t.Logf("original journal: %v", names(t, j, "r"))
	if rerr != nil {
		t.Fatalf("replayed run failed: %v", rerr)
	}
	if rres.Spend != res.Spend {
		t.Fatalf("replayed Spend = %+v, original Spend = %+v: Replay drops the spend journaled after a landed record", rres.Spend, res.Spend)
	}
}

// BUG candidate (F4): the @llm/0 write reports failure but landed, and the lookup that should
// tell so fails too. The record's spend is then journaled a second time in @spend/0, and every
// later invocation counts the one request twice.
func TestAdv104_F4LookupErrorDoubleCounts(t *testing.T) {
	ctx := context.Background()
	st := newFaultStore()
	st.commitThenErr[modelStep(0)] = true
	st.armGetOnFault[modelStep(0)] = true
	j, err := NewJournal(st)
	if err != nil {
		t.Fatal(err)
	}
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	if _, err := mustNew(m, j).Run(ctx, "r", UserText("go")); err == nil {
		t.Fatal("want the write failure")
	}
	st.mu.Lock()
	st.failGet = map[string]bool{}
	st.mu.Unlock()
	res, err := mustNew(&scriptModel{}, j).Run(ctx, "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("journal: %v", names(t, j, "r"))
	if res.Spend != billed {
		t.Fatalf("Spend = %+v, want the one request's %+v (counted once)", res.Spend, billed)
	}
}

// BUG candidate (F3 bound): a turn that fails with a request still in flight waits
// lateRequestWait in the failure path, then leave's settle waits lateRequestWait again. The
// documented bound is two seconds.
func TestAdv104_F3FailedTurnWaitsTwice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		bg := &lateModel{u: late, started: make(chan struct{}), gate: gate}
		var wg sync.WaitGroup
		fail := func(next ModelHandler) ModelHandler {
			return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
				wg.Add(1)
				go func() { defer wg.Done(); _, _ = next(context.WithoutCancel(ctx), call) }()
				<-bg.started
				return ModelResponse{}, errors.New("gave up")
			}
		}
		start := time.Now()
		_, err := mustNew(bg, memJournal(), WithMiddleware(fail)).Run(context.Background(), "r", UserText("go"))
		elapsed := time.Since(start)
		close(gate)
		wg.Wait()
		if err == nil {
			t.Fatal("want the failure")
		}
		t.Logf("run returned after %v (lateRequestWait %v)", elapsed, lateRequestWait)
		if elapsed > lateRequestWait {
			t.Fatalf("the failed run waited %v for a stuck request, past the documented bound %v", elapsed, lateRequestWait)
		}
	})
}

// Probe (expected to hold): the late spend record's write errors after committing (A3). The run
// reports the error and writes no run:complete; the re-drive counts the late spend once and
// writes no second late record.
func TestAdv104_LateSpendCommittedThenErrored(t *testing.T) {
	ctx := context.Background()
	bg := &lateModel{u: late, started: make(chan struct{}), gate: make(chan struct{})}
	st := newFaultStore()
	st.commitThenErrPrefix = lateSpendPrefix
	st.afterInsert = func(name string) {
		if name == modelStep(0) {
			close(bg.gate)
		}
	}
	j, _ := NewJournal(st)
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	if _, err := mustNew(m, j, WithMiddleware(answerAndLeave(bg))).Run(ctx, "r", UserText("go")); err == nil {
		t.Fatal("want the late spend write's error")
	}
	res, err := mustNew(&scriptModel{}, j).Run(ctx, "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	want := billed
	addUsage(&want, late)
	ns := names(t, j, "r")
	t.Logf("journal: %v", ns)
	if res.Spend != want || strings.Count(strings.Join(ns, " "), lateSpendPrefix) != 1 {
		t.Fatalf("Spend = %+v, want %+v, with one late record", res.Spend, want)
	}
}

// Probe (expected to hold): the late record lands and run:complete fails without landing (a
// crash between the two). The re-drive counts the late spend once and writes run:complete.
func TestAdv104_CrashBetweenLateSpendAndComplete(t *testing.T) {
	ctx := context.Background()
	bg := &lateModel{u: late, started: make(chan struct{}), gate: make(chan struct{})}
	st := newFaultStore()
	st.failNoCommit[runCompleteStep] = true
	st.afterInsert = func(name string) {
		if name == modelStep(0) {
			close(bg.gate)
		}
	}
	j, _ := NewJournal(st)
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	if _, err := mustNew(m, j, WithMiddleware(answerAndLeave(bg))).Run(ctx, "r", UserText("go")); err == nil {
		t.Fatal("want the run:complete write's error")
	}
	res, err := mustNew(&scriptModel{}, j).Run(ctx, "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	want := billed
	addUsage(&want, late)
	ns := names(t, j, "r")
	t.Logf("journal: %v", ns)
	if res.Spend != want || strings.Count(strings.Join(ns, " "), lateSpendPrefix) != 1 || ns[len(ns)-1] != runCompleteStep {
		t.Fatalf("Spend = %+v, want %+v, one late record, run:complete last", res.Spend, want)
	}
}

// Probe (expected to hold): a pause with a request in flight journals its spend once; the resume
// does not write it again.
func TestAdv104_PauseThenResumeLateSpendOnce(t *testing.T) {
	ctx := context.Background()
	bg := &lateModel{u: late, started: make(chan struct{}), gate: make(chan struct{})}
	st := newFaultStore()
	st.afterInsert = func(name string) {
		if name == modelStep(0) {
			close(bg.gate)
		}
	}
	j, _ := NewJournal(st)
	var charged int
	charge := &countingTool{name: "charge", approval: SingleApproval(), calls: &charged}
	m := &scriptModel{turns: [][]Emit{toolTurnWithUsage("c1", "charge", `{}`, billed)}}
	_, err := mustNew(m, j, WithTools(charge), WithMiddleware(answerAndLeave(bg))).Run(ctx, "r", UserText("go"))
	var pend *ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("err = %v, want a pause", err)
	}
	if err := Approve(ctx, j, "r", pend.ToolUseID, true); err != nil {
		t.Fatal(err)
	}
	res, err := mustNew(&scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}, j, WithTools(charge)).Run(ctx, "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	want := billed
	addUsage(&want, billed)
	addUsage(&want, late)
	ns := names(t, j, "r")
	t.Logf("journal: %v", ns)
	if res.Spend != want || strings.Count(strings.Join(ns, " "), lateSpendPrefix) != 1 {
		t.Fatalf("Spend = %+v, want %+v with one late record", res.Spend, want)
	}
}

// BUG candidate (F1 claim): OnAnswer is documented to run once per turn "with the response the
// turn records". When the turn's record fails to write (not landed), fn has already run with a
// response the journal never holds, and the re-driven turn runs it again: two answers for one
// recorded turn.
func TestAdv104_OnAnswerRunsForUnrecordedResponse(t *testing.T) {
	ctx := context.Background()
	st := newFaultStore()
	st.failNoCommit[modelStep(0)] = true
	j, _ := NewJournal(st)
	var answers atomic.Int32
	key := new(int)
	count := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			call.OnAnswer(key, func(context.Context, ModelResponse) { answers.Add(1) })
			return next(ctx, call)
		}
	}
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("one", billed), textTurnWithUsage("two", billed)}}
	a := mustNew(m, j, WithMiddleware(count))
	if _, err := a.Run(ctx, "r", UserText("go")); err == nil {
		t.Fatal("want the write failure")
	}
	if _, err := a.Run(ctx, "r", UserText("go")); err != nil {
		t.Fatal(err)
	}
	var modelRecs int
	recs, _ := j.History(ctx, "r")
	for _, r := range recs {
		if r.Kind == StepModel {
			modelRecs++
		}
	}
	if int(answers.Load()) != modelRecs {
		t.Fatalf("OnAnswer ran %d times for %d recorded turns", answers.Load(), modelRecs)
	}
}

// blockOnce is a read-only tool whose first call signals blocked and waits for release; later
// calls return at once.
type blockOnce struct {
	once             sync.Once
	blocked, release chan struct{}
}

// Spec describes the tool to the agent (see Tool).
func (t *blockOnce) Spec() ToolSpec {
	return ToolSpec{Name: "wait", Description: "", Input: json.RawMessage(`{"type":"object"}`), Safety: Safety{ReadOnly: true}}
}

func (t *blockOnce) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	first := false
	t.once.Do(func() { first = true })
	if first {
		close(t.blocked)
		<-t.release
	}
	return json.RawMessage(`"ok"`), nil
}

// procStore is one process's handle on a shared store: a value, so each Journal over it shares
// in-flight steps with no other, and without the store's Leaser, as two processes of a store
// without leases.
type procStore struct{ Store }

// (a) Two drivers of one run each journal the late spend of their own requests: the one that
// started first must not lose its late record to the other's under one key.
func TestAdv104_TwoDriversLateSpend(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	journal := func() *Journal {
		j, err := NewJournal(procStore{mem})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	store := journal()
	tool := &blockOnce{blocked: make(chan struct{}), release: make(chan struct{})}
	lateB := Usage{InputTokens: 11}
	lateA := Usage{InputTokens: 13}
	bgB := &lateModel{u: lateB, started: make(chan struct{}), gate: make(chan struct{})}
	bgA := &lateModel{u: lateA, started: make(chan struct{}), gate: make(chan struct{})}
	// A's request ends once A's turn is recorded: its spend is late, journaled at A's end.
	fsA := &faultStore{Store: mem, commitThenErr: map[string]bool{}, failNoCommit: map[string]bool{}, failGet: map[string]bool{}, armGetOnFault: map[string]bool{}}
	fsA.afterInsert = func(name string) {
		if name == modelStep(1) {
			close(bgA.gate)
		}
	}
	jA, err := NewJournal(procStore{fsA})
	if err != nil {
		t.Fatal(err)
	}

	// Driver B: turn 0 calls the blocking tool, with a request of its own still in flight.
	mB := &scriptModel{turns: [][]Emit{toolTurnWithUsage("c1", "wait", `{}`, billed)}}
	doneB := make(chan error, 1)
	go func() {
		_, err := mustNew(mB, journal(), WithTools(tool), WithMiddleware(answerAndLeave(bgB))).Run(ctx, "r", UserText("go"))
		doneB <- err
	}()
	<-tool.blocked
	// Driver A: resumes the run while B is blocked, runs the tool (retry-safe), answers turn 1
	// with a request of its own in flight, and completes.
	mA := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	if _, err := mustNew(mA, jA, WithTools(tool), WithMiddleware(answerAndLeave(bgA))).Run(ctx, "r", UserText("go")); err != nil {
		t.Fatal(err)
	}
	close(bgB.gate)
	close(tool.release)
	if err := <-doneB; err != nil {
		t.Fatal(err)
	}
	res, err := mustNew(&scriptModel{}, store, WithTools(tool)).Run(ctx, "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("journal: %v", names(t, store, "r"))
	want := billed
	addUsage(&want, billed)
	addUsage(&want, lateA)
	addUsage(&want, lateB)
	if res.Spend != want {
		t.Fatalf("Spend = %+v, want %+v; journal %v", res.Spend, want, names(t, store, "r"))
	}
}

// (b) A late spend record that precedes every model record still reaches the replayed run's
// spend: Replay carries it to the first turn.
func TestAdv104_ReplayLateRecordBeforeAnyTurn(t *testing.T) {
	ctx := context.Background()
	src := memJournal()
	if _, err := src.do(ctx, "r", lateSpendStep("x"), func(context.Context) (Record, error) {
		return Record{Kind: StepValue, DiscardedUsage: &late}, nil
	}); err != nil {
		t.Fatal(err)
	}
	msg := Message{Role: RoleAssistant, Parts: []Part{Text{Text: "done"}}}
	u := billed
	if _, err := src.do(ctx, "r", modelStep(0), func(context.Context) (Record, error) {
		return Record{Kind: StepModel, Message: &msg, Usage: &u, ModelTurn: &ModelTurn{Finish: FinishStop}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	rm, err := Replay(ctx, src, "r")
	if err != nil {
		t.Fatal(err)
	}
	res, err := mustNew(rm, memJournal()).Run(ctx, "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	want := billed
	addUsage(&want, late)
	if res.Spend != want {
		t.Fatalf("replayed Spend = %+v, want %+v", res.Spend, want)
	}
}

// rewrap is a Model decorator that forwards its inner model's events through a stream of its
// own, as a logging or metering wrapper does.
type rewrap struct{ inner Model }

func (w rewrap) Unwrap() Model { return w.inner }
func (w rewrap) Stream(ctx context.Context, req Request) (*Stream, error) {
	s, err := w.inner.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	return NewStreamFunc(ctx, func(send func(Emit) bool) {
		for ev, err := range s.Events() {
			if !send(Emit{Event: ev, Err: err}) {
				return
			}
		}
	}), nil
}

// (c) A decorator that re-wraps the replay stream keeps the replayed turn's recorded model.
func TestAdv104_ReplayThroughADecoratorKeepsModel(t *testing.T) {
	ctx := context.Background()
	src := memJournal()
	m := describedScript{&scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}}
	if _, err := mustNew(m, src).Run(ctx, "r", UserText("go")); err != nil {
		t.Fatal(err)
	}
	rm, err := Replay(ctx, src, "r")
	if err != nil {
		t.Fatal(err)
	}
	dst := memJournal()
	if _, err := mustNew(rewrap{rm}, dst).Run(ctx, "r", UserText("go")); err != nil {
		t.Fatal(err)
	}
	a, b := modelRecords(t, src, "r"), modelRecords(t, dst, "r")
	if a[0].Model() == nil || b[0].Model() == nil || *a[0].Model() != *b[0].Model() {
		t.Fatalf("replayed Model = %v, original %v", b[0].Model(), a[0].Model())
	}
}

// When a model record's write reports an error and the read that should settle its fate fails
// too, the turn's answer functions wait for the run's next drive in the process: the record
// landed, so they run then, once.
func TestAdv104_LookupErrorAnswerRunsOnNextDrive(t *testing.T) {
	ctx := context.Background()
	st := newFaultStore()
	st.commitThenErr[modelStep(0)] = true
	st.armGetOnFault[modelStep(0)] = true
	j, err := NewJournal(st)
	if err != nil {
		t.Fatal(err)
	}
	var answers int
	key := new(int)
	count := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			call.OnAnswer(key, func(context.Context, ModelResponse) { answers++ })
			return next(ctx, call)
		}
	}
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	if _, err := mustNew(m, j, WithMiddleware(count)).Run(ctx, "r", UserText("go")); err == nil {
		t.Fatal("want the write failure")
	}
	if answers != 0 {
		t.Fatalf("answers ran %d times before the record's fate was known", answers)
	}
	st.mu.Lock()
	st.failGet = map[string]bool{}
	st.mu.Unlock()
	if _, err := mustNew(&scriptModel{}, j).Run(ctx, "r", UserText("go")); err != nil {
		t.Fatal(err)
	}
	if answers != 1 {
		t.Fatalf("answers ran %d times, want once for the one recorded turn", answers)
	}
}

// gatedTurn answers after every gatedTurn sharing its gate has been called, so two drivers' calls
// of one turn are both made before either is recorded.
type gatedTurn struct {
	g    *twoGate
	text string
	u    Usage
}

type twoGate struct {
	mu   sync.Mutex
	n    int
	open chan struct{}
}

func (m *gatedTurn) Stream(context.Context, Request) (*Stream, error) {
	m.g.mu.Lock()
	if m.g.n++; m.g.n == 2 {
		close(m.g.open)
	}
	m.g.mu.Unlock()
	<-m.g.open
	ch := make(chan Emit, 2)
	ch <- Emit{Event: TextDelta{Text: m.text}}
	ch <- Emit{Event: Finish{Reason: FinishStop, Usage: m.u}}
	close(ch)
	return NewStream(ch), nil
}

// Two drivers of one run both answer its turn; one records it. The other's request was billed all
// the same: its spend is journaled as late spend, so the run's Spend counts both requests, and only
// the recorded turn's driver runs its answer functions.
func TestAdv104_LostRaceSpendIsLate(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	g := &twoGate{open: make(chan struct{})}
	uA, uB := Usage{InputTokens: 5}, Usage{InputTokens: 9}
	var wg sync.WaitGroup
	for _, m := range []*gatedTurn{{g: g, text: "a", u: uA}, {g: g, text: "b", u: uB}} {
		j, err := NewJournal(procStore{mem})
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := mustNew(m, j).Run(ctx, "r", UserText("go")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	j, _ := NewJournal(procStore{mem})
	res, err := mustNew(&scriptModel{}, j).Run(ctx, "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	want := uA
	addUsage(&want, uB)
	if res.Spend != want {
		t.Fatalf("Spend = %+v, want both requests' %+v; journal %v", res.Spend, want, names(t, j, "r"))
	}
}

// A late spend record whose write fails without landing is kept by the process and written by the
// run's next drive in it, so the run's Spend still counts the request, once.
func TestAdv104_FailedLateSpendWriteIsWrittenNextDrive(t *testing.T) {
	ctx := context.Background()
	bg := &lateModel{u: late, started: make(chan struct{}), gate: make(chan struct{})}
	st := newFaultStore()
	st.failNoCommitPrefix = lateSpendPrefix
	st.afterInsert = func(name string) {
		if name == modelStep(0) {
			close(bg.gate)
		}
	}
	j, err := NewJournal(st)
	if err != nil {
		t.Fatal(err)
	}
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	if _, err := mustNew(m, j, WithMiddleware(answerAndLeave(bg))).Run(ctx, "r", UserText("go")); err == nil {
		t.Fatal("want the late spend write's error")
	}
	res, err := mustNew(&scriptModel{}, j).Run(ctx, "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	want := billed
	addUsage(&want, late)
	if res.Spend != want {
		t.Fatalf("Spend = %+v, want %+v; journal %v", res.Spend, want, names(t, j, "r"))
	}
}

// Late spend, content check and kept spend (second round).

// C1: the content check of fix 4 compares the record the step built with the record the journal
// returns, decoded. A tool call whose args are not in compact JSON (a provider that streams
// `{"q": "x"}`) is compacted by the journal's encoding, so the decoded record never equals the
// built one: the turn's OnAnswer never runs, and its spend, taken for another driver's, is
// journaled again as late spend.
func TestAdv104c_NonCompactArgsFailContentCheck(t *testing.T) {
	ctx := context.Background()
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{
		toolTurnWithUsage("c1", "lookup", `{"q": "x"}`, billed),
		textTurnWithUsage("final", billed),
	}}
	var answers int
	key := new(int)
	count := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			call.OnAnswer(key, func(context.Context, ModelResponse) { answers++ })
			return next(ctx, call)
		}
	}
	res, err := mustNew(m, memJournal(), WithTools(tool), WithMiddleware(count)).Run(ctx, "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	want := billed
	addUsage(&want, billed)
	if answers != 2 || res.Spend != want {
		t.Fatalf("answers = %d, want 2; Spend = %+v, want %+v", answers, res.Spend, want)
	}
}

// lookupErrDrive drives run "r" through d once: the @llm/0 write fails without landing and the
// read that should settle it fails too, so the drive keeps the turn's spend for the next drive.
func lookupErrDrive(t *testing.T, st *faultStore, d *Journal) {
	t.Helper()
	st.mu.Lock()
	st.failNoCommit[modelStep(0)] = true
	st.mu.Unlock()
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	if _, err := mustNew(m, d, WithMiddleware(armGet(st))).Run(context.Background(), "r", UserText("go")); err == nil {
		t.Fatal("want the write failure")
	}
	st.mu.Lock()
	st.failGet = map[string]bool{}
	st.mu.Unlock()
}

// C2 control: the next drive through the same Journal journals the failed turn's spend.
func TestAdv104c_KeptSpendSameJournal(t *testing.T) {
	st := newFaultStore()
	j, _ := NewJournal(st)
	lookupErrDrive(t, st, j)
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	res, err := mustNew(m, j).Run(context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	want := billed
	addUsage(&want, billed)
	if res.Spend != want {
		t.Fatalf("Spend = %+v, want %+v", res.Spend, want)
	}
}

// C2: the kept spend is keyed by the *Journal pointer, not by the store's identity (the key
// claims and in-flight steps share across Journals, storeIdentity). The run's next drive in the
// same process through another Journal over the same store (a Journal per request) never sees it:
// the failed call's billed spend is never journaled, and the entry is never freed.
func TestAdv104c_KeptSpendOtherJournalSameStore(t *testing.T) {
	st := newFaultStore()
	j1, _ := NewJournal(st)
	lookupErrDrive(t, st, j1)
	j2, _ := NewJournal(st)
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	res, err := mustNew(m, j2).Run(context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	want := billed
	addUsage(&want, billed)
	pendingSpends.Lock()
	left := len(pendingSpends.m[pendingKey{j1, "r"}])
	pendingSpends.Unlock()
	if res.Spend != want || left != 0 {
		t.Fatalf("Spend = %+v, want %+v; entries still kept under the first Journal: %d", res.Spend, want, left)
	}
}

// C3: kept spend is never evicted. Each run whose lookup failed and that is not driven again in
// this process (the process that resumes it is another one, or the run is abandoned) leaves an
// entry, holding the drive's turn state and answer closures, for the life of the process.
func TestAdv104c_KeptSpendIsUnbounded(t *testing.T) {
	defer func(n int) { maxPendingSpends = n }(maxPendingSpends)
	maxPendingSpends = 16
	st := newFaultStore()
	j, _ := NewJournal(st)
	const n = 50
	for i := range n {
		st.mu.Lock()
		st.failNoCommit[modelStep(0)] = true
		st.mu.Unlock()
		m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
		_, _ = mustNew(m, j, WithMiddleware(armGet(st))).Run(context.Background(), "run-"+string(rune('a'+i%26))+string(rune('a'+i/26)), UserText("go"))
		st.mu.Lock()
		st.failGet = map[string]bool{}
		st.mu.Unlock()
	}
	pendingSpends.Lock()
	kept := 0
	for k := range pendingSpends.m {
		if k.store == any(j) || k.store == j.id {
			kept++
		}
	}
	pendingSpends.Unlock()
	if kept > maxPendingSpends {
		t.Fatalf("%d runs kept %d pending entries, past the bound %d", n, kept, maxPendingSpends)
	}
	// The oldest runs were dropped, the newest kept.
	pendingSpends.Lock()
	_, newest := pendingSpends.m[pendingKey{j.id, "run-" + string(rune('a'+(n-1)%26)) + string(rune('a'+(n-1)/26))}]
	_, oldest := pendingSpends.m[pendingKey{j.id, "run-aa"}]
	pendingSpends.Unlock()
	if !newest || oldest {
		t.Fatalf("newest kept %v, oldest kept %v: want the oldest dropped first", newest, oldest)
	}
	// Driving a kept run takes its entry, and the count of entries held stays exact.
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	if _, err := mustNew(m, j).Run(context.Background(), "run-"+string(rune('a'+(n-1)%26))+string(rune('a'+(n-1)/26)), UserText("go")); err != nil {
		t.Fatal(err)
	}
	pendingSpends.Lock()
	total := 0
	for _, ps := range pendingSpends.m {
		total += len(ps)
	}
	held := pendingSpends.n
	pendingSpends.Unlock()
	if held != total {
		t.Fatalf("pendingSpends counts %d entries, holds %d", held, total)
	}
}

// armGet makes reads of @llm/0 fail once the model has answered, before the turn's write.
func armGet(st *faultStore) Middleware {
	return func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			resp, err := next(ctx, call)
			st.mu.Lock()
			st.failGet[modelStep(0)] = true
			st.mu.Unlock()
			return resp, err
		}
	}
}

// Text with invalid UTF-8 is journaled as U+FFFD, so the record read back differs from the one
// the step built. The turn is still identified as the step's own (by the salt the journal
// stamps): its OnAnswer runs once and its spend is counted once.
func TestAdv104c_InvalidUTF8TextIsTheTurnsOwnRecord(t *testing.T) {
	ctx := context.Background()
	m := &scriptModel{turns: [][]Emit{{{Event: TextDelta{Text: "bad \xff byte"}}, {Event: Finish{Reason: FinishStop, Usage: billed}}}}}
	var answers int
	key := new(int)
	count := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			call.OnAnswer(key, func(context.Context, ModelResponse) { answers++ })
			return next(ctx, call)
		}
	}
	res, err := mustNew(m, memJournal(), WithMiddleware(count)).Run(ctx, "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	if answers != 1 || res.Spend != billed {
		t.Fatalf("answers = %d, want 1; Spend = %+v, want %+v", answers, res.Spend, billed)
	}
}

// Two drivers of one run answer its turn with records equal in every journaled field; one records
// it. The salt the journal holds tells them apart: the other's request is journaled as late spend,
// so both requests count, and only the recording driver runs its answer functions.
func TestAdv104c_IdenticalRecordsOfTwoDrivers(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	g := &twoGate{open: make(chan struct{})}
	var answers atomic.Int32
	key := new(int)
	count := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			call.OnAnswer(key, func(context.Context, ModelResponse) { answers.Add(1) })
			return next(ctx, call)
		}
	}
	var wg sync.WaitGroup
	for range 2 {
		j, err := NewJournal(procStore{mem})
		if err != nil {
			t.Fatal(err)
		}
		m := &gatedTurn{g: g, text: "same", u: billed}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := mustNew(m, j, WithMiddleware(count)).Run(ctx, "r", UserText("go")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	j, _ := NewJournal(procStore{mem})
	res, err := mustNew(&scriptModel{}, j).Run(ctx, "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Spend != twice(billed) || answers.Load() != 1 {
		t.Fatalf("Spend = %+v, want both requests' %+v; answers ran %d times, want 1", res.Spend, twice(billed), answers.Load())
	}
}

// A store that journals no salt (it breaks the contract) is compared by the journal encoding: a
// record read back in its canonical form is the one built, and a different one is not.
func TestAdv104c_OwnRecordWithoutSalt(t *testing.T) {
	msg := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "lookup", Args: json.RawMessage(`{"q": "x"}`)}, Text{Text: "bad \xff"}}}
	built := Record{Kind: StepModel, Message: &msg, Usage: &billed, ModelTurn: &ModelTurn{Finish: FinishToolUse}}
	if err := stampSalt(&built); err != nil {
		t.Fatal(err)
	}
	b, err := EncodeRecord(Record{Name: "@llm/0", Kind: StepModel, Message: &msg, Usage: &billed, ModelTurn: &ModelTurn{Finish: FinishToolUse}})
	if err != nil {
		t.Fatal(err)
	}
	held, err := DecodeRecord(b)
	if err != nil {
		t.Fatal(err)
	}
	if !ownRecord(held, built) {
		t.Fatal("the record read back in canonical form is not taken as the one built")
	}
	other := held
	other.ModelTurn = &ModelTurn{Finish: FinishStop}
	if ownRecord(other, built) {
		t.Fatal("a different record is taken as the one built")
	}
	salted := held
	salted.salt = make([]byte, SaltSize)
	if ownRecord(salted, built) {
		t.Fatal("a record with another salt is taken as the one built")
	}
}

// lateModel streams one delta once started (closing started), then waits for gate, ignoring its
// context, and finishes with usage u.
type lateModel struct {
	u       Usage
	started chan struct{}
	gate    chan struct{}
}

func (m *lateModel) Stream(context.Context, Request) (*Stream, error) {
	close(m.started)
	ch := make(chan Emit)
	go func() {
		defer close(ch)
		<-m.gate
		ch <- Emit{Event: TextDelta{Text: "late"}}
		ch <- Emit{Event: Finish{Reason: FinishStop, Usage: m.u}}
	}()
	return NewStream(ch), nil
}

// leaveAndFail is a middleware that starts a request it does not wait for, and fails the call once
// that request has started, releasing the request to finish.
func leaveAndFail(m *lateModel) Middleware {
	return func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			go func() { _, _ = next(context.WithoutCancel(ctx), call) }()
			<-m.started
			close(m.gate)
			return ModelResponse{}, errors.New("gave up")
		}
	}
}

// F3: a request still in flight when a turn fails for good is waited for, and its spend is in the
// failed call's @spend record (the one spend record), so the budget and a re-drive's Result.Spend
// count it.
func TestF3_InFlightRequestOfFailedTurnIsJournaled(t *testing.T) {
	m := &lateModel{u: billed, started: make(chan struct{}), gate: make(chan struct{})}
	store := memJournal()
	if _, err := mustNew(m, store, WithMiddleware(leaveAndFail(m))).Run(context.Background(), "r", UserText("go")); err == nil {
		t.Fatal("want the call's failure")
	}
	var journaled Usage
	var names []string
	for _, r := range modelRecordsAll(t, store, "r") {
		if r.DiscardedUsage != nil {
			addUsage(&journaled, *r.DiscardedUsage)
			names = append(names, r.Name)
		}
	}
	if journaled != billed || len(names) != 1 || !strings.HasPrefix(names[0], spendStepPrefix) {
		t.Fatalf("journaled spend %+v in %q, want %+v in one %s record", journaled, names, billed, spendStepPrefix)
	}
}

// F3: a request that ignores cancellation past lateRequestWait is not waited for beyond it: the
// run returns, and that request's spend is not in Result.Spend (the documented limit).
func TestF3_WaitIsBounded(t *testing.T) {
	defer func(d time.Duration) { lateRequestWait = d }(lateRequestWait)
	lateRequestWait = 20 * time.Millisecond
	gate := make(chan struct{})
	defer close(gate)
	m := &lateModel{u: billed, started: make(chan struct{}), gate: gate}
	stuck := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			go func() { _, _ = next(context.WithoutCancel(ctx), call) }()
			<-m.started
			return ModelResponse{Message: Message{Role: RoleAssistant, Parts: []Part{Text{Text: "own"}}}}, nil
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, err := mustNew(m, memJournal(), WithMiddleware(stuck)).Run(context.Background(), "r", UserText("go"))
		if err != nil || res.Spend != (Usage{}) {
			t.Errorf("res %+v, err %v: want the run to finish without the stuck request's spend", res, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run waited past its bound for a request that ignores cancellation")
	}
}

// modelRecordsAll returns every record of run runID.
func modelRecordsAll(t *testing.T, store *Journal, runID string) []Record {
	t.Helper()
	recs, err := store.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// F3: the spend of a request that ended after its turn, journaled at the run's end in a late
// spend record, counts in Result.Spend, on re-entry, and when the run is replayed.
func TestF3_LateSpendIsJournaledAndReplayed(t *testing.T) {
	m := &lateModel{u: billed, started: make(chan struct{}), gate: make(chan struct{})}
	answer := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			go func() { _, _ = next(context.WithoutCancel(ctx), call) }()
			<-m.started
			close(m.gate) // it ends while the run completes
			return ModelResponse{Message: Message{Role: RoleAssistant, Parts: []Part{Text{Text: "own"}}}}, nil
		}
	}
	store := memJournal()
	a := mustNew(m, store, WithMiddleware(answer))
	res, err := a.Run(context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := a.Run(context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Spend != billed || again.Spend != billed {
		t.Fatalf("Spend = %+v, re-entered %+v; want the late request's %+v", res.Spend, again.Spend, billed)
	}
	rm, err := Replay(context.Background(), store, "r")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := mustNew(rm, memJournal()).Run(context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Spend != billed {
		t.Fatalf("replayed Spend = %+v, want %+v", replayed.Spend, billed)
	}
}

// landsThenFails records the first Insert of the named step, then reports that it failed.
type landsThenFails struct {
	Store
	name   string
	failed bool
}

func (s *landsThenFails) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	e, ok, err := s.Store.Insert(ctx, runID, name, data)
	if name == s.name && !s.failed && err == nil {
		s.failed = true
		return Entry{}, false, errors.New("connection reset after commit")
	}
	return e, ok, err
}

// F4: when writing a model turn's record is reported failed but the record landed, its spend is
// not journaled a second time: the resumed run's Spend counts the request once.
func TestF4_LandedRecordIsNotCountedTwice(t *testing.T) {
	ctx := context.Background()
	st := &landsThenFails{Store: NewMemStore(), name: "@llm/0"}
	j, err := NewJournal(st)
	if err != nil {
		t.Fatal(err)
	}
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed), textTurnWithUsage("again", billed)}}
	a := mustNew(m, j)
	if _, err := a.Run(ctx, "r", UserText("go")); err == nil {
		t.Fatal("want the write failure")
	}
	res, err := a.Run(ctx, "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Spend != billed {
		t.Fatalf("Spend = %+v, want the one request's %+v", res.Spend, billed)
	}
}
