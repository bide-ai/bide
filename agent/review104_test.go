package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

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
	if _, err := mustNew(m, store, WithMiddleware(leaveAndFail(m))).RunResult(context.Background(), "r", "go"); err == nil {
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
		res, err := mustNew(m, memJournal(), WithMiddleware(stuck)).RunResult(context.Background(), "r", "go")
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
	res, err := a.RunResult(context.Background(), "r", "go")
	if err != nil {
		t.Fatal(err)
	}
	again, err := a.RunResult(context.Background(), "r", "go")
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
	replayed, err := mustNew(rm, memJournal()).RunResult(context.Background(), "r", "go")
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
	if _, err := a.RunResult(ctx, "r", "go"); err == nil {
		t.Fatal("want the write failure")
	}
	res, err := a.RunResult(ctx, "r", "go")
	if err != nil {
		t.Fatal(err)
	}
	if res.Spend != billed {
		t.Fatalf("Spend = %+v, want the one request's %+v", res.Spend, billed)
	}
}
