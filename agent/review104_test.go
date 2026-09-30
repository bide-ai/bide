package agent

import (
	"context"
	"errors"
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
// failed call's @spend record, so the budget and a re-drive's Result.Spend count it.
func TestF3_InFlightRequestOfFailedTurnIsJournaled(t *testing.T) {
	m := &lateModel{u: billed, started: make(chan struct{}), gate: make(chan struct{})}
	store := NewMemStore()
	if _, err := New(m, store).Use(leaveAndFail(m)).RunResult(context.Background(), "r", "go"); err == nil {
		t.Fatal("want the call's failure")
	}
	var journaled Usage
	for _, r := range modelRecordsAll(t, store, "r") {
		if r.DiscardedUsage != nil {
			addUsage(&journaled, *r.DiscardedUsage)
		}
	}
	if journaled != billed {
		t.Fatalf("journaled spend %+v, want %+v", journaled, billed)
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
		res, err := New(m, NewMemStore()).Use(stuck).RunResult(context.Background(), "r", "go")
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
func modelRecordsAll(t *testing.T, store Durable, runID string) []Record {
	t.Helper()
	recs, err := store.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}
