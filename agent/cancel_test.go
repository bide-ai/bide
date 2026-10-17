package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/internal/journalhook"
)

// A cancelled tool call has an unknown outcome, not a failed one. Here a non-idempotent charge
// sends its request (the side effect happens), then the run is cancelled before the tool can
// report back, so it returns context.Canceled. If the loop journals that as the call's result, a
// resume sees a "failed" charge instead of an attempt with no result: the halt that protects
// at-most-once never fires, and a model that retries the failed charge charges twice.
func TestCancelledToolCall_IsNotRecordedAsItsOutcome(t *testing.T) {
	store := memJournal()
	var charged int
	fired := make(chan struct{})
	var fireOnce sync.Once
	charge := MustFunc("charge", "charge the card", func(ctx context.Context, _ struct{}) (string, error) {
		charged++ // the request reached the provider
		fireOnce.Do(func() { close(fired) })
		<-ctx.Done() // cancelled while waiting for the response
		return "", ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-fired; cancel() }()
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	_, err := mustNew(m, store, WithTools(charge)).Run(ctx, "r1", UserText("pay"))
	if rec, ok := hasStep(t, store, "r1", ToolResultStep("c1")); ok {
		t.Errorf("the cancelled call's outcome was journaled as %s (is_error=%v); want no result recorded, since the outcome is unknown", rec.Result, rec.IsError)
	}
	if complete, _ := IsComplete(context.Background(), store, "r1"); complete {
		t.Errorf("the cancelled run was marked complete")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run err = %v, want a cancellation", err)
	}
	if errors.Is(err, ErrTool) {
		t.Fatalf("run err = %v reports the cancellation as a tool fault", err)
	}

	// Resume: the charge was attempted with no recorded result, so the run halts for
	// confirmation instead of letting a retry charge again.
	m2 := &greedyModel{script: [][]Emit{toolTurn("c2", "charge", `{}`), textTurn("done")}}
	done := make(chan struct{})
	var resumeErr error
	go func() {
		defer close(done)
		_, resumeErr = mustNew(m2, store, WithTools(charge)).Run(context.Background(), "r1", UserText("pay"))
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("resume hung")
	}
	var halt *OutcomeUnknown
	if !errors.As(resumeErr, &halt) || halt.Op.ID != "c1" {
		t.Fatalf("resume err = %v, want OutcomeUnknown for c1 (charged %d times)", resumeErr, charged)
	}
	if charged != 1 {
		t.Fatalf("charged %d times, want 1", charged)
	}
}

// ctxModel wraps a model the way a real provider adapter behaves: a call on a cancelled context
// fails instead of answering.
type ctxModel struct{ inner Model }

func (m ctxModel) Stream(ctx context.Context, req Request) (*Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.inner.Stream(ctx, req)
}

// The double charge end to end, with a model that honors cancellation as real adapters do: the
// run is cancelled while the charge's request is in flight, so the first run stops; on resume, the
// model reads the journaled "failed" charge and retries it. The customer must still be charged
// once.
func TestCancelledToolCall_ResumeDoesNotChargeTwice(t *testing.T) {
	store := memJournal()
	var charged int
	fired := make(chan struct{}, 1)
	charge := MustFunc("charge", "charge the card", func(ctx context.Context, _ struct{}) (string, error) {
		charged++
		select {
		case fired <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return "", ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-fired; cancel() }()
	first := ctxModel{&greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}}
	_, _ = mustNew(first, store, WithTools(charge)).Run(ctx, "r1", UserText("pay"))

	// The retry would block in the charge until its context ends, so give the resume one.
	rctx, rcancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer rcancel()
	retry := ctxModel{&greedyModel{script: [][]Emit{toolTurn("c2", "charge", `{}`), textTurn("done")}}}
	_, err := mustNew(retry, store, WithTools(charge)).Run(rctx, "r1", UserText("pay"))
	if charged != 1 {
		t.Fatalf("charged %d times, want 1 (resume err: %v)", charged, err)
	}
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) || halt.Op.ID != "c1" {
		t.Fatalf("resume err = %v, want OutcomeUnknown for c1", err)
	}
}

// A run cancelled while a tool is running stops there, even when the tool finishes anyway and the
// model adapter ignores cancellation: the finished call's result is its known outcome and is
// journaled, but the run asks for no further turn and is not marked complete, so a resume
// continues from the journal instead of the cancelled run answering on its own.
func TestCancelledRun_StopsBeforeTheNextTurn(t *testing.T) {
	store := memJournal()
	ctx, cancel := context.WithCancel(context.Background())
	lookup := MustFunc("lookup", "look up the order", func(context.Context, struct{}) (string, error) {
		cancel() // the caller cancels while the call is in flight; the call completes regardless
		return "shipped", nil
	}, WithSafety(Safety{ReadOnly: true}))
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "lookup", `{}`), textTurn("done")}}
	_, err := mustNew(m, store, WithTools(lookup)).Run(ctx, "r1", UserText("status?"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("run err = %v, want a cancellation", err)
	}
	if m.calls != 1 {
		t.Errorf("the model was asked for %d turns after the run was cancelled, want 0 (calls = %d)", m.calls-1, m.calls)
	}
	if complete, _ := IsComplete(context.Background(), store, "r1"); complete {
		t.Errorf("the cancelled run was marked complete")
	}
	if rec, ok := hasStep(t, store, "r1", ToolResultStep("c1")); !ok || rec.IsError {
		t.Errorf("the completed call's result was not journaled (found=%v, rec=%+v)", ok, rec)
	}
}

