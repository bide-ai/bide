package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"
)

// protocol:claims begin Claim ClaimRetry ClaimInsert ClaimNS RClaim RRetry RInsert RClaimNS

// ClaimAttempt writes the attempt marker named name as an exclusive claim and reports whether
// the caller won it. It stamps rec with a fresh random claim id and inserts it; because a journal
// records a name at most once, exactly one driver's marker is stored, and a driver won only if the
// marker it gets back carries its own claim (see Record.ClaimID). A driver that loses must not run
// the side effect the marker guards: another driver owns it and may be running it right now.
//
// This makes at-most-once independent of leasing. A lease cannot guarantee mutual exclusion (a
// holder stalled past its TTL wakes still believing it holds the lease), but two drivers that
// overlap still cannot both win the claim, since the store's atomic insert decides the winner.
// With a single driver the claim is always won, so a run with no contention behaves exactly as
// before.
//
// Every claim is made with a fresh id. If the marker cannot be written, the claim records that
// its attempt did not start, and if even that record cannot be written, the process remembers it
// and writes it again at the next claim of the same name, which then loses to the marker its
// earlier claim may have left: a caller that claims one name only (rather than numbered
// re-attempts, as Step and tool calls do) halts on it, which is safe.
func ClaimAttempt(ctx context.Context, d *Journal, runID, name string, rec Record) (won bool, got Record, err error) {
	j := d
	return j.claim(ctx, runID, name, rec)
}

// protocol:claims end

// Step runs fn as a named durable step and returns its typed result. On resume, a
// completed step returns its recorded result without re-running fn. This is the
// Option-B authoring primitive: write plain Go control flow, and name the operations
// that must survive a crash.
//
//	inv, err := agent.Step(ctx, dur, runID, "fetch-invoice", fetchInvoice, agent.WithSafety(agent.Safety{ReadOnly: true}))
//	res, err := agent.Step(ctx, dur, runID, "reserve", reserve) // a side effect: at most once
//
// A step runs at most once, like a tool call. By default it is treated as a side effect: an
// attempt marker is journaled before fn runs, so if the process dies after fn's effect and
// before its result is recorded, the resumed step returns *OutcomeUnknown instead of running fn
// again. Clear it with ResolveHaltRef (the halt's Op is OpRef{Kind: OpStep, ID: name}) once the
// true outcome is known. A step that is safe to re-run declares it with WithSafety (ReadOnly or
// Idempotent); it then skips the marker and simply re-runs after a crash.
//
// If fn returns an error, nothing is recorded but the marker: a side-effecting step whose fn
// failed halts on the next attempt too, since a failed call may still have taken effect.
//
// A step that is not retry-safe must not pause: if fn returns a pause (Interrupt, Sleep, Await, a
// pending approval, a halt from a sub-agent it drives), Step returns an ErrConfig error instead,
// and the marker stays, so the step halts on the next attempt, since fn may have done something
// before it paused. Put the pause in a retry-safe step of its own, before or after the side effect.
//
// T must be JSON-serializable: the result is marshaled into the journal, so a struct with
// unexported fields round-trips those fields to their zero values (encoding/json skips them)
// with no error reported. Return exported fields, a map, or a pointer whose fields are exported.
//
// Inside the body of a plan flow node of the same run, the step is recorded under the node's key
// ("node:<node>:step:<name>", or "node:iter:<n>:<node>:step:<name>" in a loop body), so a loop
// body's step runs once per iteration, and name need be unique only within the node. Its halt names
// that key (OutcomeUnknown.Op.ID); resolve it with the halt's Ref.
//
// name must not be empty, and must not start with a prefix the engine reserves for its own journal keys ("@", "run:",
// "tool:", "attempt:", "approval:", "signal:", and the rest; see IsReservedStepName): such a
// name is ErrConfig.
func Step[T any](ctx context.Context, d *Journal, runID, name string, fn func(context.Context) (T, error), opts ...StepOption) (T, error) {
	if err := checkStepName("Step", name); err != nil {
		var zero T
		return zero, err
	}
	return step(ctx, d, runID, planScopedStep(ctx, runID, name), fn, opts...)
}

