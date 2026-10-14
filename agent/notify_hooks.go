package agent

import (
	"context"
	"errors"
	"fmt"
)

// eventFromErr maps the error a drive of runID ended with to the event it raises, if it raises one.
func (e *notifyEngine) eventFromErr(runID string, err error) (notifyEvent, bool) {
	if err == nil {
		return notifyEvent{}, false
	}
	if h, ok := errors.AsType[*OutcomeUnknown](err); ok {
		ev := notifyEvent{
			key: HaltedKey(h.RunID, h.Op.Kind, h.Op.ID, -1), kind: NotificationOutcomeUnknown, runID: rootOr(runID, h.RunRef), opRun: h.RunID,
			eventAt: h.AttemptedAt, halt: &NotifyHalt{Op: h.Op, Cause: h.Cause, AttemptedAt: h.AttemptedAt}, err: err,
		}
		if h.AttemptedAt.IsZero() && e.j != nil {
			// An untimed marker is told apart by its generation, which takes a read of the journal:
			// the worker does it, so a drive never waits on the store.
			ev.keyFn = func(ctx context.Context) (string, error) {
				attempt, aerr := e.haltAttempt(ctx, h)
				return HaltedKey(h.RunID, h.Op.Kind, h.Op.ID, attempt), aerr
			}
		} else if !h.AttemptedAt.IsZero() {
			ev.key = HaltedKey(h.RunID, h.Op.Kind, h.Op.ID, h.AttemptedAt.UnixMilli())
		}
		return ev, true
	}
	if p, ok := errors.AsType[*ApprovalPending](err); ok {
		ap := &NotifyApproval{ToolUseID: p.ToolUseID, ToolName: p.ToolName}
		if q := p.Quorum; q != nil {
			ap.Need, ap.Of, ap.Approved = q.Need, len(q.Approvers), q.Approved
			ap.Approvers = append([]string(nil), q.Approvers...)
		}
		return notifyEvent{
			key: ApprovalKey(p.RunID, p.ToolUseID, 0), kind: NotificationApprovalPending, runID: rootOr(runID, p.RunRef), opRun: p.RunID,
			approval: ap, err: err,
		}, true
	}
	if sa, ok := errors.AsType[*SagaAborted](err); ok && sa.CompensateErr == nil && len(sa.UnknownOutcome) == 0 && !errors.Is(err, ErrRunCancelled) {
		return notifyEvent{key: AbortedKey(runID), kind: NotificationRunAborted, runID: runID, reason: "saga_aborted"}, true
	}
	return notifyEvent{}, false
}

// haltAttempt is what tells one halt of an operation from a later one: the live attempt marker's
// time in Unix milliseconds, and for a marker that carries none (one an earlier version wrote), the
// negative of one more than its generation, so the first attempt of an untimed marker, and each
// numbered re-attempt after a not-started record, are different events. A marker whose generation
// cannot be read counts as -1.
func (e *notifyEngine) haltAttempt(ctx context.Context, h *OutcomeUnknown) (int64, error) {
	if !h.AttemptedAt.IsZero() {
		return h.AttemptedAt.UnixMilli(), nil
	}
	base := toolAttemptStep(h.Op.ID)
	if h.Op.Kind == OpStep {
		base = stepAttemptStep(h.Op.ID)
	}
	gen, err := e.liveGeneration(ctx, h.RunID, base)
	if err != nil {
		return 0, err
	}
	return -int64(gen) - 1, nil
}

// liveGeneration is the generation of the live attempt of the effect whose first marker key is
// base: 0 for the first attempt, n for the n-th re-attempt (see journal.liveAttempt).
func (e *notifyEngine) liveGeneration(ctx context.Context, runID, base string) (int, error) {
	for gen := 0; ; gen++ {
		key := retryAttemptStep(base, gen)
		marker, ok, err := e.j.Get(ctx, runID, key)
		if err != nil {
			return 0, err
		}
		if !ok {
			return max(gen-1, 0), nil
		}
		if v, err := e.j.voided(ctx, runID, key, marker); err != nil {
			return 0, err
		} else if !v {
			return gen, nil
		}
	}
}

// rootOr returns the root run of ref, else the run the drive was of.
func rootOr(run string, ref RunRef) string {
	if ref.RootRunID != "" {
		return ref.RootRunID
	}
	return run
}

// principalOf reads the run's principal from its run:start, nil if it has none or cannot be read.
func (e *notifyEngine) principalOf(ctx context.Context, runID string) *Principal {
	if start, ok, err := RecordedStart(ctx, e.j, runID); err == nil && ok {
		return start.Principal
	}
	return nil
}

// driveEnd is called when a drive of runID ends with err. It queues the event err raises, and
// returns at once.
func (e *notifyEngine) driveEnd(runID string, err error) {
	if IsSubRun(runID) {
		return // the root run's drive carries the same error up and raises it
	}
	if ev, ok := e.eventFromErr(runID, err); ok {
		e.w.submit(ev)
	}
	e.queueScan(runID) // the drive may have resolved, decided or ended an event already delivered
}

