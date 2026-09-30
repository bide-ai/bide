package agent

// Adversarial re-review of the #104 review fixes (f2b31af). Copy into agent/ to run.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

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

func names(t *testing.T, d Durable, runID string) []string {
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
	if _, err := New(m, j).Use(answerAndLeave(bg)).RunResult(ctx, "r", "go"); err == nil {
		t.Fatal("want the write failure")
	}
	res, err := New(&scriptModel{}, j).RunResult(ctx, "r", "go")
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
	rres, rerr := New(rm, NewMemStore()).RunResult(ctx, "r", "go")
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
	if _, err := New(m, j).RunResult(ctx, "r", "go"); err == nil {
		t.Fatal("want the write failure")
	}
	st.mu.Lock()
	st.failGet = map[string]bool{}
	st.mu.Unlock()
	res, err := New(&scriptModel{}, j).RunResult(ctx, "r", "go")
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
		_, err := New(bg, NewMemStore()).Use(fail).RunResult(context.Background(), "r", "go")
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
	if _, err := New(m, j).Use(answerAndLeave(bg)).RunResult(ctx, "r", "go"); err == nil {
		t.Fatal("want the late spend write's error")
	}
	res, err := New(&scriptModel{}, j).RunResult(ctx, "r", "go")
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
	if _, err := New(m, j).Use(answerAndLeave(bg)).RunResult(ctx, "r", "go"); err == nil {
		t.Fatal("want the run:complete write's error")
	}
	res, err := New(&scriptModel{}, j).RunResult(ctx, "r", "go")
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
	charge := &countingTool{name: "charge", safety: Safety{RequiresApproval: true}, calls: &charged}
	m := &scriptModel{turns: [][]Emit{toolTurnWithUsage("c1", "charge", `{}`, billed)}}
	_, err := New(m, j, charge).Use(answerAndLeave(bg)).RunResult(ctx, "r", "go")
	var pend *PendingApproval
	if !errors.As(err, &pend) {
		t.Fatalf("err = %v, want a pause", err)
	}
	if err := Approve(ctx, j, "r", pend.ToolUseID, true); err != nil {
		t.Fatal(err)
	}
	res, err := New(&scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}, j, charge).RunResult(ctx, "r", "go")
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
	a := New(m, j).Use(count)
	if _, err := a.RunResult(ctx, "r", "go"); err == nil {
		t.Fatal("want the write failure")
	}
	if _, err := a.RunResult(ctx, "r", "go"); err != nil {
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
