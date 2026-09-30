package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// A sub-agent is a tool, so a cancelled parent waits for it as it waits for any tool call:
// when Run returns, no work of the run is still in flight. Here the sub-agent's write does not
// stop for cancellation (it has already reached the provider) and finishes 100ms later. If Run
// returned first, a caller shutting down after Run (closing the store, exiting) could cut off
// the journaling of a write that happened.
func TestSubAgent_CancelledParentWaitsForTheChild(t *testing.T) {
	var finished atomic.Bool
	started := make(chan struct{})
	write := Func("write", "write the record", Safety{Idempotent: true}, func(context.Context, struct{}) (string, error) {
		close(started)
		time.Sleep(100 * time.Millisecond) // in flight; does not observe cancellation
		finished.Store(true)
		return "written", nil
	})
	store := NewMemStore()
	child := New(NewScriptedModel(ToolTurn("w1", "write", `{}`), TextTurn("done")), store, write)
	parent := New(NewScriptedModel(ToolTurn("s1", "clerk", `{"task":"file it"}`), TextTurn("done")), store,
		SubAgent("clerk", "files records", child))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	_, _ = parent.Run(ctx, "r1", "file the record")
	if !finished.Load() {
		t.Fatal("Run returned while the sub-agent's write was still in flight")
	}
	if _, ok := hasStep(t, store, "r1>s1", ToolResultStep("w1")); !ok {
		t.Fatal("the sub-agent's completed write was not journaled by the time Run returned")
	}
}