// startedRun records a run:start for run r, a saga's or not, and returns the store.
func startedRun(t *testing.T, saga bool) *MemStore {
	t.Helper()
	s := NewMemStore()
	start := RunStart{Input: UserText("go"), Kind: RunKindAgent, Saga: saga}
	if _, _, _, err := journalhook.Begin(context.Background(), mustJournal(s), "r", start); err != nil {
		t.Fatal(err)
	}
	return s
}

// Cancel refuses a nil journal, an empty run ID, and a run that never started, and a store it cannot
// read or write is reported with the run still running (nothing recorded as cancelled).
func TestCancel_RefusalsAndFaults(t *testing.T) {
	ctx := context.Background()
	if err := Cancel(ctx, nil, "r", "x"); !errors.Is(err, ErrConfig) {
		t.Errorf("nil journal = %v; want ErrConfig", err)
	}
	if err := Cancel(ctx, memJournal(), "", "x"); !errors.Is(err, ErrConfig) {
		t.Errorf("empty run ID = %v; want ErrConfig", err)
	}
	if err := Cancel(ctx, memJournal(), "never", "x"); !errors.Is(err, ErrNotStarted) {
		t.Errorf("run never started = %v; want ErrNotStarted", err)
	}
	for name, c := range map[string]struct {
		wrap func(Store) Store
	}{
		"end marker unreadable": {func(s Store) Store { return covGetFail{s, runCompleteStep} }},
		"start unreadable":      {func(s Store) Store { return covGetFail{s, runStartStep} }},
		"cancel unwritable":     {func(s Store) Store { return covInsertFail{s, runCancelledStep} }},
	} {
		s := startedRun(t, false)
		j := mustJournal(c.wrap(s))
		if err := Cancel(ctx, j, "r", "stop"); err == nil || errors.Is(err, ErrRunEnded) {
			t.Errorf("%s: Cancel = %v; want a store error", name, err)
		}
		if st, err := Status(ctx, mustJournal(s), "r"); err != nil || st.State != RunStarted {
			t.Errorf("%s: Status after the failed Cancel = %+v, %v; want started", name, st, err)
		}
	}
	// A saga's cancellation is a request for its rollback, which also must not be lost silently.
	sj := mustJournal(covInsertFail{startedRun(t, true), runCancelRequestedStep})
	if err := Cancel(ctx, sj, "r", "stop"); err == nil {
		t.Error("Cancel of a saga whose request cannot be written succeeded")
	}
}

