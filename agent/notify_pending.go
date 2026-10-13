package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
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
	e, err := newNotifyEngine(j, n, trail, opts)
	if err != nil {
		return err
	}
	var errs []error
	for runID, err := range lister.Runs(ctx, recoverFilter) {
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
		recs, err := j.History(ctx, runID)
		if err != nil {
			errs = append(errs, fmt.Errorf("notification sweep: read run %s: %w", runID, err))
			continue
		}
		for _, ev := range e.pendingEvents(runID, recs) {
			if _, err := e.round(ctx, ev); err != nil {
				errs = append(errs, fmt.Errorf("notification sweep: run %s: %w", runID, err))
			}
		}
	}
	if err := e.flush(context.WithoutCancel(ctx)); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// pendingEvents returns the events journaled in recs, the history of the unfinished run runID, that
// still need a person. A sub-agent's run (its ID holds '>') is attributed to its root run.
func (e *notifyEngine) pendingEvents(runID string, recs []Record) []notifyEvent {
	root := treeRootID(runID)
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
		if err := json.Unmarshal(r.Result, &w); err != nil || w.ToolUseID == "" || have[ToolResultStep(w.ToolUseID)] {
			continue
		}
		ap := &NotifyApproval{ToolUseID: w.ToolUseID, ToolName: w.ToolName}
		if !w.Single {
			ap.Need, ap.Of, ap.Approvers = w.Need, len(w.Approvers), append([]string(nil), w.Approvers...)
		}
		out = append(out, notifyEvent{key: ApprovalKey(runID, w.ToolUseID, 0), kind: NotificationApprovalPending, runID: root, opRun: runID, approval: ap})
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
