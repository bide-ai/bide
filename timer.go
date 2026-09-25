package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Sleeping is returned by Run when a tool called Sleep or WaitUntil and the wake time has not yet
// passed. The run has paused durably at the timer: its wake time is journaled, so the pause
// survives a restart. Re-invoke Run with the same runID at or after FireAt to resume (a Waker does
// this automatically; otherwise the deployment re-invokes on its own schedule).
type Sleeping struct {
	RunID  string
	Name   string
	FireAt time.Time
}

func (e *Sleeping) Error() string {
	return fmt.Sprintf("run %s sleeping at %q until %s", e.RunID, e.Name, e.FireAt.Format(time.RFC3339))
}

type clockKey struct{}

// WithClock binds a clock to ctx for the durable timer to read "now". Deployments leave it unset
// (defaulting to time.Now); tests inject a controllable clock to advance time deterministically.
func WithClock(ctx context.Context, now func() time.Time) context.Context {
	return context.WithValue(ctx, clockKey{}, now)
}

func clockFrom(ctx context.Context) func() time.Time {
	if now, ok := ctx.Value(clockKey{}).(func() time.Time); ok && now != nil {
		return now
	}
	return time.Now
}

// Sleep pauses the current run until d has elapsed from the FIRST time this timer was reached,
// durably. Call it from inside a retry-safe tool (the agent loop supplies the run context). On the
// first encounter it journals the wake time (now + d) and returns *Sleeping, pausing the run; on a
// later resume it returns nil once the wake time has passed, and continues past this point. The
// wake time is fixed on the first call and memoized, so a resumed or crash-recovered run waits to
// the same absolute instant rather than restarting the clock. Use distinct names for distinct
// timers. Sleep requires a retry-safe tool (Safety.ReadOnly or Idempotent), like Interrupt.
func Sleep(ctx context.Context, name string, d time.Duration) error {
	return waitUntil(ctx, name, func(now time.Time) time.Time { return now.Add(d) })
}

// WaitUntil pauses the current run until the absolute time `until`, durably. It is the deadline form
// of Sleep: same semantics, but the wake time is the given instant rather than a relative delay.
func WaitUntil(ctx context.Context, name string, until time.Time) error {
	return waitUntil(ctx, name, func(time.Time) time.Time { return until })
}

func waitUntil(ctx context.Context, name string, fireAtFrom func(now time.Time) time.Time) error {
	d, runID, ok := runContext(ctx)
	if !ok {
		return fmt.Errorf("agent: Sleep called outside a running agent: %w", ErrConfig)
	}
	now := clockFrom(ctx)

	// Journal the wake time once (at-most-once by name), so it is stable across resume and restart.
	rec, err := d.Do(ctx, runID, timerStep(name), func(context.Context) (Record, error) {
		b, err := json.Marshal(fireAtFrom(now()))
		if err != nil {
			return Record{}, fmt.Errorf("agent: encode wake time for %q: %w (%w)", name, err, ErrConfig)
		}
		return Record{Kind: StepValue, Result: b}, nil
	})
	if err != nil {
		return fmt.Errorf("agent: timer %q: %w (%w)", name, err, ErrStorage)
	}
	var fireAt time.Time
	if err := json.Unmarshal(rec.Result, &fireAt); err != nil {
		return fmt.Errorf("agent: decode wake time for %q: %w (%w)", name, err, ErrProtocol)
	}

	if !now().Before(fireAt) {
		return nil // due: the wait is over, continue
	}
	// Not yet due: schedule a wake if a Waker is bound, then pause durably.
	if w := wakerFrom(ctx); w != nil {
		w.Schedule(runID, name, fireAt)
	}
	return &Sleeping{RunID: runID, Name: name, FireAt: fireAt}
}

func timerStep(name string) string { return "timer:" + name }