// A saga that is cancelled records a rollback request, not an end marker: it stays started until
// its rollback finishes, and the reason is kept.
func TestCancel_SagaRecordsARequest(t *testing.T) {
	ctx := context.Background()
	j := mustJournal(startedRun(t, true))
	if err := Cancel(ctx, j, "r", "customer asked"); err != nil {
		t.Fatal(err)
	}
	if st, err := Status(ctx, j, "r"); err != nil || st.State != RunStarted {
		t.Fatalf("Status = %+v, %v; want started", st, err)
	}
	if r, ok, err := j.Get(ctx, "r", runCancelRequestedStep); err != nil || !ok || endText(r) != "customer asked" {
		t.Fatalf("request = %+v, %v, %v", r, ok, err)
	}
	if _, ok, _ := j.Get(ctx, "r", runCancelledStep); ok {
		t.Fatal("a saga's Cancel wrote the end marker before the rollback")
	}
}

// Status reports a store it cannot read, and reads a reason from both the current and the older
// string form of a saga's abort.
func TestStatus_FaultsAndTerminalText(t *testing.T) {
	ctx := context.Background()
	if _, err := Status(ctx, nil, "r"); !errors.Is(err, ErrConfig) {
		t.Errorf("nil journal = %v; want ErrConfig", err)
	}
	if _, err := Status(ctx, mustJournal(loadFails{NewMemStore()}), "r"); !errors.Is(err, ErrStorage) {
		t.Errorf("unreadable store = %v; want ErrStorage", err)
	}
	for name, c := range map[string]struct {
		result string
		want   string
	}{
		"current form": {`{"reason":"no seats","aborted_at":"2026-10-13T00:00:00.000Z"}`, "no seats"},
		"older string": {`"no seats"`, "no seats"},
		"other value":  {`[1]`, ""},
	} {
		s := startedRun(t, true)
		j := mustJournal(s)
		writeRaw(t, j, "r", runAbortedStep, Record{Kind: StepValue, Result: []byte(c.result)})
		st, err := Status(ctx, j, "r")
		if err != nil || st.State != RunAborted || st.Terminal != c.want {
			t.Errorf("%s: Status = %+v, %v; want aborted with %q", name, st, err, c.want)
		}
	}
}

// pcHookStore wraps a Store: it counts Inserts, and calls onGet after each Get and onLoad after
// each Load's first yielded entry, so a test can land another writer's entry at an exact point of
// a reader's sequence of reads (no sleeps).
type pcHookStore struct {
	Store
	mu      sync.Mutex
	inserts []string
	onGet   func(runID, name string)
	onLoad  func(runID string)
}

func (s *pcHookStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	e, ins, err := s.Store.Insert(ctx, runID, name, data)
	if ins {
		s.mu.Lock()
		s.inserts = append(s.inserts, name)
		s.mu.Unlock()
	}
	return e, ins, err
}

func (s *pcHookStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	e, ok, err := s.Store.Get(ctx, runID, name)
	if f := s.onGet; f != nil {
		f(runID, name)
	}
	return e, ok, err
}

func (s *pcHookStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	return func(yield func(Entry, error) bool) {
		first := true
		for e, err := range s.Store.Load(ctx, runID, after) {
			if !yield(e, err) {
				return
			}
			if first && err == nil {
				first = false
				if f := s.onLoad; f != nil {
					f(runID)
				}
			}
		}
	}
}

func (s *pcHookStore) takeInserts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.inserts
	s.inserts = nil
	return out
}

// pcJournal returns a journal over a pcHookStore on a MemStore.
func pcJournal(t *testing.T) (*Journal, *pcHookStore) {
	t.Helper()
	hs := &pcHookStore{Store: NewMemStore()}
	j, err := NewJournal(hs)
	if err != nil {
		t.Fatal(err)
	}
	return j, hs
}

// pcStart journals runID's run:start.
func pcStart(t *testing.T, j *Journal, runID string, saga bool) {
	t.Helper()
	b, err := marshalJournal(RunStart{Input: UserText("go"), Saga: saga})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.put(context.Background(), runID, runStartStep, Record{Kind: StepValue, Result: b}); err != nil {
		t.Fatal(err)
	}
}

// pcMark journals the value step name (an end marker, a request) of runID with text.
func pcMark(t *testing.T, j *Journal, runID, name, text string) {
	t.Helper()
	if _, err := j.put(context.Background(), runID, name, Record{Kind: StepValue, Result: mustJSON(text)}); err != nil {
		t.Fatal(err)
	}
}

