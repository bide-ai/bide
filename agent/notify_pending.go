package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// NotifyPending runs one catch-up sweep for a notifier: it reads every unfinished run in j and
// delivers, through n and trail as WithNotifier would, each event that needs a person and has not
// been delivered: a run waiting at an approval gate, and a run halted on an effect whose outcome is
// unknown. Call it at startup and on a timer, in an application that drives runs with Run alone
// and so has no RecoverLoop to find an event whose process died before it was recorded. With
// RecoverLoop or the sweep, an event is delivered at least once; with neither, only by the drive
// that saw it.
//
// It reads each unfinished run's journal, so it costs a History per run: run it every few minutes,
// not every second. It needs a store that implements Lister. It reports, in the error it returns, any
// delivery that failed (those events are retried by the next sweep) and any run it could not read;
// it does not stop at the first.
//
// When the trail signs and anchors its records (audit.NewAuditedEventStore beneath audit.NewNotifyTrail),
// the sweep also signs again the trail of each run it looked at that grew past its last signed head,
// within the anchor's timeout, so an outage of the anchor leaves an unattested stretch of trail only
// until the next sweep; a failure is reported to the handler of WithNotifyErrors and is not the
// sweep's error.
//
// What it can see is narrower than a drive. A gate is found from the run's approval-pending record.
// A halt is found from an attempt marker that no not-started record voids and that has no result
// after WithNotifyHaltAge (default 5 minutes): it cannot tell a halted effect from one a process it
// cannot see is still running, so a call that runs longer than that is reported as halted, and it
// reports the cause as unknown. A run that was aborted, and recovery_failing, are raised by drives
// and by Recover and RecoverLoop, not by the sweep. Events are keyed as the drive keys them, so an
// event both see is delivered once.
func NotifyPending(ctx context.Context, j *Journal, n Notifier, trail NotifyTrail, opts ...NotifyOption) error {
	if j == nil {
		return fmt.Errorf("NotifyPending: nil journal: %w", ErrConfig)
	}
	if isNil(n) || isNil(trail) {
		return fmt.Errorf("NotifyPending: nil Notifier or NotifyTrail: %w", ErrConfig)
	}
	lister, ok := Capability[Lister](j.store)
	if !ok {
		return fmt.Errorf("NotifyPending needs a store that implements Lister (itself or through Unwrap) to enumerate runs: %w", ErrConfig)
	}
	e, err := newNotifyEngine(j, n, trail, opts, "sweep")
	if err != nil {
		return err
	}
	var errs []error
	seen := map[string]bool{} // the trails of the runs the sweep looked at
	var trails []string
	for runID, err := range lister.Runs(ctx, sweepFilter) {
		if err != nil {
			errs = append(errs, fmt.Errorf("list runs for the notification sweep: %w (%w)", err, ErrStorage))
			break
		}
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		if IsSessionRun(runID) {
			continue // the session drives it
		}
		e.queueScan(runID) // an event delivered earlier may have stopped being current
		if name := NotifyTrailName(treeRootID(runID)); !seen[name] {
			seen[name] = true
			trails = append(trails, name)
		}
		if !IsSubRun(runID) {
			if abortEntry, aborted, err := j.getEntry(ctx, runID, runAbortedStep); err != nil {
				errs = append(errs, fmt.Errorf("notification sweep: read run %s: %w", runID, err))
				continue
			} else if aborted {
				// An aborted run is over, so no recovery pass lists it: the sweep is the only catch-up
				// for an abort whose drive died before it recorded the event. Only a recent one.
				at, known, terr := e.abortTime(ctx, j, runID, abortEntry)
				if terr != nil {
					errs = append(errs, fmt.Errorf("notification sweep: read run %s: %w", runID, terr))
					continue
				}
				if !known || e.cfg.now().Sub(at) > e.cfg.abortedWithin {
					continue
				}
				ev := notifyEvent{key: AbortedKey(runID), kind: NotificationRunAborted, runID: runID, reason: "saga_aborted"}
				if err := e.roundRouted(ctx, ev); err != nil {
					errs = append(errs, fmt.Errorf("notification sweep: run %s: %w", runID, err))
				}
				continue
			}
		}
		recs, err := j.History(ctx, runID)
		if err != nil {
			errs = append(errs, fmt.Errorf("notification sweep: read run %s: %w", runID, err))
			continue
		}
		e.queueScanTrail(runStartRoot(runID, recs)) // a flow run with a mark in its ID is its own root
		for _, ev := range e.pendingEvents(runID, recs) {
			if err := e.roundRouted(ctx, ev); err != nil {
				errs = append(errs, fmt.Errorf("notification sweep: run %s: %w", runID, err))
			}
		}
	}
	if err := e.backfillOpen(ctx); err != nil { // events observed before the open trail existed
		errs = append(errs, fmt.Errorf("notification sweep: list earlier events: %w", err))
	}
	e.queueScanOpen() // the all-clear of a run that is no longer unfinished
	if err := e.flushDeliveries(context.WithoutCancel(ctx)); err != nil {
		errs = append(errs, err)
	}
	// Last, after the deliveries have grown the trails: sign again any trail that grew past its last
	// head, which an anchor outage or a process that died before anchoring left behind. Its failure
	// goes to WithNotifyErrors, not to the sweep's result: it is not a delivery.
	// The trails that belong to no run are signed too: the digest's and the open trail's segments and
	// index (the list of events whose all-clear may be owed).
	if _, ok := e.trail.(NotifyTrailAnchorer); ok {
		trails = append(trails, e.globalTrails(ctx)...)
	}
	e.anchorTrails(ctx, trails...)
	return errors.Join(errs...)
}

