package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A recorded interrupt answer or signal payload that does not decode as the type the tool asks for
// is ErrProtocol, not the zero value: the tool must not continue on a value it did not receive.
func TestPause_AnswerOfAnotherTypeIsAProtocolError(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	rc := withRunContext(ctx, j, "r", "", false)
	if err := j.AnswerInterrupt(ctx, "r", "pick", "not a number"); err != nil {
		t.Fatal(err)
	}
	if err := j.Signal(ctx, "r", "hook", "not a number"); err != nil {
		t.Fatal(err)
	}
	if _, err := Interrupt[int](rc, "pick", nil); !errors.Is(err, ErrProtocol) {
		t.Errorf("Interrupt[int] over a string answer = %v; want ErrProtocol", err)
	}
	if _, err := Await[int](rc, "hook"); !errors.Is(err, ErrProtocol) {
		t.Errorf("Await[int] over a string payload = %v; want ErrProtocol", err)
	}
	if v, err := Interrupt[string](rc, "pick", nil); err != nil || v != "not a number" {
		t.Errorf("Interrupt[string] = %q, %v", v, err)
	}
	if v, err := Await[string](rc, "hook"); err != nil || v != "not a number" {
		t.Errorf("Await[string] = %q, %v", v, err)
	}
}

// A journal that cannot be read pauses nothing: Interrupt and Await report ErrStorage rather than
// a pause that a later answer could never resolve.
func TestPause_UnreadableJournalIsStorageError(t *testing.T) {
	ctx := context.Background()
	rc := withRunContext(ctx, mustJournal(loadFails{NewMemStore()}), "r", "", false)
	if _, err := Interrupt[string](rc, "pick", nil); !errors.Is(err, ErrStorage) {
		t.Errorf("Interrupt = %v; want ErrStorage", err)
	}
	if _, err := Await[string](rc, "hook"); !errors.Is(err, ErrStorage) {
		t.Errorf("Await = %v; want ErrStorage", err)
	}
}

// Empty run IDs and a payload that cannot be encoded are ErrConfig, and record nothing.
func TestPause_DeliveryRefusals(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	if err := j.AnswerInterrupt(ctx, "", "pick", 1); !errors.Is(err, ErrConfig) {
		t.Errorf("AnswerInterrupt with no run = %v; want ErrConfig", err)
	}
	if err := j.Signal(ctx, "", "hook", 1); !errors.Is(err, ErrConfig) {
		t.Errorf("Signal with no run = %v; want ErrConfig", err)
	}
	if err := j.AnswerInterrupt(ctx, "r", "pick", make(chan int)); !errors.Is(err, ErrConfig) {
		t.Errorf("AnswerInterrupt of an unencodable value = %v; want ErrConfig", err)
	}
	if err := j.Signal(ctx, "r", "hook", make(chan int)); !errors.Is(err, ErrConfig) {
		t.Errorf("Signal of an unencodable payload = %v; want ErrConfig", err)
	}
	if recs, _ := j.History(ctx, "r"); len(recs) != 0 {
		t.Errorf("refused deliveries recorded %d records", len(recs))
	}
}

// Sleep and WaitUntil outside a run are ErrConfig. In a run, the wake time is fixed by the first
// call: a later call with a different deadline still waits to the first, and once the clock passes
// it the wait is over.
func TestPause_WaitUntilFixesTheWakeTime(t *testing.T) {
	ctx := context.Background()
	if err := Sleep(ctx, "t", time.Second); !errors.Is(err, ErrConfig) {
		t.Errorf("Sleep outside a run = %v; want ErrConfig", err)
	}
	if err := WaitUntil(ctx, "t", time.Now()); !errors.Is(err, ErrConfig) {
		t.Errorf("WaitUntil outside a run = %v; want ErrConfig", err)
	}

	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	now := base
	j := memJournal()
	rc := contextWithClock(withRunContext(ctx, j, "r", "", false), func() time.Time { return now })
	deadline := base.Add(time.Hour)
	var tp *TimerPending
	if err := WaitUntil(rc, "t", deadline); !errors.As(err, &tp) || !tp.FireAt.Equal(deadline) {
		t.Fatalf("WaitUntil before the deadline = %v; want a TimerPending at %v", err, deadline)
	}
	if !strings.Contains(tp.Error(), "t") || !strings.Contains(tp.Error(), "sleeping") {
		t.Errorf("TimerPending.Error = %q", tp.Error())
	}
	if err := WaitUntil(rc, "t", base.Add(24*time.Hour)); !errors.As(err, &tp) || !tp.FireAt.Equal(deadline) {
		t.Fatalf("WaitUntil with another deadline = %v; want the first deadline kept", err)
	}
	now = deadline
	if err := WaitUntil(rc, "t", base.Add(24*time.Hour)); err != nil {
		t.Fatalf("WaitUntil at the first deadline = %v; want the wait over", err)
	}
}

// A timer record that cannot be decoded as a time is ErrProtocol, and a store that cannot record
// the wake time is ErrStorage; neither pauses nor continues past the wait.
func TestPause_TimerFaults(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	writeRaw(t, j, "r", timerStep("t"), Record{Kind: StepValue, Result: []byte(`"not a time"`)})
	rc := withRunContext(ctx, j, "r", "", false)
	if err := Sleep(rc, "t", time.Second); !errors.Is(err, ErrProtocol) {
		t.Errorf("Sleep over a corrupt wake time = %v; want ErrProtocol", err)
	}
	fj := mustJournal(covInsertFail{NewMemStore(), "timer"})
	if err := Sleep(withRunContext(ctx, fj, "r", "", false), "t", time.Second); !errors.Is(err, ErrStorage) {
		t.Errorf("Sleep over a failing store = %v; want ErrStorage", err)
	}
}

// The pause errors name the run and what it waits for.
func TestPause_ErrorsNameTheWait(t *testing.T) {
	ref := RunRef{RunID: "run-7", RootRunID: "run-7"}
	for name, c := range map[string]struct {
		err  error
		want []string
	}{
		"interrupt": {&InterruptPending{RunRef: ref, Name: "pick"}, []string{"run-7", "pick", "interrupted"}},
		"signal":    {&SignalPending{RunRef: ref, Name: "hook"}, []string{"run-7", "hook", "signal"}},
	} {
		for _, w := range c.want {
			if !strings.Contains(c.err.Error(), w) {
				t.Errorf("%s: %q lacks %q", name, c.err.Error(), w)
			}
		}
	}
}