// Rule 1: Cancel reads the end markers and refuses a run that is over: a completed or aborted run
// is ErrRunEnded with nothing written, and a cancelled one is cancelled already (nil, nothing
// written).
func TestP14Rule01_CancelRefusesARunThatIsOver(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, marker string
		saga         bool
		want         error
	}{
		{"completed", runCompleteStep, false, ErrRunEnded},
		{"aborted saga", runAbortedStep, true, ErrRunEnded},
		{"completed saga", runCompleteStep, true, ErrRunEnded},
		{"cancelled", runCancelledStep, false, nil},
		{"cancelled saga", runCancelledStep, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j, hs := pcJournal(t)
			pcStart(t, j, "r", tc.saga)
			pcMark(t, j, "r", tc.marker, "x")
			hs.takeInserts()
			err := Cancel(ctx, j, "r", "stop")
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("Cancel = %v, want %v", err, tc.want)
			}
			if got := hs.takeInserts(); len(got) != 0 {
				t.Fatalf("Cancel of a run that is over wrote %v", got)
			}
		})
	}
	// The first end marker decides: a run completed and then cancelled (two keys can both land)
	// is over as completed.
	j, hs := pcJournal(t)
	pcStart(t, j, "r", false)
	pcMark(t, j, "r", runCompleteStep, "")
	pcMark(t, j, "r", runCancelledStep, "late")
	hs.takeInserts()
	if err := Cancel(ctx, j, "r", "stop"); !errors.Is(err, ErrRunEnded) {
		t.Fatalf("Cancel of a run completed first = %v, want ErrRunEnded", err)
	}
	// And one cancelled first, then completed, is cancelled.
	j, _ = pcJournal(t)
	pcStart(t, j, "r", false)
	pcMark(t, j, "r", runCancelledStep, "first")
	pcMark(t, j, "r", runCompleteStep, "")
	if err := Cancel(ctx, j, "r", "stop"); err != nil {
		t.Fatalf("Cancel of a run cancelled first = %v, want nil", err)
	}
	// A live run is cancelled: run:cancelled {reason} is written.
	j, hs = pcJournal(t)
	pcStart(t, j, "r", false)
	hs.takeInserts()
	if err := Cancel(ctx, j, "r", "stop"); err != nil {
		t.Fatalf("Cancel of a live run = %v", err)
	}
	if got := hs.takeInserts(); len(got) != 1 || got[0] != runCancelledStep {
		t.Fatalf("Cancel wrote %v, want [run:cancelled]", got)
	}
	rec, ok, err := j.Get(ctx, "r", runCancelledStep)
	if err != nil || !ok || string(rec.Result) != `{"reason":"stop"}` {
		t.Fatalf("run:cancelled = %s, %v, %v; want {\"reason\":\"stop\"}", rec.Result, ok, err)
	}
	if err := Cancel(ctx, nil, "r", "x"); !errors.Is(err, ErrConfig) {
		t.Fatalf("Cancel with a nil journal = %v, want ErrConfig", err)
	}
	if err := Cancel(ctx, j, "", "x"); !errors.Is(err, ErrConfig) {
		t.Fatalf("Cancel of an empty run ID = %v, want ErrConfig", err)
	}
	// A session turn's run (and a sub-run) is a run: its ID is accepted.
	turn := sessionTurnRunID("chat", 0)
	pcStart(t, j, turn, false)
	if err := Cancel(ctx, j, turn, "x"); err != nil {
		t.Fatalf("Cancel of a session turn's run = %v", err)
	}
}

// Rule 4 (L3), Cancel's side: every writer of an end marker reads the markers back and reports
// the first. run:complete lands after Cancel's checks and before its insert; Cancel's own
// run:cancelled lands second, so the run completed, and Cancel says so (model 10's
// regress/cancel-verdict is Cancel reporting cancelled here).
func TestP14Rule04_CancelReadsBackTheFirstEndMarker(t *testing.T) {
	ctx := context.Background()
	j, hs := pcJournal(t)
	pcStart(t, j, "r", false)
	fired := false
	hs.onGet = func(runID, name string) {
		if name == runStartStep && !fired { // Cancel's last read before its insert
			fired = true
			if _, err := j.put(ctx, runID, runCompleteStep, Record{Kind: StepValue}); err != nil {
				t.Error(err)
			}
		}
	}
	err := Cancel(ctx, j, "r", "stop")
	if !fired {
		t.Fatal("the hook never fired: Cancel did not read run:start")
	}
	if !errors.Is(err, ErrRunEnded) {
		t.Fatalf("Cancel = %v, want ErrRunEnded: run:complete is the first end marker", err)
	}
	st, serr := Status(ctx, j, "r")
	if serr != nil || st.State != RunCompleted {
		t.Fatalf("Status = %+v, %v; want completed", st, serr)
	}
}

