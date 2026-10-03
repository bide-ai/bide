package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
)

// A call the base handler REFUSED is not terminal once the chain returns: the loop only moves an
// OPEN call to closed, and enterTool moves refused to reached. So a middleware that got a refusal,
// left next running, and returned, reaches the tool after the loop has recorded the call as a
// known failure (called=false). The documented contract says the agent refuses any invocation of
// next that comes after the chain returned.
func TestAdv117c_RefusedThenLeakedNextReachesTool(t *testing.T) {
	var calls atomic.Int32
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) { calls.Add(1); return "ok", nil })
	release := make(chan struct{})
	leaked := make(chan error, 1)
	mw := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			bad := call
			bad.Use.ID = "other"
			_, err := next(ctx, bad) // refused before the ran map: state open -> refused
			go func() {
				<-release
				_, e := next(context.WithoutCancel(ctx), call) // after the chain returned
				leaked <- e
			}()
			return nil, err
		}
	}
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	if _, err := mustNew(m, store, WithTools(charge), WithToolMiddleware(mw)).Run(context.Background(), "r1", UserText("go")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rec, ok := hasStep(t, store, "r1", ToolResultStep("c1"))
	if !ok || !rec.IsError {
		t.Fatalf("setup: want the call recorded as a known failure, got %s (recorded %v)", rec.Result, ok)
	}
	close(release)
	e := <-leaked
	if n := calls.Load(); n != 0 {
		t.Fatalf("the journal records call c1 as a known failure (%s), yet a next invoked after the chain returned reached the tool %d time(s) (leaked next err: %v)", rec.Result, n, e)
	}
	if !errors.Is(e, ErrToolNotCalled) {
		t.Fatalf("leaked next = %v, want a refusal wrapping ErrToolNotCalled", e)
	}
}

// The same hole with a run cancellation: the refused call is recorded as NEVER STARTED, the leaked
// next fires the side effect, and a resume fires it again.
func TestAdv117c_RefusedThenLeakedNextDoubleFiresAcrossResume(t *testing.T) {
	var calls atomic.Int32
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) { calls.Add(1); return "ok", nil })
	release := make(chan struct{})
	leaked := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	mw := func(next ToolHandler) ToolHandler {
		return func(c context.Context, call ToolCall) (json.RawMessage, error) {
			bad := call
			bad.Use.ID = "other"
			_, err := next(c, bad) // refused: state refused
			go func() {
				<-release
				_, e := next(context.WithoutCancel(c), call)
				leaked <- e
			}()
			cancel()
			<-c.Done()
			return nil, err
		}
	}
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	_, err1 := mustNew(m, store, WithTools(charge), WithToolMiddleware(mw)).Run(ctx, "r1", UserText("go"))
	if _, recorded := hasStep(t, store, "r1", ToolResultStep("c1")); recorded || calls.Load() != 0 {
		t.Fatalf("setup: first drive %v; want no result recorded and no call yet", err1)
	}
	close(release)
	e := <-leaked
	if _, err := mustNew(m, store, WithTools(charge)).Run(context.Background(), "r1", UserText("go")); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n := calls.Load(); n > 1 {
		t.Fatalf("side-effect tool charge fired %d times for one call (first drive: %v; the leaked next after the chain returned: err %v; then the resume, since the claim was recorded never started)", n, err1, e)
	}
}