// step is Step without the check on name, for the engine's own steps.
func step[T any](ctx context.Context, d *Journal, runID, name string, fn func(context.Context) (T, error), opts ...StepOption) (T, error) {
	var out T
	if err := ctx.Err(); err != nil {
		return out, err // a cancelled caller starts no new step
	}
	if err := checkJournal(d); err != nil {
		return out, err
	}
	var cfg stepConfig
	if err := applyOptions("Step", &cfg, opts, StepOption.applyStep); err != nil {
		return out, err
	}
	// body runs fn and journals its value. A step that is not retry-safe must not pause (see Step).
	body := func(ctx context.Context) (Record, error) {
		v, err := fn(stepOnceScope(ctx, runID, name)) // the step numbers its own NextOnceKey keys
		if err != nil {
			if !cfg.safety.RetrySafe() && IsPause(err) {
				return Record{}, &stepPauseError{name: name, pause: err.Error()}
			}
			return Record{}, err
		}
		b, err := marshalJournal(v) // the journal's one encoding: no HTML escapes
		if err != nil {
			return Record{}, err
		}
		return Record{Kind: StepValue, Result: b}, nil
	}
	var rec Record
	var err error
	j := d
	rec, err = journalStep(ctx, j, runID, name, cfg, body)
	if err != nil {
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

// protocol:claims begin Open Lost Join LoserRead LoserWait Win Call NotStarted

// journalStep runs a step through j: a recorded step costs one Get. A side-effect step claims its
// attempt with one Insert and records its value with another; a retry-safe step reads the marker
// an earlier attempt may have left (the attempt's recorded safety) and records its value.
func journalStep(ctx context.Context, j *Journal, runID, name string, cfg stepConfig, body func(context.Context) (Record, error)) (Record, error) {
	if rec, ok, err := j.Get(ctx, runID, name); err != nil || ok {
		return rec, err
	}
	base := stepAttemptStep(name)
	if cfg.safety.RetrySafe() {
		// A retry-safe step writes no marker, but an earlier attempt of it may have, if it was
		// declared a side effect then. The marker is the attempt's recorded safety, so the step
		// halts as it would have, rather than run a side effect a second time, unless that
		// attempt is recorded as never started. The step was not recorded when read above, so it
		// runs without another read; a caller that recorded it meanwhile wins the Insert.
		return j.doFresh(ctx, runID, name, func(ctx context.Context) (Record, error) {
			if m, ok, err := j.liveAttempt(ctx, runID, base); err != nil {
				return Record{}, err
			} else if ok {
				return Record{}, stepHalt(runID, name, markerTime(m.AttemptedAt), HaltCrashed)
			}
			if err := cfg.checkCancelled(ctx); err != nil {
				return Record{}, err
			}
			return body(ctx)
		})
	}
	// The attempt marker is an exclusive claim, as for a tool call: a driver that did not write
	// it (a resume after a crash, or a second driver of the same run) must not run fn. An earlier
	// attempt recorded as not started does not count (see attempt.go): the claim is then for the
	// next attempt.
	won, marker, key, err := j.claimNext(ctx, runID, base,
		Record{Kind: StepAttempt, ToolUseID: name, AttemptedAt: time.Now().UnixMilli()})
	if err != nil {
		return Record{}, err
	}
	if !won {
		// Another driver owns the attempt. Its value, if it is recorded (or being recorded in this
		// process) by now, is the step's; otherwise the outcome is unknown. This driver only
		// joins a call in flight and never starts one: the owner, in this process, must not find
		// a loser's call in flight and take the loser's halt as its own outcome.
		//
		// The halt's cause is what this driver saw of the owner. An owner whose call it found in
		// flight was live after the claim: HaltContended, since that driver owns the effect and
		// may still be running it. Otherwise nothing says the owner is live (it may have died, or
		// be in another process, or not have called yet): HaltCrashed, which ResolveHaltRef
		// resolves only after its own live-driver check.
		at := markerTime(marker.AttemptedAt)
		if b, ok, err := joinFlight(flightKey{j.id, runID, name}); ok {
			if err != nil {
				return Record{}, stepHalt(runID, name, at, HaltContended)
			}
			return decodeStored(runID, name, b)
		}
		if rec, ok, err := j.Get(ctx, runID, name); err != nil || ok {
			return rec, err
		}
		return Record{}, stepHalt(runID, name, at, HaltCrashed)
	}
	var started atomic.Bool // fn was called: from here on its effect may have fired
	rec, err := j.doFresh(ctx, runID, name, func(ctx context.Context) (Record, error) {
		if err := ctx.Err(); err != nil {
			return Record{}, err // cancelled after the claim: fn is not called, and that is recorded below
		}
		if err := cfg.checkCancelled(ctx); err != nil {
			return Record{}, err // the run was cancelled: fn is not called, and that is recorded below
		}
		started.Store(true)
		return body(ctx)
	})
	if err != nil && !started.Load() {
		// This driver claimed the attempt and never called fn (it was cancelled, or the store
		// failed, first): record that, so the next attempt runs fn instead of halting.
		if nerr := j.notStarted(ctx, runID, key, marker); nerr != nil {
			err = fmt.Errorf("%w (%w)", err, nerr)
		}
	}
	return rec, err
}

// protocol:claims end

// protocol:claims begin Call

// stepPauseError is Step's refusal of a pause from a step that is not retry-safe (see Step). It
// wraps ErrConfig and not the pause, so no caller takes it for one. A tool call that returns it
// records nothing, as for a pause: the step's marker then halts the call's next attempt, where a
// recorded failure would let the model call again under a new id and fire the step's effect again.
type stepPauseError struct{ name, pause string }

func (e *stepPauseError) Error() string {
	return fmt.Sprintf("agent: step %q paused (%s) but is not retry-safe, so its attempt marker stays and it halts on the next attempt; put the pause in a retry-safe step of its own (WithSafety): %v", e.name, e.pause, ErrConfig)
}

func (e *stepPauseError) Unwrap() error { return ErrConfig }

// protocol:claims end

type stepConfig struct {
	safety Safety
	// cancelled, when set (a plan flow's node: journalhook.Step), is the run's cancellation check,
	// made once the step will run: after a side-effect step's claim is won and before fn (the
	// attempt is then recorded as not started), and before a retry-safe step's fn.
	cancelled func(context.Context) error
}

// stepCancelCheck is the internal StepOption that sets stepConfig.cancelled.
type stepCancelCheck func(context.Context) error

func (f stepCancelCheck) applyStep(c *stepConfig) error { c.cancelled = f; return nil }

// checkCancelled is cfg's cancellation check, nil when it has none.
func (cfg stepConfig) checkCancelled(ctx context.Context) error {
	if cfg.cancelled == nil {
		return nil
	}
	return cfg.cancelled(ctx)
}