// Rule 5 (L4), Cancel's side: Cancel reads run:start. A run with none is ErrNotStarted and nothing
// is written; on a saga Cancel writes the rollback request run:cancel-requested, not the end
// marker run:cancelled, so recovery still lists the run, and Status says Started.
func TestP14Rule05_CancelNotStartedAndSagaRequest(t *testing.T) {
	ctx := context.Background()
	j, hs := pcJournal(t)
	// A run that holds records but no run:start (a Signal sent to a mistyped ID).
	pcMark(t, j, "r", "signal:x", "hi")
	hs.takeInserts()
	err := Cancel(ctx, j, "r", "stop")
	if !errors.Is(err, ErrNotStarted) || errors.Is(err, ErrConfig) {
		t.Fatalf("Cancel of a run with no run:start = %v, want ErrNotStarted (no category)", err)
	}
	if got := hs.takeInserts(); len(got) != 0 {
		t.Fatalf("Cancel of a run with no run:start wrote %v", got)
	}
	// And one with no entries at all.
	if err := Cancel(ctx, j, "empty", "stop"); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Cancel of an empty run = %v, want ErrNotStarted", err)
	}
	if got := hs.takeInserts(); len(got) != 0 {
		t.Fatalf("Cancel of an empty run wrote %v", got)
	}

	pcStart(t, j, "saga", true)
	hs.takeInserts()
	if err := Cancel(ctx, j, "saga", "stop"); err != nil {
		t.Fatalf("Cancel of a saga = %v", err)
	}
	if got := hs.takeInserts(); len(got) != 1 || got[0] != runCancelRequestedStep {
		t.Fatalf("Cancel of a saga wrote %v, want [run:cancel-requested]", got)
	}
	rec, ok, err := j.Get(ctx, "saga", runCancelRequestedStep)
	if err != nil || !ok || string(rec.Result) != `{"reason":"stop"}` {
		t.Fatalf("run:cancel-requested = %s, %v, %v", rec.Result, ok, err)
	}
	if _, ok, _ := j.Get(ctx, "saga", runCancelledStep); ok {
		t.Fatal("Cancel of a saga wrote run:cancelled: recovery would never roll it back (L4)")
	}
	if over, err := runEnded(ctx, j, "saga"); err != nil || over {
		t.Fatalf("runEnded of a saga with a rollback request = %v, %v; want not over", over, err)
	}
	st, err := Status(ctx, j, "saga")
	if err != nil || st.State != RunStarted {
		t.Fatalf("Status of a saga with a rollback request = %+v, %v; want started", st, err)
	}
	// A second Cancel while the request is pending is a no-op success.
	if err := Cancel(ctx, j, "saga", "again"); err != nil {
		t.Fatalf("second Cancel of a saga = %v", err)
	}
	if got := hs.takeInserts(); len(got) != 0 {
		t.Fatalf("second Cancel of a saga wrote %v", got)
	}
}

// Parallel with a concurrency cap starts no task once its context is cancelled: the tasks that
// never started must not run, and each reports the cancellation.
func TestParallel_StartsNoTaskAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ran atomic.Int32
	tasks := make([]Task[int], 5)
	for i := range tasks {
		tasks[i] = Task[int]{Name: fmt.Sprintf("t%d", i), Safety: Safety{ReadOnly: true}, Fn: func(context.Context) (int, error) {
			if ran.Add(1) == 1 {
				cancel() // the caller gives up while the first task runs
			}
			return i, nil
		}}
	}
	_, err := memJournal().Parallel(ctx, "r1", tasks, WithMaxConcurrency(1))
	if n := ran.Load(); n != 1 {
		t.Fatalf("%d tasks ran; want 1 (none started after the cancellation)", n)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled for the tasks that never started", err)
	}
}

