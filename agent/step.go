package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// ClaimAttempt writes the attempt marker named name as an exclusive claim and reports whether
// the caller won it. It stamps rec with a fresh random claim id and records it with Do; because
// Do records a name at most once, exactly one driver's marker is stored, and a driver won only
// if the marker Do returns carries its own claim. A driver that loses must not run the side
// effect the marker guards: another driver owns it and may be running it right now.
//
// This makes at-most-once independent of leasing. A lease cannot guarantee mutual exclusion (a
// holder stalled past its TTL wakes still believing it holds the lease), but two drivers that
// overlap still cannot both win the claim, since the store's atomic insert decides the winner.
// With a single driver the claim is always won, so a run with no contention behaves exactly as
// before.
func ClaimAttempt(ctx context.Context, d Durable, runID, name string, rec Record) (won bool, got Record, err error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return false, Record{}, fmt.Errorf("claim %s: %w (%w)", name, err, ErrStorage)
	}
	claim := hex.EncodeToString(b[:])
	rec.Claim = claim
	got, err = d.Do(ctx, runID, name, func(context.Context) (Record, error) { return rec, nil })
	if err != nil {
		return false, Record{}, err
	}
	return got.Claim == claim, got, nil
}

// claimAttempt is ClaimAttempt for a marker key a probe of this process may be reading at the
// same moment (see doShared): it claims again rather than fail with the probe's outcome.
func claimAttempt(ctx context.Context, d Durable, runID, name string, rec Record) (bool, Record, error) {
	for {
		won, got, err := ClaimAttempt(ctx, d, runID, name, rec)
		if !errors.Is(err, errNoRecord) {
			return won, got, err
		}
		if err := ctx.Err(); err != nil {
			return false, Record{}, err
		}
	}
}

// hasValueStep reports whether runID's journal holds a StepValue record named name.
func hasValueStep(ctx context.Context, store Durable, runID, name string) (bool, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return false, fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
	}
	for _, r := range recs {
		if r.Kind == StepValue && r.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// Step runs fn as a named durable step and returns its typed result. On resume, a
// completed step returns its recorded result without re-running fn. This is the
// Option-B authoring primitive: write plain Go control flow, and name the operations
// that must survive a crash.
//
//	inv, err := agent.Step(ctx, dur, runID, "fetch-invoice", fetchInvoice, agent.StepSafety(agent.Safety{ReadOnly: true}))
//	res, err := agent.Step(ctx, dur, runID, "reserve", reserve) // a side effect: at most once
//
// A step runs at most once, like a tool call. By default it is treated as a side effect: an
// attempt marker is journaled before fn runs, so if the process dies after fn's effect and
// before its result is recorded, the resumed step returns *ResumeHalt instead of running fn
// again. Clear it with ResolveStepHalt (the halt's ToolUseID is the step name; its ToolName is
// empty) once the true outcome is known. A step that is safe to re-run declares it with StepSafety (ReadOnly,
// Idempotent, or an IdempotencyKey); it then skips the marker and simply re-runs after a crash.
//
// If fn returns an error, nothing is recorded but the marker: a side-effecting step whose fn
// failed halts on the next attempt too, since a failed call may still have taken effect.
//
// T must be JSON-serializable: the result is marshaled into the journal, so a struct with
// unexported fields round-trips those fields to their zero values (encoding/json skips them)
// with no error reported. Return exported fields, a map, or a pointer whose fields are exported.
//
// name must not start with a prefix the engine reserves for its own journal keys ("@", "run:",
// "tool:", "attempt:", "approval:", "signal:", and the rest; see IsReservedStepName): such a
// name is ErrConfig.
func Step[T any](ctx context.Context, d Durable, runID, name string, fn func(context.Context) (T, error), opts ...StepOption) (T, error) {
	if err := checkStepName("Step", name); err != nil {
		var zero T
		return zero, err
	}
	return step(ctx, d, runID, name, fn, opts...)
}

// step is Step without the check on name, for the engine's own steps.
func step[T any](ctx context.Context, d Durable, runID, name string, fn func(context.Context) (T, error), opts ...StepOption) (T, error) {
	var out T
	if err := ctx.Err(); err != nil {
		return out, err // a cancelled caller starts no new step
	}
	var cfg stepConfig
	for _, o := range opts {
		o(&cfg)
	}
	// The attempt marker is an exclusive claim, as for a tool call: a driver that did not
	// write it (a resume after a crash, or a second driver of the same run) must not run fn.
	// An earlier attempt recorded as not started does not count (see attempt.go): the claim is
	// then for the next attempt.
	base := stepAttemptStep(name)
	claimed, won := true, false
	var marker Record
	var markerKey string
	var attemptedAt time.Time
	if !cfg.safety.RetrySafe() {
		w, got, key, err := claimNextAttempt(ctx, d, runID, base,
			Record{Kind: StepAttempt, ToolUseID: name, AttemptedAt: time.Now().UnixMilli()})
		if err != nil {
			return out, err
		}
		claimed, won, marker, markerKey = w, w, got, key
		attemptedAt = markerTime(got.AttemptedAt)
	}
	var started atomic.Bool // fn was called: from here on its effect may have fired
	rec, err := d.Do(ctx, runID, name, func(ctx context.Context) (Record, error) {
		if claimed && cfg.safety.RetrySafe() {
			// A retry-safe step writes no marker, but an earlier attempt of it may have, if it
			// was declared a side effect then. The marker is the attempt's recorded safety, so
			// the step halts as it would have, rather than run a side effect a second time,
			// unless that attempt is recorded as never started.
			m, ok, err := liveAttempt(ctx, d, runID, base)
			if err != nil {
				return Record{}, err
			}
			if ok {
				claimed = false
				attemptedAt = markerTime(m.AttemptedAt)
			}
		}
		if !claimed {
			// Attempted before, with no recorded result: the outcome is unknown.
			return Record{}, &ResumeHalt{RunID: runID, RootRunID: runID, ToolUseID: name, AttemptedAt: attemptedAt}
		}
		if err := ctx.Err(); won && err != nil {
			return Record{}, err // cancelled after the claim: fn is not called, and that is recorded below
		}
		started.Store(true)
		v, err := fn(stepOnceScope(ctx, runID, name)) // the step numbers its own NextOnceKey keys
		if err != nil {
			return Record{}, err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return Record{}, err
		}
		return Record{Kind: StepValue, Result: b}, nil
	})
	if err != nil {
		if won && !started.Load() {
			// This driver claimed the attempt and never called fn (it was cancelled, or the store
			// failed, first): record that, so the next attempt runs fn instead of halting.
			if nerr := recordNotStarted(ctx, d, runID, markerKey, marker); nerr != nil {
				err = fmt.Errorf("%w (%w)", err, nerr)
			}
		}
		return out, err
	}
	if rec.IsError { // resolved by ResolveHalt as failed
		return out, fmt.Errorf("step %q was resolved as failed: %s: %w", name, rec.Result, ErrTool)
	}
	if len(rec.Result) == 0 {
		return out, nil
	}
	err = json.Unmarshal(rec.Result, &out)
	return out, err
}

// StepOption configures Step.
type StepOption func(*stepConfig)

type stepConfig struct{ safety Safety }

// StepSafety declares how safe a step is to re-run, as Safety does for a tool. A step that is
// RetrySafe (ReadOnly, Idempotent, or keyed) re-runs after a crash; any other step halts.
func StepSafety(s Safety) StepOption { return func(c *stepConfig) { c.safety = s } }
