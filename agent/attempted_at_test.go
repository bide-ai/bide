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
	j := mustJournal(s)
	turn := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "charge", Args: []byte(`{}`)}}}
	if _, err := j.do(ctx, "r", modelStep(0), func(context.Context) (Record, error) {
		return Record{Kind: StepModel, Message: &turn}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.do(ctx, "r", toolAttemptStep("c1"), func(context.Context) (Record, error) {
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
	for _, ms := range []int64{0, -1, -now.UnixMilli()} {
		s := mustJournal(markerAt(t, ms))
		err := ResolveHalt(ctx, s, HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}, Outcome{Result: "charged", IsError: false}, WithMinHaltAge(time.Hour), WithClock(func() time.Time { return now }))
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
	s := mustJournal(markerAt(t, now.Add(24*time.Hour).UnixMilli()))
	err := ResolveHalt(ctx, s, HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}, Outcome{Result: "charged", IsError: false}, WithMinHaltAge(time.Minute), WithClock(func() time.Time { return now }))
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
	a := mustNew(NewScriptedModel(), mustJournal(markerAt(t, -1)), WithTools(charge))
	_, err := a.Run(ctx, "r", UserText("go"))
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) {
		t.Fatalf("Run = %v, want *ResumeHalt", err)
	}
	if !halt.AttemptedAt.IsZero() {
		t.Errorf("ResumeHalt.AttemptedAt = %v, want zero for a marker stamped -1", halt.AttemptedAt)
	}

	// A Step's marker too.
	s := memJournal()
	if _, err := s.do(ctx, "r", stepAttemptStep("reserve"), func(context.Context) (Record, error) {
		return Record{Kind: StepAttempt, ToolUseID: "reserve", AttemptedAt: -1}, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Step(ctx, "r", "reserve", func(context.Context) (int, error) { return 1, nil })
	if !errors.As(err, &halt) {
		t.Fatalf("Step = %v, want *ResumeHalt", err)
	}
	if !halt.AttemptedAt.IsZero() {
		t.Errorf("Step ResumeHalt.AttemptedAt = %v, want zero for a marker stamped -1", halt.AttemptedAt)
	}
}

// A retry-safe Step that finds an earlier attempt's marker halts; the marker's non-positive
// timestamp is reported as unknown.
func TestStepHalt_RetrySafeFindsNonPositiveMarker(t *testing.T) {
	ctx := context.Background()
	s := memJournal()
	if _, err := s.do(ctx, "r", stepAttemptStep("reserve"), func(context.Context) (Record, error) {
		return Record{Kind: StepAttempt, ToolUseID: "reserve", AttemptedAt: -1}, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Step(ctx, "r", "reserve", func(context.Context) (int, error) { return 1, nil },
		WithSafety(Safety{ReadOnly: true}))
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) {
		t.Fatalf("Step = %v, want *ResumeHalt", err)
	}
	if !halt.AttemptedAt.IsZero() {
		t.Errorf("ResumeHalt.AttemptedAt = %v, want zero", halt.AttemptedAt)
	}
}

// claimRacer is a MemStore where another driver writes the attempt marker of call c1, stamped -1,
// just before this driver's claim, so this driver loses the claim.
type claimRacer struct{ *MemStore }

func (s claimRacer) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if name == toolAttemptStep("c1") {
		other, err := JournalEntry(name, Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: -1, claim: "other"})
		if err != nil {
			return Entry{}, false, err
		}
		if _, _, err := s.MemStore.Insert(ctx, runID, name, other); err != nil {
			return Entry{}, false, err
		}
	}
	return s.MemStore.Insert(ctx, runID, name, data)
}

// A driver that loses the claim to a marker stamped -1 reports the attempt time as unknown.
func TestResumeHalt_LostClaimToNonPositiveMarker(t *testing.T) {
	charge := Func("charge", "", Safety{}, func(context.Context, struct{}) (string, error) { return "ok", nil })
	m := &sagaTurns{turns: [][][3]string{{{"c1", "charge", `{}`}}}}
	_, err := mustNew(m, mustJournal(claimRacer{NewMemStore()}), WithTools(charge)).Run(context.Background(), "r", UserText("go"))
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) {
		t.Fatalf("Run = %v, want *ResumeHalt from the lost claim", err)
	}
	if !halt.AttemptedAt.IsZero() {
		t.Errorf("ResumeHalt.AttemptedAt = %v, want zero", halt.AttemptedAt)
	}
}

// A saga rollback that stops on a started call reports a non-positive marker's time as unknown.
func TestSagaRollbackHalt_NonPositiveMarker(t *testing.T) {
	ctx := context.Background()
	pay := CompensatedFunc("pay", "", Safety{},
		func(context.Context, struct{}) (string, error) { return "p", nil },
		func(context.Context, struct{}, string) error { return nil })
	book := Func("book", "", Safety{}, func(context.Context, struct{}) (string, error) { return "", errors.New("no seats") })
	s := memJournal()
	turn := Message{Role: RoleAssistant, Parts: []Part{
		ToolUse{ID: "p1", Name: "pay", Args: []byte(`{}`)},
		ToolUse{ID: "b1", Name: "book", Args: []byte(`{}`)},
	}}
	for name, rec := range map[string]Record{
		modelStep(0):          {Kind: StepModel, Message: &turn},
		toolAttemptStep("p1"): {Kind: StepAttempt, ToolUseID: "p1", AttemptedAt: -1},
		ToolResultStep("b1"):  {Kind: StepSagaFail, ToolUseID: "b1", Result: []byte(`"no seats"`)},
	} {
		if _, err := s.do(ctx, "r", name, func(context.Context) (Record, error) { return rec, nil }); err != nil {
			t.Fatal(err)
		}
	}
	_, err := mustNew(&sagaTurns{}, s, WithTools(pay, book)).Run(ctx, "r", UserText("trip"), WithSaga())
	var aborted *SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("RunSaga = %v, want *SagaAborted", err)
	}
	var halt *OutcomeUnknown
	if !errors.As(aborted.CompensateErr, &halt) || halt.Op.ID != "p1" {
		t.Fatalf("CompensateErr = %v, want a ResumeHalt on p1", aborted.CompensateErr)
	}
	if !halt.AttemptedAt.IsZero() {
		t.Errorf("ResumeHalt.AttemptedAt = %v, want zero", halt.AttemptedAt)
	}
}
