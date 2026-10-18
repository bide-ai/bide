package audit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/journaltest"
)

// After the timer's publish fails, the docs say Flush (or the next write) publishes the growth.
// Flush walks the pending map only, and the failed timer removed the run from it.
func TestReview2_FlushPublishesGrowthAfterAFailedTimerPublish(t *testing.T) {
	r := newIntervalRig(t, 10*time.Second)
	r.write(t, "a")
	r.clk.Advance(time.Second)
	r.write(t, "b") // held back, timer armed
	r.anchor.mu.Lock()
	r.anchor.fail = errors.New("anchor down")
	r.anchor.mu.Unlock()
	r.clk.Advance(10 * time.Second) // the timer fires and fails
	r.anchor.mu.Lock()
	r.anchor.fail = nil
	r.anchor.mu.Unlock()
	before := len(r.anchor.sizes())
	if err := r.store.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.anchor.sizes()) == before {
		t.Fatalf("Flush published nothing: the growth of 'b' stays unanchored (Pending = %v)", r.store.Pending())
	}
}

// run:cancelled ends a run like run:complete and run:aborted, but is not published at once.
func TestReview2_CancelEndIsPublishedAtOnce(t *testing.T) {
	r := newIntervalRig(t, time.Hour)
	r.write(t, "a")
	r.clk.Advance(time.Second)
	if _, err := journaltest.Do(context.Background(), r.j, "run-1", "run:cancelled", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: []byte(`{"reason":"x"}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(r.anchor.sizes()) < 2 {
		t.Fatalf("the cancellation of a run is held back for the interval: heads %v, pending %v", r.anchor.sizes(), r.store.Pending())
	}
}
