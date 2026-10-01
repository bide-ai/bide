package agent

import (
	"context"
	"testing"
)

// Rule 7 (L6): Status reads a prefix of the journal (one Load), so it never reports a completed run
// cancelled. Model 10's finding status-gets: a Status that reads one Get per end marker reads
// run:complete (absent), the run completes, Cancel writes run:cancelled second, and the Status
// reads run:cancelled and reports a completed run cancelled. The hooks land those two writes just
// after Status's first read, whichever read that is.
func TestP14Rule07_StatusReadsAPrefixNotOneRoundOfGets(t *testing.T) {
	ctx := context.Background()
	j, hs := p14Journal(t)
	p14Start(t, j, "r", false)
	fired := false
	land := func(runID string) {
		if fired {
			return
		}
		fired = true
		p14Mark(t, j, runID, runCompleteStep, "")
		p14Mark(t, j, runID, runCancelledStep, "late")
	}
	hs.onGet = func(runID, name string) {
		if name == runCompleteStep {
			land(runID)
		}
	}
	hs.onLoad = land
	st, err := Status(ctx, j, "r")
	if err != nil {
		t.Fatal(err)
	}
	if !fired {
		t.Fatal("the hooks never fired: Status read no end marker and no Load")
	}
	if st.State != RunStarted && st.State != RunCompleted {
		t.Fatalf("Status = %+v during the race; want started or completed (the run completed first), never cancelled", st)
	}
	hs.onGet, hs.onLoad = nil, nil
	st, err = Status(ctx, j, "r")
	if err != nil || st.State != RunCompleted || st.Terminal != "" {
		t.Fatalf("Status after the race = %+v, %v; want completed", st, err)
	}
}

// Rule 7: no run:start is NotStarted; no end marker is Started; the first end marker in journal
// order is the state, with its text as Terminal; Records counts the journal, header included.
func TestP14Rule07_StatusStates(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	if st, err := Status(ctx, j, "none"); err != nil || st != (RunStatus{State: RunNotStarted}) {
		t.Fatalf("Status of an empty run = %+v, %v; want not started, 0 records", st, err)
	}
	p14Mark(t, j, "nostart", "signal:x", "hi")
	if st, err := Status(ctx, j, "nostart"); err != nil || st.State != RunNotStarted || st.Records != 2 {
		t.Fatalf("Status of a run with no run:start = %+v, %v; want not started, 2 records", st, err)
	}
	p14Start(t, j, "live", false)
	if st, err := Status(ctx, j, "live"); err != nil || st != (RunStatus{State: RunStarted, Records: 2}) {
		t.Fatalf("Status of a live run = %+v, %v; want started, 2 records", st, err)
	}
	p14Start(t, j, "done", false)
	p14Mark(t, j, "done", runCompleteStep, "")
	p14Mark(t, j, "done", runCancelledStep, "late")
	if st, err := Status(ctx, j, "done"); err != nil || st != (RunStatus{State: RunCompleted, Records: 4}) {
		t.Fatalf("Status of a run completed first = %+v, %v", st, err)
	}
	p14Start(t, j, "c", false)
	if err := Cancel(ctx, j, "c", "operator said so"); err != nil {
		t.Fatal(err)
	}
	p14Mark(t, j, "c", runCompleteStep, "")
	if st, err := Status(ctx, j, "c"); err != nil || st != (RunStatus{State: RunCancelled, Terminal: "operator said so", Records: 4}) {
		t.Fatalf("Status of a run cancelled first = %+v, %v", st, err)
	}
	p14Start(t, j, "a", true)
	p14Mark(t, j, "a", runAbortedStep, "step failed")
	if st, err := Status(ctx, j, "a"); err != nil || st != (RunStatus{State: RunAborted, Terminal: "step failed", Records: 3}) {
		t.Fatalf("Status of an aborted saga = %+v, %v", st, err)
	}
	// A saga whose cancellation's rollback finished: run:cancelled after the request.
	p14Start(t, j, "s", true)
	if err := Cancel(ctx, j, "s", "stop"); err != nil {
		t.Fatal(err)
	}
	if st, err := Status(ctx, j, "s"); err != nil || st.State != RunStarted {
		t.Fatalf("Status of a saga with a pending rollback request = %+v, %v; want started", st, err)
	}
	if _, err := j.put(ctx, "s", runCancelledStep, Record{Kind: StepValue, Result: []byte(`{"reason":"stop"}`)}); err != nil {
		t.Fatal(err)
	}
	if st, err := Status(ctx, j, "s"); err != nil || st != (RunStatus{State: RunCancelled, Terminal: "stop", Records: 4}) {
		t.Fatalf("Status of a rolled-back cancelled saga = %+v, %v", st, err)
	}
	if _, err := Status(ctx, nil, "x"); err == nil {
		t.Fatal("Status with a nil journal: want ErrConfig")
	}
}
