// awaitfor.go adds AwaitFor: Await with a durable deadline. It is the ambient
// "wait for an event, but give up after D" primitive, composed from the in-package
// signal scan (see Await in pause.go) and the durable-timer pattern (see waitUntil in
// pause.go): the signal delivery and the wake time are both journaled steps, so the
// race between "signal arrived" and "timeout elapsed" resolves deterministically across
// resume and crash-recovery. It adds no new persistence model; it reuses Durable.Do.

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// AwaitFor blocks the current run until either a single-shot signal named `name` is
// delivered or the duration d elapses, whichever happens first. It returns (payload,
// true, nil) when the signal wins, or (zero, false, nil) when the timeout wins. Call it
// from inside a retry-safe tool (Safety.ReadOnly or Idempotent), like Await and Sleep:
// on resume the tool re-runs from the top until the await resolves, so everything before
// the AwaitFor call must be safe to repeat.
//
// It is Await composed with a durable timer. The deadline is journaled once, on the first
// encounter (at-most-once by name), as now()+d, so a resumed or crash-recovered run
// races against the same absolute instant rather than restarting the clock. While neither
// side has resolved, AwaitFor schedules a wake with the bound Waker (if any) and returns
// *Awaiting, pausing the run durably exactly like Await. The bool return is only
// meaningful when err is nil.
func AwaitFor[T any](ctx context.Context, name string, d time.Duration) (T, bool, error) {
	var zero T
	dur, runID, ok := runContext(ctx)
	if !ok {
		return zero, false, fmt.Errorf("agent: AwaitFor called outside a running agent: %w", ErrConfig)
	}

	// Signal first: if the single-shot signal is already delivered, it wins the race.
	recs, err := dur.History(ctx, runID)
	if err != nil {
		return zero, false, fmt.Errorf("agent: awaitfor %q: %w (%w)", name, err, ErrStorage)
	}
	sig := signalStep(name)
	for _, r := range recs {
		if r.Kind == StepSignal && r.Name == sig {
			var v T
			if len(r.Result) > 0 {
				if err := json.Unmarshal(r.Result, &v); err != nil {
					return zero, false, fmt.Errorf("agent: decode signal %q: %w (%w)", name, err, ErrProtocol)
				}
			}
			return v, true, nil
		}
	}

	// Journal the deadline once (at-most-once by name), computed as now()+d on the first
	// encounter, so it is stable across resume and restart (same pattern as waitUntil).
	now := clockFrom(ctx)
	timeout := "await-timeout:" + name
	rec, err := dur.Do(ctx, runID, timeout, func(context.Context) (Record, error) {
		b, err := json.Marshal(now().Add(d))
		if err != nil {
			return Record{}, fmt.Errorf("agent: encode deadline for %q: %w (%w)", name, err, ErrConfig)
		}
		return Record{Kind: StepValue, Result: b}, nil
	})
	if err != nil {
		return zero, false, fmt.Errorf("agent: awaitfor deadline %q: %w (%w)", name, err, ErrStorage)
	}
	var deadline time.Time
	if err := json.Unmarshal(rec.Result, &deadline); err != nil {
		return zero, false, fmt.Errorf("agent: decode deadline for %q: %w (%w)", name, err, ErrProtocol)
	}

	// Timeout won: no signal, and the deadline has passed.
	if !now().Before(deadline) {
		return zero, false, nil
	}

	// Neither side resolved yet: schedule a wake to expire the deadline (if a Waker is
	// bound), then pause durably. Re-invoking the run after delivery or after the deadline
	// re-scans and resolves the race.
	if w := wakerFrom(ctx); w != nil {
		w.Schedule(runID, timeout, deadline)
	}
	return zero, false, &Awaiting{RunID: runID, Name: name}
}
