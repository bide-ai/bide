package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
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
	charge := Func("charge", "charge the card", Safety{}, func(ctx context.Context, _ struct{}) (string, error) {
		charged++ // the request reached the provider
		fireOnce.Do(func() { close(fired) })
		<-ctx.Done() // cancelled while waiting for the response
		return "", ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-fired; cancel() }()
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	_, err := mustNew(m, store, WithTools(charge)).Run(ctx, "r1", "pay")
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
		_, resumeErr = mustNew(m2, store, WithTools(charge)).Run(context.Background(), "r1", "pay")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("resume hung")
	}
	var halt *ResumeHalt
	if !errors.As(resumeErr, &halt) || halt.Op.ID != "c1" {
		t.Fatalf("resume err = %v, want ResumeHalt for c1 (charged %d times)", resumeErr, charged)
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
	charge := Func("charge", "charge the card", Safety{}, func(ctx context.Context, _ struct{}) (string, error) {
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
	_, _ = mustNew(first, store, WithTools(charge)).Run(ctx, "r1", "pay")

	// The retry would block in the charge until its context ends, so give the resume one.
	rctx, rcancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer rcancel()
	retry := ctxModel{&greedyModel{script: [][]Emit{toolTurn("c2", "charge", `{}`), textTurn("done")}}}
	_, err := mustNew(retry, store, WithTools(charge)).Run(rctx, "r1", "pay")
	if charged != 1 {
		t.Fatalf("charged %d times, want 1 (resume err: %v)", charged, err)
	}
	var halt *ResumeHalt
	if !errors.As(err, &halt) || halt.Op.ID != "c1" {
		t.Fatalf("resume err = %v, want ResumeHalt for c1", err)
	}
}

// A run cancelled while a tool is running stops there, even when the tool finishes anyway and the
// model adapter ignores cancellation: the finished call's result is its known outcome and is
// journaled, but the run asks for no further turn and is not marked complete, so a resume
// continues from the journal instead of the cancelled run answering on its own.
func TestCancelledRun_StopsBeforeTheNextTurn(t *testing.T) {
	store := memJournal()
	ctx, cancel := context.WithCancel(context.Background())
	lookup := Func("lookup", "look up the order", Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) {
		cancel() // the caller cancels while the call is in flight; the call completes regardless
		return "shipped", nil
	})
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "lookup", `{}`), textTurn("done")}}
	_, err := mustNew(m, store, WithTools(lookup)).Run(ctx, "r1", "status?")
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
