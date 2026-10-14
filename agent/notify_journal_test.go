package agent

import (
	"context"
	"testing"
	"time"
)

func journalEventByKey(evs []JournalNotifyEvent, key string) *JournalNotifyEvent {
	for i := range evs {
		if evs[i].Key == key {
			return &evs[i]
		}
	}
	return nil
}

// A gate the run paused at is an event whether or not it was decided since; Current says which.
func TestJournalNotifyEvents_GatesAndDecisions(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	at := time.Date(2026, 10, 13, 9, 0, 0, 123, time.UTC)
	putGate(t, j, "r", ApprovalWait{ToolUseID: "open", ToolName: "refund", Single: true, RecordedAt: at.Format(time.RFC3339Nano)})
	putGate(t, j, "r", ApprovalWait{ToolUseID: "done", ToolName: "refund", Single: true})
	if _, _, err := j.putEntry(ctx, "r", ToolResultStep("done"), Record{Kind: StepToolResult, ToolUseID: "done"}); err != nil {
		t.Fatal(err)
	}
	recs, err := j.History(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	evs := JournalNotifyEvents("r", recs)
	if len(evs) != 2 {
		t.Fatalf("%d events, want 2: %+v", len(evs), evs)
	}
	open, done := journalEventByKey(evs, ApprovalKey("r", "open", 0)), journalEventByKey(evs, ApprovalKey("r", "done", 0))
	if open == nil || !open.Current || open.Kind != NotificationApprovalPending || open.RunID != "r" || !open.At.Equal(at) {
		t.Fatalf("open gate = %+v", open)
	}
	if done == nil || done.Current {
		t.Fatalf("a decided gate must be returned, not current: %+v", done)
	}
}

// A live attempt marker with no result is a halt at its marker time; a result, a not-started record
// or an untimed marker is not one. A sub-run's event belongs to the root run's trail.
func TestJournalNotifyEvents_HaltsAndSubRuns(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	ms := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC).UnixMilli()
	putMarker(t, j, "r>sub", "c1", ms)
	putMarker(t, j, "done", "c1", ms)
	if _, _, err := j.putEntry(ctx, "done", ToolResultStep("c1"), Record{Kind: StepToolResult, ToolUseID: "c1"}); err != nil {
		t.Fatal(err)
	}
	putMarker(t, j, "untimed", "c1", 0)
	for run, want := range map[string]int{"r>sub": 1, "done": 0, "untimed": 0} {
		recs, _ := j.History(ctx, run)
		evs := JournalNotifyEvents(run, recs)
		if len(evs) != want {
			t.Fatalf("%s: %d events, want %d: %+v", run, len(evs), want, evs)
		}
		if want == 1 {
			e := evs[0]
			if e.Key != HaltedKey("r>sub", OpTool, "c1", ms) || e.Kind != NotificationOutcomeUnknown || e.RunID != "r" || !e.Current || e.At.UnixMilli() != ms {
				t.Fatalf("halt = %+v", e)
			}
		}
	}
}

// An aborted run is an event of its root run, with the abort time when the journal has one.
func TestJournalNotifyEvents_Abort(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	if _, _, err := j.putEntry(ctx, "r", runAbortedStep, Record{Kind: StepValue, Result: []byte(`{"aborted_at":"2026-10-13T09:00:00Z"}`)}); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.History(ctx, "r")
	evs := JournalNotifyEvents("r", recs)
	if len(evs) != 1 || evs[0].Key != AbortedKey("r") || evs[0].Kind != NotificationRunAborted || evs[0].At.IsZero() {
		t.Fatalf("events = %+v", evs)
	}
}

// The keys JournalNotifyEvents returns are the keys the notifier's sweep writes, for a gate, a halt
// and an abort, so an auditor comparing the two finds a missing record and nothing else.
func TestJournalNotifyEvents_KeysMatchTheSweep(t *testing.T) {
	ctx := context.Background()
	clk := sweepClock()
	j := memJournal()
	putGate(t, j, "g", ApprovalWait{ToolUseID: "c1", ToolName: "refund", Single: true})
	putMarker(t, j, "h", "c1", clk.now().Add(-time.Hour).UnixMilli())
	if _, _, err := j.putEntry(ctx, "a", runAbortedStep, Record{Kind: StepValue, Result: []byte(`{"aborted_at":"` + clk.now().Add(-time.Minute).Format(time.RFC3339Nano) + `"}`)}); err != nil {
		t.Fatal(err)
	}
	trail := NewMemNotifyTrail()
	if err := sweep(t, j, &fakeSink{}, trail, clk); err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{"g", "h", "a"} {
		recs, _ := j.History(ctx, run)
		evs := JournalNotifyEvents(run, recs)
		if len(evs) != 1 {
			t.Fatalf("%s: %+v", run, evs)
		}
		st := FoldNotify(mustLoad(t, trail, NotifyTrailName(evs[0].RunID)))
		if st.Events[evs[0].Key] == nil {
			t.Fatalf("%s: the sweep wrote no event under key %q (trail has %v)", run, evs[0].Key, st.Keys)
		}
	}
}

func mustLoad(t *testing.T, tr NotifyTrail, name string) []NotifyRecord {
	t.Helper()
	recs, err := tr.Load(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// A durable step's attempt marker with no result is a halt of kind step, keyed by the step's name.
func TestJournalNotifyEvents_StepHalt(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	ms := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC).UnixMilli()
	if _, err := j.do(ctx, "r", stepAttemptStep("pay"), func(context.Context) (Record, error) {
		return Record{Kind: StepAttempt, AttemptedAt: ms}, nil
	}); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.History(ctx, "r")
	evs := JournalNotifyEvents("r", recs)
	if len(evs) != 1 || evs[0].Key != HaltedKey("r", OpStep, "pay", ms) || evs[0].Kind != NotificationOutcomeUnknown {
		t.Fatalf("events = %+v, want the step halt %q", evs, HaltedKey("r", OpStep, "pay", ms))
	}
}

// A sub-run's gate is keyed by the sub-run, as the notifier keys it, and belongs to the root's trail;
// a sub-run's abort is not an event of its own (the root run reports its end).
func TestJournalNotifyEvents_SubRunGateAndAbort(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	sub := SubRunID("r", "c0")
	putGate(t, j, sub, ApprovalWait{ToolUseID: "c1", ToolName: "refund", Single: true})
	if _, _, err := j.putEntry(ctx, sub, runAbortedStep, Record{Kind: StepValue, Result: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.History(ctx, sub)
	evs := JournalNotifyEvents(sub, recs)
	if len(evs) != 1 {
		t.Fatalf("events = %+v, want only the gate", evs)
	}
	if e := evs[0]; e.Key != ApprovalKey(sub, "c1", 0) || e.RunID != "r" || e.Kind != NotificationApprovalPending {
		t.Fatalf("gate = %+v", e)
	}
}
