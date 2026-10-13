package agent

import (
	"context"
	"errors"
)

// eventFromErr maps the error a drive of runID ended with to the event it raises, if it raises one.
func (e *notifyEngine) eventFromErr(runID string, err error) (notifyEvent, bool) {
	if err == nil {
		return notifyEvent{}, false
	}
	if h, ok := errors.AsType[*OutcomeUnknown](err); ok {
		var ms int64
		if !h.AttemptedAt.IsZero() {
			ms = h.AttemptedAt.UnixMilli()
		}
		return notifyEvent{
			key: HaltedKey(h.RunID, h.Op.Kind, h.Op.ID, ms), kind: NotificationOutcomeUnknown, runID: rootOr(runID, h.RunRef), opRun: h.RunID,
			eventAt: h.AttemptedAt, halt: &NotifyHalt{Op: h.Op, Cause: h.Cause, AttemptedAt: h.AttemptedAt}, err: err,
		}, true
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
		return notifyEvent{key: AbortedKey(runID), kind: NotificationRunAborted, runID: runID, err: err}, true
	}
	return notifyEvent{}, false
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
				eventAt: st.FailingSince.At, reason: st.FailingSince.Reason, episode: st.Episodes, err: cause})
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
// (driven), whether it called the Resumer (resumed), and the error.
func (e *notifyEngine) afterRecover(ctx context.Context, runID string, driven, resumed bool, err error) {
	if ev, ok := e.eventFromErr(runID, err); ok {
		e.w.submit(ev)
		return // a halt or a gate is the run waiting for a person, not recovery failing
	}
	switch {
	case err != nil && (!driven || !IsPause(err) && !errors.Is(err, ErrLeaseLost) && !cancelledEnd(err)):
		e.recoveryFailed(ctx, runID, err)
	case err == nil && resumed:
		e.recoveryOK(ctx, runID)
	}
}
