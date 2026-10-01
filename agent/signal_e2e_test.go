package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// End-to-end tests for durable signals: the signal pause/resume path composed with the
// at-most-once side-effect guarantee under crashes (DST), and the deliver-then-wake idiom
// driven by a real Waker. The per-stage tests (signal_test.go, awaitfor_test.go,
// channel_test.go) cover the units; these prove the whole loop.

// awaitThenChargeModel calls the retry-safe "await" tool first (which pauses until a signal
// is delivered), then the NON-idempotent "charge" tool once the await has resolved, then
// answers. It is a pure function of how many tool results are already in the conversation,
// so re-calling it after a crash returns the same turn (replayable model).
type awaitThenChargeModel struct{}

func (awaitThenChargeModel) Stream(_ context.Context, req Request) (*Stream, error) {
	results := 0
	for _, m := range req.Messages {
		if m.Role == RoleTool {
			for _, p := range m.Parts {
				if _, ok := p.(ToolResult); ok {
					results++
				}
			}
		}
	}
	var emits []Emit
	switch results {
	case 0:
		emits = toolTurn("c-await", "await", `{}`) // gate on the signal first
	case 1:
		emits = toolTurn("c-charge", "charge", `{}`) // then the non-idempotent action
	default:
		emits = textTurn("done")
	}
	ch := make(chan Emit, len(emits))
	for _, e := range emits {
		ch <- e
	}
	close(ch)
	return NewStream(ch), nil
}

func runAwaitCharge(mem Durable, awaitCalls, chargeCount *int, crashAt int) error {
	awaitT := &awaitTool{name: "await", safety: Safety{ReadOnly: true}, sig: "go", calls: awaitCalls}
	chargeT := chargeTool{count: chargeCount}
	a := New(awaitThenChargeModel{}, crashJournal(mem, crashAt), awaitT, chargeT).SetMaxConcurrency(1)
	_, err := a.Run(context.Background(), "dst-sig", "start")
	return err
}

// A signal-gated non-idempotent action fires at most once across every crash schedule. The
// run awaits a delivered signal, resolves it on resume, then charges; a crash swept across
// every write point must never double-charge, and the run always ends completed or halted.
// This proves durable signals compose with the at-most-once moat end to end.
func TestDST_Signal_NoDoubleFire_CrashSweep(t *testing.T) {
	haltSeen := false
	for crashAt := 1; crashAt <= 40; crashAt++ {
		var awaitCalls, count int
		mem := NewMemStore()
		// The signal is delivered (persisted) up front, so the await resolves on the first run;
		// the crash sweep then exercises the resume-and-charge path.
		if err := Signal(context.Background(), mem, "dst-sig", "go", "payload"); err != nil {
			t.Fatalf("Signal: %v", err)
		}

		err := runAwaitCharge(mem, &awaitCalls, &count, crashAt)
		crashed := errors.Is(err, errCrash)
		for errors.Is(err, errCrash) { // resume without further crashes
			err = runAwaitCharge(mem, &awaitCalls, &count, 0)
		}

		if count > 1 {
			t.Fatalf("crashAt=%d: charge fired %d times after signal-driven resume: DOUBLE FIRE", crashAt, count)
		}
		var halt *ResumeHalt
		switch {
		case err == nil:
			if count != 1 {
				t.Fatalf("crashAt=%d: completed run charged %d times, want 1", crashAt, count)
			}
		case errors.As(err, &halt):
			haltSeen = true // charge fired but its outcome was unknown at the crash: halted, not retried
		default:
			t.Fatalf("crashAt=%d: unexpected terminal error: %v", crashAt, err)
		}

		if !crashed { // crashAt exceeded the clean-run write count: sweep complete
			break
		}
	}
	if !haltSeen {
		t.Fatal("no crash point exercised ResumeHalt: the halt path was never tested")
	}
}

// Deliver-then-wake, end to end: a run awaits a signal and pauses; an external deliverer
// records the signal and schedules a wake at the current time; the Waker then resumes the run,
// which resolves the await and completes. This is the idiom stage 4 (a separate Notifier)
// would have added, shown to already work with the existing Waker.
func TestSignal_DeliverThenWake(t *testing.T) {
	mem := NewMemStore()
	var got string
	awaitT := &awaitTool{name: "await", safety: Safety{ReadOnly: true}, sig: "go", calls: new(int), got: &got}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "await", `{}`), textTurn("done")}}

	var a *Agent
	var completed bool
	waker := NewMemWaker(func(ctx context.Context, runID string) error {
		out, err := a.Run(ctx, runID, "hi")
		var awt *Awaiting
		if errors.As(err, &awt) {
			return nil // still waiting is not an error
		}
		if err != nil {
			return err
		}
		if textOf(out) == "done" {
			completed = true
		}
		return nil
	})
	a = New(m, mem, awaitT)
	ctx := ContextWithWaker(context.Background(), waker)

	// First run pauses on the await (plain Await does not self-schedule a wake).
	_, err := a.Run(ctx, "r", "hi")
	var awt *Awaiting
	if !errors.As(err, &awt) {
		t.Fatalf("first run err = %v, want *Awaiting", err)
	}

	// The deliverer records the signal, then schedules a wake now (the deliver-then-wake idiom).
	if err := Signal(context.Background(), mem, "r", "go", "payload"); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	waker.Schedule(context.Background(), Wake{RunID: "r", Name: "signal:go", FireAt: time.Time{}}) // zero time is before now, so the wake is due

	n, err := waker.Fire(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if n != 1 {
		t.Fatalf("Fire resumed %d runs, want 1", n)
	}
	if !completed {
		t.Fatal("run did not complete after deliver-then-wake")
	}
	if got != "payload" {
		t.Fatalf("await received %q, want payload", got)
	}
}
