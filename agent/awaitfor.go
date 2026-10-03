// awaitfor.go adds AwaitFor: Await with a durable deadline. It is the ambient
// "wait for an event, but give up after D" primitive, composed from the in-package
// signal scan (see Await in pause.go) and the durable-timer pattern (see waitUntil in
// pause.go): the signal delivery, the wake time, and the race's outcome are all journaled
// steps, so the race between "signal arrived" and "timeout elapsed" resolves once and
// deterministically across resume and crash-recovery. It adds no new persistence model; it
// reuses Durable.Do.

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// AwaitFor blocks the current run until either a single-shot signal named `name` is
// delivered or the duration d elapses, whichever happens first. It returns (payload,
// true, nil) when the signal wins, or (zero, false, nil) when the timeout wins. Call it
// from inside a retry-safe tool (Safety.ReadOnly or Idempotent), like Await and Sleep:
// on resume the tool re-runs from the top until the await resolves, so everything before
// the AwaitFor call must be safe to repeat. A Waker that fails to schedule the deadline's wake
// fails the run as it does for Sleep.
//
// It is Await composed with a durable timer. The deadline is journaled once, on the first
// encounter (at-most-once by name), as now()+d, so a resumed or crash-recovered run
// races against the same absolute instant rather than restarting the clock. While neither
// side has resolved, AwaitFor schedules a wake for the top-level run with the bound Waker
// (if any) and returns *SignalPending, pausing the run durably exactly like Await. The bool
// return is only meaningful when err is nil.
//
// The outcome is journaled too, at-most-once by name, the first time either side wins. Every
// later evaluation of the same await (the tool re-runs because it pauses afterwards, or the
// process dies before the tool's result is recorded) returns that recorded outcome, so a
// signal delivered after the timeout won cannot flip the call to the signal branch.
func AwaitFor[T any](ctx context.Context, name string, d time.Duration) (T, bool, error) {
	var zero T
	dur, runID, ok := runContext(ctx)
	if !ok {
		return zero, false, fmt.Errorf("agent: AwaitFor called outside a running agent: %w", ErrConfig)
	}

	// Journal the deadline once (at-most-once by name), computed as now()+d on the first
	// encounter, so it is stable across resume and restart (same pattern as waitUntil).
	now := clockFrom(ctx)
	rec, err := dur.do(ctx, runID, awaitTimeoutStep(name), func(context.Context) (Record, error) {
		b, err := marshalJournal(now().Add(d))
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

	// Resolve the race once, durably. Do returns a recorded outcome without evaluating the race
	// again. Otherwise the signal wins if it has been delivered, the timeout wins if the deadline
	// has passed, and with neither nothing is recorded (errAwaitPending) and the run pauses.
	rec, err = dur.do(ctx, runID, awaitResolvedStep(name), func(ctx context.Context) (Record, error) {
		recs, err := dur.History(ctx, runID)
		if err != nil {
			return Record{}, fmt.Errorf("agent: awaitfor %q: %w (%w)", name, err, ErrStorage)
		}
		sig := signalStep(name)
		for _, r := range recs {
			if r.Kind == StepSignal && r.Name == sig {
				return resolutionRecord(name, awaitResolution{Signaled: true, Payload: r.Result})
			}
		}
		if !now().Before(deadline) {
			return resolutionRecord(name, awaitResolution{})
		}
		return Record{}, errAwaitPending
	})
	if errors.Is(err, errAwaitPending) {
		// Neither side resolved yet: schedule a wake to expire the deadline (if a Waker is
		// bound), then pause durably. Re-invoking the run after delivery or after the deadline
		// resolves the race. The wake is for the top-level run: re-running it re-enters any
		// sub-agent down to this AwaitFor, while the sub-run alone cannot be driven by the root
		// agent's resume callback (the same rule as Sleep).
		ref := RunRef{RunID: runID, RootRunID: rootRunID(ctx, runID)}
		if err := scheduleWake(ctx, Wake{RunID: ref.RunID, RootRunID: ref.RootRunID, Name: awaitTimeoutStep(name), FireAt: deadline}); err != nil {
			return zero, false, err // a *wakeError wrapping ErrStorage: fail, record nothing, schedule again on re-drive
		}
		return zero, false, &SignalPending{RunRef: ref, Name: name}
	}
	if err != nil {
		return zero, false, fmt.Errorf("agent: awaitfor %q: %w (%w)", name, err, ErrStorage)
	}
	var res awaitResolution
	if err := json.Unmarshal(rec.Result, &res); err != nil {
		return zero, false, fmt.Errorf("agent: decode awaitfor outcome %q: %w (%w)", name, err, ErrProtocol)
	}
	if !res.Signaled {
		return zero, false, nil
	}
	var v T
	if len(res.Payload) > 0 {
		if err := json.Unmarshal(res.Payload, &v); err != nil {
			return zero, false, fmt.Errorf("agent: decode signal %q: %w (%w)", name, err, ErrProtocol)
		}
	}
	return v, true, nil
}

// awaitResolution is the journaled outcome of an AwaitFor race: whether the signal won and,
// if it did, the signal's payload.
type awaitResolution struct {
	Signaled bool            `json:"signaled"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// errAwaitPending tells Do that an AwaitFor race has no winner yet, so nothing is recorded.
var errAwaitPending = errors.New("agent: awaitfor pending")

func resolutionRecord(name string, res awaitResolution) (Record, error) {
	b, err := marshalJournal(res)
	if err != nil {
		return Record{}, fmt.Errorf("agent: encode awaitfor outcome %q: %w (%w)", name, err, ErrConfig)
	}
	return Record{Kind: StepValue, Result: b}, nil
}

func awaitResolvedStep(name string) string { return "await-resolved:" + name }