// A Step called with a cancelled context does not start: fn never runs, and nothing is journaled.
func TestStep_DoesNotStartWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := memJournal()
	var ran bool
	_, err := store.Step(ctx, "r1", "charge", func(context.Context) (int, error) { ran = true; return 1, nil })
	if ran || !errors.Is(err, context.Canceled) {
		t.Fatalf("ran = %v, err = %v; want not run and context.Canceled", ran, err)
	}
	if recs, _ := store.History(context.Background(), "r1"); len(recs) != 0 {
		t.Fatalf("a cancelled Step journaled %d records", len(recs))
	}
}

// A sub-agent is a tool, so a cancelled parent waits for it as it waits for any tool call:
// when Run returns, no work of the run is still in flight. Here the sub-agent's write does not
// stop for cancellation (it has already reached the provider) and finishes 100ms later. If Run
// returned first, a caller shutting down after Run (closing the store, exiting) could cut off
// the journaling of a write that happened.
func TestSubAgent_CancelledParentWaitsForTheChild(t *testing.T) {
	var finished atomic.Bool
	started := make(chan struct{})
	write := MustFunc("write", "write the record", func(context.Context, struct{}) (string, error) {
		close(started)
		time.Sleep(100 * time.Millisecond) // in flight; does not observe cancellation
		finished.Store(true)
		return "written", nil
	}, WithSafety(Safety{Idempotent: true}))
	store := memJournal()
	child := mustNew(NewScriptedModel(ToolTurn("w1", "write", `{}`), TextTurn("done")), store, WithTools(write))
	parent := mustNew(
		NewScriptedModel(ToolTurn("s1", "clerk", `{"task":"file it"}`), TextTurn("done")),
		store,
		WithTools(MustSubAgent("clerk", "files records", child)),
	)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	_, _ = parent.Run(ctx, "r1", UserText("file the record"))
	if !finished.Load() {
		t.Fatal("Run returned while the sub-agent's write was still in flight")
	}
	if _, ok := hasStep(t, store, "r1>s1", ToolResultStep("w1")); !ok {
		t.Fatal("the sub-agent's completed write was not journaled by the time Run returned")
	}
}

// The reports a recovery pass makes once per process (ErrNotStarted, ErrNotResumable) are held in
// a bounded set: past its bound, the least recently reported run is forgotten (and is reported
// again if a later pass finds it so), so a process whose passes meet many unstarted runs does not
// grow without bound.
func TestP14_RecoverReportsAreBounded(t *testing.T) {
	old := maxRecoverReports
	maxRecoverReports = 2
	t.Cleanup(func() { maxRecoverReports = old })
	store := memJournal()
	for _, id := range []string{"bound-a", "bound-b", "bound-c"} {
		if !reportOnce(store, id, ErrNotStarted) {
			t.Fatalf("the first report of %s was suppressed", id)
		}
	}
	if n := recoverReportCount(); n > 2 {
		t.Fatalf("the report set holds %d entries, bound 2", n)
	}
	if reportOnce(store, "bound-c", ErrNotStarted) {
		t.Fatal("a run reported most recently was reported again")
	}
	if !reportOnce(store, "bound-a", ErrNotStarted) {
		t.Fatal("the least recently reported run, past the bound, was still remembered")
	}
}

// Rule 7 (L6): Status reads a prefix of the journal (one Load), so it never reports a completed run
// cancelled. Model 10's finding status-gets: a Status that reads one Get per end marker reads
// run:complete (absent), the run completes, Cancel writes run:cancelled second, and the Status
// reads run:cancelled and reports a completed run cancelled. The hooks land those two writes just
// after Status's first read, whichever read that is.
func TestP14Rule07_StatusReadsAPrefixNotOneRoundOfGets(t *testing.T) {
	ctx := context.Background()
	j, hs := pcJournal(t)
	pcStart(t, j, "r", false)
	fired := false
	land := func(runID string) {
		if fired {
			return
		}
		fired = true
		pcMark(t, j, runID, runCompleteStep, "")
		pcMark(t, j, runID, runCancelledStep, "late")
	}
	hs.onGet = func(runID, name string) {
		if name == runCompleteStep {
			land(runID)
		}
	}
	hs.onLoad = land
	st, err := Status(ctx, j, "r")
	if err != nil {
		t.Fatal(err)
	}
	if !fired {
		t.Fatal("the hooks never fired: Status read no end marker and no Load")
	}
	if st.State != RunStarted && st.State != RunCompleted {
		t.Fatalf("Status = %+v during the race; want started or completed (the run completed first), never cancelled", st)
	}
	hs.onGet, hs.onLoad = nil, nil
	st, err = Status(ctx, j, "r")
	if err != nil || st.State != RunCompleted || st.Terminal != "" {
		t.Fatalf("Status after the race = %+v, %v; want completed", st, err)
	}
}