// abortTime is when the aborted run runID aborted, for the sweep's window: the run:aborted record's
// aborted_at when it has one. For a run an earlier version aborted it is the newest time the journal
// records for the run (an attempt marker's, or the run's start), which is no later than the abort
// and so a lower bound on how recent it was; known is false when the journal records none.
func (e *notifyEngine) abortTime(ctx context.Context, j *Journal, runID string, abortEntry Entry) (at time.Time, known bool, err error) {
	rec, err := decodeStored(runID, runAbortedStep, abortEntry.Data)
	if err != nil {
		return time.Time{}, false, err
	}
	if t, ok := abortedAt(rec); ok {
		return t, true, nil
	}
	recs, err := j.History(ctx, runID)
	if err != nil {
		return time.Time{}, false, err
	}
	at, known = newestTime(recs)
	return at, known, nil
}

// newestTime is the newest wall-clock time recs record: an attempt marker's, or the run's start.
func newestTime(recs []Record) (time.Time, bool) {
	var newest time.Time
	for _, r := range recs {
		var t time.Time
		switch {
		case r.Kind == StepAttempt:
			t = markerTime(r.AttemptedAt)
		case r.Kind == StepValue && r.Name == runStartStep:
			var s RunStart
			if json.Unmarshal(r.Result, &s) == nil {
				t, _ = s.Started()
			}
		}
		if t.After(newest) {
			newest = t
		}
	}
	return newest, !newest.IsZero()
}

// sweepFilter lists the runs a sweep reads: every run that is not completed or cancelled, so the
// unfinished ones and the aborted ones.
var sweepFilter = RunFilter{ExcludeHolding: []string{runCompleteStep, runCancelledStep}}

// runStartRoot is the root run whose trail holds the events of runID, from its history: a plan
// flow's run (its run:start records RunKindFlow) is its own root, whatever its ID holds, as the
// flow files its events under; any other run is attributed to the root its ID names (treeRootID),
// so a sub-agent's run is its root run's.
func runStartRoot(runID string, recs []Record) string {
	for _, r := range recs {
		if r.Kind == StepValue && r.Name == runStartStep {
			var s RunStart
			if json.Unmarshal(r.Result, &s) == nil && s.kind() == RunKindFlow {
				return runID
			}
			break
		}
	}
	return treeRootID(runID)
}

// pendingEvents returns the events journaled in recs, the history of the unfinished run runID, that
// still need a person. A sub-agent's run (its ID holds '>') is attributed to its root run.
func (e *notifyEngine) pendingEvents(runID string, recs []Record) []notifyEvent {
	root := runStartRoot(runID, recs)
	have := make(map[string]bool, len(recs))
	for _, r := range recs {
		have[r.Name] = true
	}
	now := e.cfg.now()
	var out []notifyEvent

	for _, r := range recs {
		if r.Kind != StepValue || !strings.HasPrefix(r.Name, "approval-pending:") {
			continue
		}
		var w ApprovalWait
		if err := json.Unmarshal(r.Result, &w); err != nil || w.ToolUseID == "" {
			continue
		}
		ap := &NotifyApproval{ToolUseID: w.ToolUseID, ToolName: w.ToolName}
		// The first pause note, or a re-ask note (ApprovalReaskStep): each is its own ask, decided by
		// its own outcome.
		key, decided := ApprovalKey(runID, w.ToolUseID, 0), gateDecidedNames(w.ToolUseID)
		if w.Reask != "" {
			sod := w.Reask == PendingSoDReapproval
			if r.Name != ApprovalReaskStep(w.ToolUseID, sod) {
				continue
			}
			key, decided = ApprovalReaskKey(runID, w.ToolUseID, sod), reaskDecidedNames(w.ToolUseID, sod)
			ap.Reask = w.Reask
		}
		if slices.ContainsFunc(decided, func(n string) bool { return have[n] }) {
			continue
		}
		if !w.Single {
			ap.Need, ap.Of, ap.Approvers = w.Need, len(w.Approvers), append([]string(nil), w.Approvers...)
			if w.ExcludePrincipal {
				for _, a := range w.Approvers {
					if slices.ContainsFunc(w.Requesters, func(r string) bool { return r != "" && SameApprover(a, r) }) {
						ap.Excluded = append(ap.Excluded, a)
					}
				}
			}
		}
		out = append(out, notifyEvent{key: key, kind: NotificationApprovalPending, runID: root, opRun: runID, approval: ap})
	}

	live := liveAttempts(recs)
	for _, base := range slices.Sorted(maps.Keys(live)) {
		m := live[base]
		var op OpRef
		var result string
		switch {
		case strings.HasPrefix(base, "attempt:tool:"):
			op = OpRef{Kind: OpTool, ID: m.ToolUseID}
			result = ToolResultStep(m.ToolUseID)
			if _, call, ok := FindToolCall(recs, m.ToolUseID); ok {
				op.ToolName = call.Name
			}
		case strings.HasPrefix(base, "attempt:step:"):
			op = OpRef{Kind: OpStep, ID: strings.TrimPrefix(base, "attempt:step:")}
			result = op.ID
		default:
			continue
		}
		at := markerTime(m.AttemptedAt)
		if have[result] || at.IsZero() || now.Sub(at) < e.cfg.haltAge {
			continue
		}
		out = append(out, notifyEvent{
			key: HaltedKey(runID, op.Kind, op.ID, at.UnixMilli()), kind: NotificationOutcomeUnknown, runID: root, opRun: runID,
			eventAt: at, halt: &NotifyHalt{Op: op, AttemptedAt: at},
		})
	}
	return out
}
