package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
)

// B4: the rollback re-runs a retry-safe compensable call cut off by a sibling's failure. The re-run
// starts a programmatic sub-run (which books) and then reports its outcome unknown. rollbackRun
// refreshes `links` only on the re-run's success path, so on the unknown path the new link is
// never read: the child's completed, compensable book is neither compensated nor listed (only
// "charge" is listed as unknown). The live path walks a failed-unknown call's sub-runs.
func TestAdv127b_RerunUnknownSkipsItsSubRuns(t *testing.T) {
	store := memJournal()
	var undone, calls atomic.Int32
	book := MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
		func(context.Context, struct{}, string) error { undone.Add(1); return nil })
	child, err := New(NewScriptedModel(ToolTurn("k1", "book", `{}`), TextTurn("child done")), store, WithTools(book))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	charge := MustCompensatedFunc("charge", "", func(ctx context.Context, _ struct{}) (string, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done() // cut off by the sibling's failure
			return "", ctx.Err()
		}
		info, _ := RunInfoFrom(ctx)
		if _, err := child.Run(ctx, info.SubRunFor("child"), UserText("work"), WithSaga()); err != nil {
			return "", err
		}
		return "", fmt.Errorf("charge: gateway timeout after the booking: %w", ErrToolOutcomeUnknown)
	},
		func(context.Context, struct{}, string) error { return nil }, WithSafety(Safety{Idempotent: true}),
		WithSubRuns(func(string) *Agent { return child }))
	fail := MustFunc("fail", "", func(context.Context, struct{}) (string, error) {
		<-started
		return "", errors.New("declined")
	})
	_, err = mustNew(t4TwoCalls{}, store, WithTools(charge, fail)).Run(context.Background(), "r", UserText("go"), WithSaga())
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("saga Run = %v, want *SagaAborted", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("charge ran %d times, want 2 (live, then the rollback's re-run)", calls.Load())
	}
	if undone.Load() != 1 && !slices.Contains(ab.Uncompensated, "book") && !slices.Contains(ab.UnknownOutcome, "book") {
		t.Errorf("re-run's sub-run: book undone %d; compensated %v, uncompensated %v, unknown %v, err %v; book listed nowhere",
			undone.Load(), ab.Compensated, ab.Uncompensated, ab.UnknownOutcome, ab.CompensateErr)
	}
	// The rollback finished and marked the run terminal, so no later drive revisits it.
	if _, err := mustNew(t4TwoCalls{}, store, WithTools(charge, fail)).Run(context.Background(), "r", UserText("go"), WithSaga()); errors.As(err, &ab) && undone.Load() == 0 {
		t.Logf("second drive: compensated %v, unknown %v; book still not undone", ab.Compensated, ab.UnknownOutcome)
	}
}
