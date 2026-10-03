package agent

import (
	"context"
	"errors"
	"testing"
)

// A saga that aborted and finished its rollback is over. Recover must not re-drive it and report
// it as a failure on every pass, which buries real recovery failures in noise.
func TestRecover_SkipsAFinishedSagaAbort(t *testing.T) {
	store := memJournal()
	fail := MustFunc("book", "book it", func(context.Context, struct{}) (string, error) { return "", errors.New("no seats") })
	a := mustNew(NewScriptedModel(ToolTurn("b1", "book", `{}`), TextTurn("done")), store, WithTools(fail))
	var aborted *SagaAborted
	if _, err := a.Run(context.Background(), "r1", UserText("go"), WithSaga()); !errors.As(err, &aborted) {
		t.Fatalf("setup: %v", err)
	}
	var redriven int
	for pass := range 2 {
		n, err := Recover(context.Background(), store, func(ctx context.Context, runID string, _ RunStart) error {
			redriven++
			_, err := a.Run(ctx, runID, UserText("go"), WithSaga())
			return err
		})
		if n != 0 || err != nil {
			t.Fatalf("pass %d: Recover re-drove %d runs, err = %v; want the finished abort left alone", pass, n, err)
		}
	}
	if redriven != 0 {
		t.Fatalf("the finished abort was re-driven %d times", redriven)
	}
}
