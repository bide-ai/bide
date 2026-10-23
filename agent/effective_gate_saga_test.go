package agent

import (
	"context"
	"errors"
	"testing"
)

// TestSaga_RollbackRerunRecordCarriesTheGate: the rollback's re-run of a retry-safe compensable call
// that a sibling's failure cut off records its result with the gate the tool declares (saga.go,
// through effectiveGates), as the live call's result does. The journal-bytes golden does not reach this
// path, so it is pinned here.
func TestSaga_RollbackRerunRecordCarriesTheGate(t *testing.T) {
	ctx := context.Background()
	var calls int
	chargeStarted := make(chan struct{})
	charge := MustCompensatedFunc("charge", "charge the card", func(ctx context.Context, in chargeArgs) (string, error) {
		calls++
		if calls == 1 {
			close(chargeStarted)
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "ok", nil
	}, func(context.Context, chargeArgs, string) error { return nil },
		WithSafety(Safety{Idempotent: true}), WithApproval(SingleApproval()))
	book := MustFunc("book", "book the flight", func(context.Context, struct{}) (string, error) {
		<-chargeStarted
		return "", errors.New("no seats")
	})
	j := memJournal()
	if err := Approve(ctx, j, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	m := &sagaTurns{turns: [][][3]string{{{"c1", "charge", `{"amount":5}`}, {"b1", "book", `{}`}}}}
	_, err := mustNew(m, j, WithTools(charge, book)).Run(ctx, "r", UserText("trip"), WithSaga())
	var aborted *SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("saga Run = %v, want *SagaAborted", err)
	}
	rec, ok, err := j.Get(ctx, "r", ToolResultStep("c1"))
	if err != nil || !ok {
		t.Fatalf("no result record for c1: %v %v", ok, err)
	}
	if calls != 2 || rec.Approval == nil || !rec.Approval.single() {
		t.Fatalf("charge ran %d times; re-run record's gate = %+v; want 2 runs and the single-approval gate", calls, rec.Approval)
	}
}
