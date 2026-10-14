package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/bide-ai/bide/internal/journalhook"
)

// startedRun records a run:start for run r, a saga's or not, and returns the store.
func startedRun(t *testing.T, saga bool) *MemStore {
	t.Helper()
	s := NewMemStore()
	start := RunStart{Input: UserText("go"), Kind: RunKindAgent, Saga: saga}
	if _, _, _, err := journalhook.Begin(context.Background(), mustJournal(s), "r", start); err != nil {
		t.Fatal(err)
	}
	return s
}

// Cancel refuses a nil journal, an empty run ID, and a run that never started, and a store it cannot
// read or write is reported with the run still running (nothing recorded as cancelled).
func TestCancel_RefusalsAndFaults(t *testing.T) {
	ctx := context.Background()
	if err := Cancel(ctx, nil, "r", "x"); !errors.Is(err, ErrConfig) {
		t.Errorf("nil journal = %v; want ErrConfig", err)
	}
	if err := Cancel(ctx, memJournal(), "", "x"); !errors.Is(err, ErrConfig) {
		t.Errorf("empty run ID = %v; want ErrConfig", err)
	}
	if err := Cancel(ctx, memJournal(), "never", "x"); !errors.Is(err, ErrNotStarted) {
		t.Errorf("run never started = %v; want ErrNotStarted", err)
	}
	for name, c := range map[string]struct {
		wrap func(Store) Store
	}{
		"end marker unreadable": {func(s Store) Store { return covGetFail{s, runCompleteStep} }},
		"start unreadable":      {func(s Store) Store { return covGetFail{s, runStartStep} }},
		"cancel unwritable":     {func(s Store) Store { return covInsertFail{s, runCancelledStep} }},
	} {
		s := startedRun(t, false)
		j := mustJournal(c.wrap(s))
		if err := Cancel(ctx, j, "r", "stop"); err == nil || errors.Is(err, ErrRunEnded) {
			t.Errorf("%s: Cancel = %v; want a store error", name, err)
		}
		if st, err := Status(ctx, mustJournal(s), "r"); err != nil || st.State != RunStarted {
			t.Errorf("%s: Status after the failed Cancel = %+v, %v; want started", name, st, err)
		}
	}
	// A saga's cancellation is a request for its rollback, which also must not be lost silently.
	sj := mustJournal(covInsertFail{startedRun(t, true), runCancelRequestedStep})
	if err := Cancel(ctx, sj, "r", "stop"); err == nil {
		t.Error("Cancel of a saga whose request cannot be written succeeded")
	}
}

// A saga that is cancelled records a rollback request, not an end marker: it stays started until
// its rollback finishes, and the reason is kept.
func TestCancel_SagaRecordsARequest(t *testing.T) {
	ctx := context.Background()
	j := mustJournal(startedRun(t, true))
	if err := Cancel(ctx, j, "r", "customer asked"); err != nil {
		t.Fatal(err)
	}
	if st, err := Status(ctx, j, "r"); err != nil || st.State != RunStarted {
		t.Fatalf("Status = %+v, %v; want started", st, err)
	}
	if r, ok, err := j.Get(ctx, "r", runCancelRequestedStep); err != nil || !ok || endText(r) != "customer asked" {
		t.Fatalf("request = %+v, %v, %v", r, ok, err)
	}
	if _, ok, _ := j.Get(ctx, "r", runCancelledStep); ok {
		t.Fatal("a saga's Cancel wrote the end marker before the rollback")
	}
}

// Status reports a store it cannot read, and reads a reason from both the current and the older
// string form of a saga's abort.
func TestStatus_FaultsAndTerminalText(t *testing.T) {
	ctx := context.Background()
	if _, err := Status(ctx, nil, "r"); !errors.Is(err, ErrConfig) {
		t.Errorf("nil journal = %v; want ErrConfig", err)
	}
	if _, err := Status(ctx, mustJournal(loadFails{NewMemStore()}), "r"); !errors.Is(err, ErrStorage) {
		t.Errorf("unreadable store = %v; want ErrStorage", err)
	}
	for name, c := range map[string]struct {
		result string
		want   string
	}{
		"current form": {`{"reason":"no seats","aborted_at":"2026-10-13T00:00:00.000Z"}`, "no seats"},
		"older string": {`"no seats"`, "no seats"},
		"other value":  {`[1]`, ""},
	} {
		s := startedRun(t, true)
		j := mustJournal(s)
		writeRaw(t, j, "r", runAbortedStep, Record{Kind: StepValue, Result: []byte(c.result)})
		st, err := Status(ctx, j, "r")
		if err != nil || st.State != RunAborted || st.Terminal != c.want {
			t.Errorf("%s: Status = %+v, %v; want aborted with %q", name, st, err, c.want)
		}
	}
}