// Rule 7: no run:start is NotStarted; no end marker is Started; the first end marker in journal
// order is the state, with its text as Terminal; Records counts the journal, header included.
func TestP14Rule07_StatusStates(t *testing.T) {
	ctx := context.Background()
	j, _ := pcJournal(t)
	if st, err := Status(ctx, j, "none"); err != nil || st != (RunStatus{State: RunNotStarted}) {
		t.Fatalf("Status of an empty run = %+v, %v; want not started, 0 records", st, err)
	}
	pcMark(t, j, "nostart", "signal:x", "hi")
	if st, err := Status(ctx, j, "nostart"); err != nil || st.State != RunNotStarted || st.Records != 2 {
		t.Fatalf("Status of a run with no run:start = %+v, %v; want not started, 2 records", st, err)
	}
	pcStart(t, j, "live", false)
	if st, err := Status(ctx, j, "live"); err != nil || st != (RunStatus{State: RunStarted, Records: 2}) {
		t.Fatalf("Status of a live run = %+v, %v; want started, 2 records", st, err)
	}
	pcStart(t, j, "done", false)
	pcMark(t, j, "done", runCompleteStep, "")
	pcMark(t, j, "done", runCancelledStep, "late")
	if st, err := Status(ctx, j, "done"); err != nil || st != (RunStatus{State: RunCompleted, Records: 4}) {
		t.Fatalf("Status of a run completed first = %+v, %v", st, err)
	}
	pcStart(t, j, "c", false)
	cancelledAt := time.Date(2026, 10, 14, 1, 2, 3, 456_000_000, time.UTC)
	if err := Cancel(ctx, j, "c", "operator said so", WithClock(func() time.Time { return cancelledAt })); err != nil {
		t.Fatal(err)
	}
	pcMark(t, j, "c", runCompleteStep, "")
	if st, err := Status(ctx, j, "c"); err != nil || st != (RunStatus{State: RunCancelled, Terminal: "operator said so", Records: 4, EndedAt: "2026-10-14T01:02:03.456Z"}) {
		t.Fatalf("Status of a run cancelled first = %+v, %v", st, err)
	}
	pcStart(t, j, "a", true)
	pcMark(t, j, "a", runAbortedStep, "step failed")
	if st, err := Status(ctx, j, "a"); err != nil || st != (RunStatus{State: RunAborted, Terminal: "step failed", Records: 3}) {
		t.Fatalf("Status of an aborted saga = %+v, %v", st, err)
	}
	// A saga whose cancellation's rollback finished: run:cancelled after the request.
	pcStart(t, j, "s", true)
	if err := Cancel(ctx, j, "s", "stop"); err != nil {
		t.Fatal(err)
	}
	if st, err := Status(ctx, j, "s"); err != nil || st.State != RunStarted {
		t.Fatalf("Status of a saga with a pending rollback request = %+v, %v; want started", st, err)
	}
	if _, err := j.put(ctx, "s", runCancelledStep, Record{Kind: StepValue, Result: []byte(`{"reason":"stop"}`)}); err != nil {
		t.Fatal(err)
	}
	if st, err := Status(ctx, j, "s"); err != nil || st != (RunStatus{State: RunCancelled, Terminal: "stop", Records: 4}) {
		t.Fatalf("Status of a rolled-back cancelled saga = %+v, %v", st, err)
	}
	if _, err := Status(ctx, nil, "x"); err == nil {
		t.Fatal("Status with a nil journal: want ErrConfig")
	}
}
