package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// A saga's finished rollback records when the run aborted, by the drive's clock, beside why.
func TestAbort_RecordsWhenTheRunAborted(t *testing.T) {
	ctx := context.Background()
	l := newLedger()
	store := memJournal()
	a := mustNew(&scriptModel{turns: [][]Emit{toolTurn("a1", "A", `{}`), toolTurn("a2", "boom", `{}`)}}, store, WithTools(l.write("A"), failTool("boom")))
	at := time.Date(2026, 10, 13, 15, 4, 5, 678_000_000, time.UTC)
	if _, err := a.Run(ctx, "root", UserText("go"), WithSaga(), WithClock(func() time.Time { return at })); err == nil {
		t.Fatal("setup: the saga did not abort")
	}
	e, ok, err := store.getEntry(ctx, "root", runAbortedStep)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	rec, err := decodeStored("root", runAbortedStep, e.Data)
	if err != nil {
		t.Fatal(err)
	}
	got, has := abortedAt(rec)
	if !has || !got.Equal(at) {
		t.Fatalf("aborted_at = %v, %v; want %v", got, has, at)
	}
	st, err := Status(ctx, store, "root")
	if err != nil || st.State != RunAborted || st.Terminal == "" {
		t.Fatalf("Status = %+v, %v: the reason must still read as the terminal text", st, err)
	}
}

// aborted_at is informational: a run:aborted with the time, without it (an earlier version's, the
// reason as a string), or with another time behaves the same for Status, a drive and a recovery pass.
func TestAbort_TheTimeIsReadByNoDrive(t *testing.T) {
	ctx := context.Background()
	variants := map[string]json.RawMessage{
		"with a time":            mustJSONValue(abortValue{Reason: "refund failed", AbortedAt: "2026-10-13T09:00:00.000Z"}),
		"another time":           mustJSONValue(abortValue{Reason: "refund failed", AbortedAt: "1999-01-01T00:00:00.000Z"}),
		"a time that is no time": mustJSONValue(abortValue{Reason: "refund failed", AbortedAt: "yesterday"}),
		"no time":                mustJSONValue(abortValue{Reason: "refund failed"}),
		"an earlier version's":   mustJSON("refund failed"),
	}
	type outcome struct {
		state    RunState
		terminal string
		runErr   string
		recover  int
	}
	var first *outcome
	var firstName string
	for name, value := range variants {
		j := memJournal()
		start, _ := json.Marshal(RunStart{Input: UserText("go"), Saga: true})
		if _, _, err := j.putEntry(ctx, "r", runStartStep, Record{Kind: StepValue, Result: start}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := j.putEntry(ctx, "r", runAbortedStep, Record{Kind: StepValue, Result: value}); err != nil {
			t.Fatal(err)
		}
		st, err := Status(ctx, j, "r")
		if err != nil {
			t.Fatal(name, err)
		}
		a := mustNew(NewScriptedModel(), j)
		_, runErr := a.Resume(ctx, "r")
		n, rerr := Recover(ctx, j, func(context.Context, string, RunStart) error { return nil })
		if rerr != nil {
			t.Fatal(name, rerr)
		}
		o := &outcome{st.State, st.Terminal, fmtErr(runErr), n}
		if first == nil {
			first, firstName = o, name
		} else if *o != *first {
			t.Fatalf("%q behaves as %+v but %q as %+v: the time was read", name, *o, firstName, *first)
		}
	}
	if first.state != RunAborted || first.terminal != "refund failed" || first.recover != 0 {
		t.Fatalf("outcome = %+v", *first)
	}
}

func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// The sweep dates an abort by its aborted_at, whatever the run's start says; for a run an earlier
// version aborted, by the newest time the journal records (an attempt marker, else the start).
func TestNotifyPending_AbortedRunsAreDatedByTheAbortThenByTheNewestRecord(t *testing.T) {
	ctx := context.Background()
	clk := sweepClock()
	now := clk.now()
	j := memJournal()
	abort := func(run string, v json.RawMessage) {
		t.Helper()
		if _, _, err := j.putEntry(ctx, run, runAbortedStep, Record{Kind: StepValue, Result: v}); err != nil {
			t.Fatal(err)
		}
	}
	stamp := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339Nano) }
	// aborted_at says recent although the run started three days ago: caught up.
	putStart(t, j, "long-recent", now.Add(-72*time.Hour))
	abort("long-recent", mustJSONValue(abortValue{Reason: "x", AbortedAt: stamp(time.Minute)}))
	// aborted_at says old although the run started a minute ago (clocks differ): not caught up.
	putStart(t, j, "start-recent-abort-old", now.Add(-time.Minute))
	abort("start-recent-abort-old", mustJSONValue(abortValue{Reason: "x", AbortedAt: stamp(48 * time.Hour)}))
	// An earlier version's abort, busy until two minutes ago: caught up by its newest attempt marker.
	putStart(t, j, "legacy-busy", now.Add(-72*time.Hour))
	putMarker(t, j, "legacy-busy", "c1", now.Add(-2*time.Minute).UnixMilli())
	abort("legacy-busy", mustJSON("x"))
	// An earlier version's abort whose newest time is old: not caught up.
	putStart(t, j, "legacy-old", now.Add(-72*time.Hour))
	putMarker(t, j, "legacy-old", "c1", now.Add(-48*time.Hour).UnixMilli())
	abort("legacy-old", mustJSON("x"))
	// An earlier version's abort of a run that records no time at all: unknown, not caught up.
	abort("legacy-untimed", mustJSON("x"))

	sink := &fakeSink{}
	if err := sweep(t, j, sink, NewMemNotifyTrail(), clk); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range sink.calls {
		got[c.RunID] = true
	}
	if len(got) != 2 || !got["long-recent"] || !got["legacy-busy"] {
		t.Fatalf("caught up %v; want long-recent and legacy-busy only", got)
	}
}

func TestNotifyClock_DatesTheSweepAndRefusesNil(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	// The run aborted at 2020-01-01 by the drive's clock.
	if _, _, err := j.putEntry(ctx, "r", runAbortedStep, Record{Kind: StepValue, Result: mustJSONValue(abortValue{Reason: "x", AbortedAt: "2020-01-01T00:00:00.000Z"})}); err != nil {
		t.Fatal(err)
	}
	wall := &fakeSink{}
	if err := NotifyPending(ctx, j, wall, NewMemNotifyTrail()); err != nil || wall.count() != 0 {
		t.Fatalf("wall time: %v, %d notifications; a 2020 abort is long past", err, wall.count())
	}
	same := &fakeSink{}
	clk := func() time.Time { return time.Date(2020, 1, 1, 0, 5, 0, 0, time.UTC) }
	if err := NotifyPending(ctx, j, same, NewMemNotifyTrail(), WithNotifyClock(clk)); err != nil || same.count() != 1 {
		t.Fatalf("the drive's clock: %v, %d notifications; want the abort caught up", err, same.count())
	}
	if err := NotifyPending(ctx, j, same, NewMemNotifyTrail(), WithNotifyClock(nil)); !errors.Is(err, ErrConfig) {
		t.Fatalf("nil clock: %v", err)
	}
}
