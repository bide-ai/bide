package agent

import (
	"context"
	"testing"
	"time"
)

// REVIEW: the resolution claims its own attempt (AttemptedAt: cfg.now, the wall clock without
// WithClock) and stamps the result with the context's clock: one call, two clocks.
func TestReview_ResolveHaltOneCallOneClock(t *testing.T) {
	ctx := contextWithClock(context.Background(), fixedClock(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)))
	j := mustJournal(markerAt(t, time.Now().Add(-2*time.Hour).UnixMilli()))
	if err := ResolveHalt(ctx, j, covHalt, Outcome{Result: "charged"}, WithMinHaltAge(time.Hour)); err != nil {
		t.Fatal(err)
	}
	res, _ := hasStep(t, j, "r", ToolResultStep("c1"))
	recs, _ := j.History(context.Background(), "r")
	var claimed time.Time
	for _, r := range recs {
		if r.Kind == StepAttempt && r.AttemptedAt > 0 && r.Name != toolAttemptStep("c1") {
			claimed = time.UnixMilli(r.AttemptedAt)
		}
	}
	at, err := ParseTimestamp(res.RecordedAt())
	if err != nil || claimed.IsZero() {
		t.Fatalf("recorded_at %q (%v), claim marker %v", res.RecordedAt(), err, claimed)
	}
	if d := claimed.Sub(at); d > time.Minute || d < -time.Minute {
		t.Errorf("one ResolveHalt call: its claim marker says %v, its result says %v", claimed.UTC(), at)
	}
}

// REVIEW: Status falls back to aborted_at only when recorded_at is absent; a garbage recorded_at
// hides a valid aborted_at.
func TestReview_EndedAtFallsBackToAbortedAt(t *testing.T) {
	ctx := context.Background()
	j, _ := pcJournal(t)
	pcStart(t, j, "x", true)
	v := mustJSONValue(abortValue{Reason: "boom", AbortedAt: "2026-10-14T07:30:15.123Z"})
	if _, err := j.put(ctx, "x", runAbortedStep, Record{Kind: StepValue, Result: v, ApproverSignature: &ApproverSignature{RecordedAt: "garbage"}}); err != nil {
		t.Fatal(err)
	}
	st, err := Status(ctx, j, "x")
	if err != nil || st.EndedAt != "2026-10-14T07:30:15.123Z" {
		t.Errorf("EndedAt = %q (%v), want the valid aborted_at", st.EndedAt, err)
	}
}

// REVIEW: a saga Cancel repeated with another clock keeps one request, first time.
func TestReview_SagaCancelRepeatKeepsFirstTime(t *testing.T) {
	ctx := context.Background()
	j, _ := pcJournal(t)
	pcStart(t, j, "s", true)
	for _, at := range []time.Time{timesA, timesB} {
		if err := Cancel(ctx, j, "s", "stop", WithClock(fixedClock(at))); err != nil {
			t.Fatal(err)
		}
	}
	recs, _ := j.History(ctx, "s")
	n := 0
	for _, r := range recs {
		if r.Name == runCancelRequestedStep {
			n++
			if r.RecordedAt() != stampA {
				t.Errorf("recorded_at = %q, want first %q", r.RecordedAt(), stampA)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d requests", n)
	}
}