// causeClass is the closed-set cause of a recovery failure.
func causeClass(err error) string {
	switch {
	case errors.Is(err, ErrStorage):
		return "store_error"
	case errors.Is(err, ErrNotResumable):
		return "not_resumable"
	default:
		return "resume_error"
	}
}

// causeSentinel is the error a recovery_failing notification carries for a cause class: a sentinel
// or nothing, never the Resumer's own error, whose text can embed a run's arguments or a person's
// data.
func causeSentinel(class string) error {
	switch class {
	case "store_error":
		return ErrStorage
	case "not_resumable":
		return ErrNotResumable
	}
	return nil
}

// recoveryFailed records that a recovery pass failed for runID, opening a failure episode if none
// is open, and queues the recovery_failing event once the episode has lasted long enough.
func (e *notifyEngine) recoveryFailed(ctx context.Context, runID string, cause error) {
	name := NotifyTrailName(runID)
	for range maxTrailRetries {
		recs, err := e.trail.Load(ctx, name)
		if err != nil {
			e.report(err)
			return
		}
		st := FoldNotify(recs)
		now := e.cfg.now()
		if st.FailingSince == nil {
			rec := NotifyRecord{Kind: NotifyFailingSince, RunID: runID, At: now, Reason: causeClass(cause)}
			conflict, err := e.appendRec(ctx, name, len(recs), rec)
			if err != nil {
				e.report(err)
				return
			}
			if conflict {
				continue
			}
			return // a fresh episode is not due
		}
		if st.RecoveryFailingDue(now, e.cfg.recoveryAfter) {
			e.w.submit(notifyEvent{key: RecoveryKey(runID, st.Episodes), kind: NotificationRecoveryFailing, runID: runID,
				eventAt: st.FailingSince.At, reason: st.FailingSince.Reason, episode: st.Episodes, err: causeSentinel(st.FailingSince.Reason)})
		}
		return
	}
}

// recoveryOK records that a recovery pass succeeded for runID, closing its failure episode if one
// is open.
func (e *notifyEngine) recoveryOK(ctx context.Context, runID string) {
	name := NotifyTrailName(runID)
	for range maxTrailRetries {
		recs, err := e.trail.Load(ctx, name)
		if err != nil {
			e.report(err)
			return
		}
		if FoldNotify(recs).FailingSince == nil {
			return
		}
		rec := NotifyRecord{Kind: NotifyRecovered, RunID: runID, At: e.cfg.now()}
		conflict, err := e.appendRec(ctx, name, len(recs), rec)
		if err != nil {
			e.report(err)
			return
		}
		if !conflict {
			return
		}
	}
}

// afterRecover is called by a recovery pass with the outcome of one run: whether it drove the run
// (driven), whether it called the Resumer (resumed), and the error (a Resumer that does not drive
// the run, ErrNotResumable, is passed as the failure it is).
func (e *notifyEngine) afterRecover(ctx context.Context, runID string, driven, resumed bool, err error) {
	defer e.queueScan(runID) // after recoveryOK and recoveryFailed: an episode may just have closed
	if ev, ok := e.eventFromErr(runID, err); ok {
		e.w.submit(ev)
		e.recoveryOK(ctx, runID) // a halt or a gate is the run waiting for a person: recovery works
		return
	}
	switch {
	case err != nil && IsPause(err):
		e.recoveryOK(ctx, runID) // a pause is a success
	case err != nil && (!driven || !errors.Is(err, ErrLeaseLost) && !cancelledEnd(err)):
		if ctx.Err() == nil && !errors.Is(err, context.Canceled) {
			e.recoveryFailed(ctx, runID, err) // not a shutdown's cancellation
		}
	case err == nil && resumed:
		e.recoveryOK(ctx, runID)
	}
}

// RunNotifier tells a notifier when a run driven outside an Agent ends needing a person. A plan
// flow uses one (see plan.Builder.WithNotifier); code that drives its own runs can too. It is the
// WithNotifier option bound to a journal, and safe for concurrent use.
type RunNotifier struct {
	e *notifyEngine
}

// NewRunNotifier binds o, a WithNotifier option, to j, the journal the runs it will be told about
// are in. A nil o, or a nil j, is ErrConfig.
func NewRunNotifier(j *Journal, o NotifierOption) (*RunNotifier, error) {
	no, ok := o.(notifierOption)
	if !ok || j == nil {
		return nil, fmt.Errorf("NewRunNotifier: a nil journal or an option that WithNotifier did not return: %w", ErrConfig)
	}
	e, err := no.engine(j, "flow")
	if err != nil {
		return nil, err
	}
	return &RunNotifier{e: e}, nil
}

// DriveEnd tells the notifier that a drive of runID ended with err: it queues the event err raises
// (a halt, a pending approval, an aborted saga), if any, and returns at once. A drive that ended
// with another error or none raises nothing.
func (r *RunNotifier) DriveEnd(runID string, err error) { r.e.driveEnd(runID, err) }

// Flush waits for the deliveries in flight to finish their round and stops the worker, as
// Agent.FlushNotifications does.
func (r *RunNotifier) Flush(ctx context.Context) error { return r.e.flush(ctx) }
