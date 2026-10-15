package agent

import (
	"context"
	"testing"
)

// Composition of the re-ask note with the notifier's catch-up scan. A re-ask event delivered to approvers must not be cleared
// by the catch-up scan while the gate is still waiting on the re-ask: eventFromKey rebuilds the
// event from its trail key without Reask, so current() asks the first-ask question (is there a
// tally?) and the weak tally the gate does not act on makes it "decided".
func TestReask_ScanDoesNotClearAStandingReask(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	err := weakThenStrong(t, store)
	clk := resolvedClock()
	sink, trail := &fakeSink{}, NewMemNotifyTrail()
	e := testEngine(t, store, sink, trail, clk, "p1")
	ev, ok := e.eventFromErr("r1", err)
	if !ok || ev.approval == nil || ev.approval.Reask == "" {
		t.Fatalf("no re-ask event from %v", err)
	}
	if res, rerr := e.round(ctx, ev); rerr != nil || !res.done {
		t.Fatal(res, rerr)
	}
	clk.advance(1)
	scanAndFlush(t, e, "r1")
	if got := resolvedCalls(sink); len(got) != 0 {
		t.Fatalf("an all-clear (%s) was sent for a re-ask the gate is still waiting on", got[0].Reason)
	}
	for _, r := range trailOf(t, trail, "r1", ev.key) {
		if r.Kind == NotifyCleared {
			t.Fatalf("the re-ask event %s was cleared (%q) while its gate still waits", ev.key, r.Reason)
		}
	}
}
