package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// An attempt marker carries two times for its claim. AttemptedAt is the wall clock, always: the grace
// period of ResolveHalt and the notifier's halt age are measured on it, and a run clock that lags or leads
// must not shorten them. RecordedAt is the same instant on the clock the run was driven with (WithClock),
// the clock the run's other records and its cap windows are dated on, for bide audit caps.

var markerClockT = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC) // a run clock far behind the wall clock

func markerClockOpt() Option { return WithClock(func() time.Time { return markerClockT }) }

// wantMarkerTimes checks a marker: AttemptedAt within [before, after] on the wall clock, RecordedAt the run clock.
func wantMarkerTimes(t *testing.T, rec Record, before, after time.Time) {
	t.Helper()
	if at := markerTime(rec.AttemptedAt); at.Before(before.Truncate(time.Millisecond)) || at.After(after) {
		t.Fatalf("AttemptedAt = %v, want the wall clock between %v and %v", at, before, after)
	}
	if got, want := rec.RecordedAt(), noteStamp(func() time.Time { return markerClockT }); got != want || got == "" {
		t.Fatalf("RecordedAt = %q, want the run's clock %q", got, want)
	}
}

func TestAttemptMarker_AToolCallCarriesBothClocks(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	var calls int
	charge := &countingTool{name: "charge", safety: Safety{}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	before := time.Now()
	if _, err := mustNew(m, j, WithTools(charge), markerClockOpt()).Run(ctx, "r", UserText("go")); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := j.Get(ctx, "r", toolAttemptStep("c1"))
	if err != nil || !ok {
		t.Fatalf("marker: %v, %v", ok, err)
	}
	wantMarkerTimes(t, rec, before, time.Now())
}

func TestAttemptMarker_AStepCarriesBothClocks(t *testing.T) {
	ctx := contextWithClock(context.Background(), func() time.Time { return markerClockT })
	j := memJournal()
	before := time.Now()
	if _, err := j.Step[string](ctx, "r", "post", func(context.Context) (string, error) { return "ok", nil }); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := j.Get(ctx, "r", stepAttemptStep("post"))
	if err != nil || !ok {
		t.Fatalf("marker: %v, %v", ok, err)
	}
	wantMarkerTimes(t, rec, before, time.Now())
}

func TestAttemptMarker_ClaimAttemptCarriesBothClocksAndRefusesACallersTimes(t *testing.T) {
	ctx := contextWithClock(context.Background(), func() time.Time { return markerClockT })
	j := memJournal()
	before := time.Now()
	won, got, err := ClaimAttempt(ctx, j, "r", "attempt:step:x", Record{Kind: StepAttempt, ToolUseID: "x"})
	if err != nil || !won {
		t.Fatal(won, err)
	}
	wantMarkerTimes(t, got, before, time.Now())
	for name, rec := range map[string]Record{
		"AttemptedAt": {Kind: StepAttempt, ToolUseID: "y", AttemptedAt: 1},
		"RecordedAt":  {Kind: StepAttempt, ToolUseID: "y", ApproverSignature: &ApproverSignature{RecordedAt: "2026-01-01T00:00:00.000Z"}},
	} {
		if _, _, err := ClaimAttempt(ctx, j, "r", "attempt:step:y", rec); !errors.Is(err, ErrConfig) {
			t.Fatalf("a caller-set %s: %v, want ErrConfig", name, err)
		}
	}
}

// A run clock behind the wall clock must not shorten the grace period of a halt: every reader of the
// marker's age still measures it on the wall clock.
func TestAttemptMarker_ALaggingRunClockDoesNotShortenAHaltsGrace(t *testing.T) {
	ctx := context.Background()
	st := newFaultStore()
	j := mustJournal(unwrapFault{st}) // Unwrap lets the sweep find the MemStore's Lister
	var calls int
	charge := &countingTool{name: "charge", safety: Safety{}, calls: &calls}
	st.failNoCommitPrefix = "tool:" // the result is lost: the marker stands
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`)}}
	if _, err := mustNew(m, j, WithTools(charge), markerClockOpt()).Run(ctx, "r", UserText("go")); err == nil {
		t.Fatal("want the lost result")
	}
	_, err := mustNew(&scriptModel{}, j, WithTools(charge), markerClockOpt()).Run(ctx, "r", UserText("go"))
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) || time.Since(halt.AttemptedAt) > time.Minute || halt.AttemptedAt.Before(markerClockT.AddDate(0, 6, 0)) {
		t.Fatalf("resume = %v (halt %+v), want a halt attempted just now on the wall clock", err, halt)
	}

	// The sweep, on a clock that is the wall clock plus a minute and then ten.
	clk := &notifyClock{t: time.Now().Add(time.Minute)}
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	if err := sweep(t, j, sink, trail, clk); err != nil || sink.count() != 0 {
		t.Fatalf("a halt one minute old was reported: %v, %d", err, sink.count())
	}
	clk.t = time.Now().Add(10 * time.Minute)
	if err := sweep(t, j, sink, trail, clk); err != nil || sink.count() != 1 {
		t.Fatalf("the halt ten minutes old: %v, %d", err, sink.count())
	}

	// The resolver's minimum age: right away it is too young, though the run clock says months ago.
	ref := HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "c1", ToolName: "charge"}, Cause: HaltCrashed}
	if err := ResolveHalt(ctx, j, ref, Outcome{Result: "x"}, WithMinHaltAge(time.Hour)); !errors.As(err, new(*HaltTooYoung)) {
		t.Fatalf("a halt made just now with a minimum of 1h: %v, want *HaltTooYoung", err)
	}
	later := func() time.Time { return time.Now().Add(2 * time.Hour) }
	if err := ResolveHalt(ctx, j, ref, Outcome{Result: "x"}, WithMinHaltAge(time.Hour), WithClock(later)); err != nil {
		t.Fatalf("a halt 2h old with a minimum of 1h: %v", err)
	}
}

// The marker ResolveHalt claims for the next attempt is stamped as every marker is: AttemptedAt from the wall
// clock, whatever WithClock the resolver was given, and that clock in RecordedAt.
func TestAttemptMarker_TheResolversHeldMarkerCarriesBothClocks(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	if won, _, err := ClaimAttempt(ctx, j, "r", "attempt:step:x", Record{Kind: StepAttempt, ToolUseID: "x"}); err != nil || !won {
		t.Fatal(won, err)
	}
	ref := HaltRef{RunID: "r", Op: OpRef{Kind: OpStep, ID: "x"}, Cause: HaltCrashed}
	before := time.Now()
	if err := ResolveHalt(ctx, j, ref, Outcome{Result: "x"}, WithClock(func() time.Time { return markerClockT })); err != nil {
		t.Fatal(err)
	}
	held, ok, err := j.Get(ctx, "r", nextAttemptStep("attempt:step:x"))
	if err != nil || !ok {
		t.Fatalf("the resolver's held marker: %v, %v", ok, err)
	}
	wantMarkerTimes(t, held, before, time.Now())
}
