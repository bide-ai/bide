package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// markerAt journals, for run "r", the model turn that called "charge" as c1 and an attempt
// marker for c1 stamped ms (a tampered or hand-written row when ms is not positive).
func markerAt(t *testing.T, ms int64) *MemStore {
	t.Helper()
	ctx := context.Background()
	s := NewMemStore()
	turn := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "charge", Args: []byte(`{}`)}}}
	if _, err := s.Do(ctx, "r", modelStep(0), func(context.Context) (Record, error) {
		return Record{Kind: StepModel, Message: &turn}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Do(ctx, "r", toolAttemptStep("c1"), func(context.Context) (Record, error) {
		return Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: ms}, nil
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

// A marker's AttemptedAt that is not a positive Unix-millis time is no timestamp at all: it must
// not pass for an effect attempted in 1969, which would make any grace period look long elapsed.
// WithMinHaltAge then refuses to resolve blind, exactly as for a marker without a timestamp.
func TestResolveHalt_NonPositiveAttemptedAtIsNoTimestamp(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, ms := range []int64{-1, -now.UnixMilli()} {
		s := markerAt(t, ms)
		err := ResolveHalt(ctx, s, "r", "c1", "charged", false,
			WithMinHaltAge(time.Hour), WithNow(func() time.Time { return now }))
		if !errors.Is(err, ErrConfig) {
			t.Errorf("AttemptedAt %d: ResolveHalt = %v, want ErrConfig (no usable timestamp)", ms, err)
		}
		if h, _ := s.History(ctx, "r"); hasResult(h, "c1") {
			t.Errorf("AttemptedAt %d: a result was recorded despite the min halt age", ms)
		}
	}
}

// A marker stamped in the future (clock skew between nodes, or a tampered row) is younger than
// any grace period: the resolution waits.
func TestResolveHalt_FutureAttemptedAtIsTooYoung(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := markerAt(t, now.Add(24*time.Hour).UnixMilli())
	err := ResolveHalt(ctx, s, "r", "c1", "charged", false,
		WithMinHaltAge(time.Minute), WithNow(func() time.Time { return now }))
	var young *HaltTooYoung
	if !errors.As(err, &young) {
		t.Fatalf("ResolveHalt = %v, want *HaltTooYoung", err)
	}
}

// ResumeHalt.AttemptedAt reports an unknown time as zero, never a 1969 instant from a
// non-positive marker.
func TestResumeHalt_NonPositiveAttemptedAtIsZero(t *testing.T) {
	ctx := context.Background()
	charge := Func("charge", "", Safety{}, func(context.Context, struct{}) (string, error) { return "ok", nil })
	a := New(NewScriptedModel(), markerAt(t, -1), charge)
	_, err := a.Run(ctx, "r", "go")
	var halt *ResumeHalt
	if !errors.As(err, &halt) {
		t.Fatalf("Run = %v, want *ResumeHalt", err)
	}
	if !halt.AttemptedAt.IsZero() {
		t.Errorf("ResumeHalt.AttemptedAt = %v, want zero for a marker stamped -1", halt.AttemptedAt)
	}

	// A Step's marker too.
	s := NewMemStore()
	if _, err := s.Do(ctx, "r", stepAttemptStep("reserve"), func(context.Context) (Record, error) {
		return Record{Kind: StepAttempt, ToolUseID: "reserve", AttemptedAt: -1}, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err = Step(ctx, s, "r", "reserve", func(context.Context) (int, error) { return 1, nil })
	if !errors.As(err, &halt) {
		t.Fatalf("Step = %v, want *ResumeHalt", err)
	}
	if !halt.AttemptedAt.IsZero() {
		t.Errorf("Step ResumeHalt.AttemptedAt = %v, want zero for a marker stamped -1", halt.AttemptedAt)
	}
}
