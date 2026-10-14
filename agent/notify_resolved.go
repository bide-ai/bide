package agent

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// This file is the all-clear of the notifier (NotificationResolved): when an event a receiver has,
// or may have, been told of stops being current, the receiver is told so, with the same trail,
// claim, acknowledgement and retry as any event. See docs/design/notifier.md, "The all-clear".

// resolvable reports whether an event of kind can stop being current, and so be cleared:
// run_aborted is a fact and never does; a resolved or overdue notice is not an incident.
func resolvable(kind string) bool {
	switch NotificationKind(kind) {
	case NotificationOutcomeUnknown, NotificationApprovalPending, NotificationRecoveryFailing:
		return true
	}
	return false
}

// eventFromKey rebuilds from a trail record of an event the part of the event that "is it still
// current" reads: the run and the operation or gate a key names. ok is false for a key it cannot
// read, which includes an ID so long that the key holds its digest (see encodeID).
func eventFromKey(es *NotifyEvent) (notifyEvent, bool) {
	ev := notifyEvent{key: es.Key, kind: NotificationKind(es.Kind), runID: es.RunID}
	parts := strings.Split(es.Key, ":")
	dec := func(i int) (string, bool) {
		if i >= len(parts) {
			return "", false
		}
		return decodeID(parts[i])
	}
	switch ev.kind {
	case NotificationOutcomeUnknown: // halted:<run>:<op kind>:<op id>:<attempt>
		run, ok1 := dec(1)
		kind, ok2 := dec(2)
		op, ok3 := dec(3)
		if len(parts) != 5 || parts[0] != "halted" || !ok1 || !ok2 || !ok3 {
			return notifyEvent{}, false
		}
		ev.opRun, ev.halt = run, &NotifyHalt{Op: OpRef{Kind: OpKind(kind), ID: op}}
	case NotificationApprovalPending: // approval:<run>:<call>:<reminder>
		run, ok1 := dec(1)
		call, ok2 := dec(2)
		if len(parts) != 4 || parts[0] != "approval" || !ok1 || !ok2 {
			return notifyEvent{}, false
		}
		ev.opRun, ev.approval = run, &NotifyApproval{ToolUseID: call}
	case NotificationRecoveryFailing: // recovery:<run>:<episode>
		episode, err := strconv.Atoi(parts[len(parts)-1])
		if len(parts) != 3 || parts[0] != "recovery" || err != nil {
			return notifyEvent{}, false
		}
		ev.episode = episode
	default:
		return notifyEvent{}, false
	}
	return ev, true
}

// scanRound runs the catch-up scan of ev.runID's trail as a round of the worker.
func (e *notifyEngine) scanRound(ctx context.Context, ev notifyEvent) (roundResult, error) {
	if err := e.clearScan(ctx, ev.runID); err != nil {
		return roundResult{next: e.cfg.now().Add(e.backoff(1))}, err
	}
	return roundResult{done: true}, nil
}

// queueScan queues the scan of runID's trail. It is called where a run's state may have changed (a
// drive ended, a recovery pass ran, a sweep reached the run) and returns at once.
func (e *notifyEngine) queueScan(runID string) {
	if IsSubRun(runID) {
		return // the root run's trail holds its events
	}
	e.w.submit(notifyEvent{key: scanKey(runID), scan: true, runID: runID})
}

// clearScan reads runID's trail and queues, for each event in it, what it owes a receiver now:
//
//   - an acknowledged event that is no longer current: its all-clear;
//   - an event cleared after a delivery was attempted (a claim is the trace of one: a call can
//     land although its reply was lost): its all-clear;
//   - an event still open after a delivery was attempted that is no longer current: it is cleared
//     (nothing is sent for it) and then its all-clear is owed;
//   - an all-clear observed and not yet acknowledged: it is delivered again (the process that
//     observed it may be gone).
//
// An event never claimed was never sent, so there is nothing to clear. It only reads the journal
// and the trail, so any number of processes may run it; the trail's append race gives each
// all-clear one observed record and, with its claim, one deliverer at a time.
func (e *notifyEngine) clearScan(ctx context.Context, runID string) error {
	recs, err := e.trail.Load(ctx, NotifyTrailName(runID))
	if err != nil {
		return err
	}
	st := FoldNotify(recs)
	var errs []error
	for _, key := range st.Keys {
		es := st.Events[key]
		if NotificationKind(es.Kind) == NotificationResolved {
			if es.Open() {
				e.resumeResolved(es, &st)
			}
			continue
		}
		if !resolvable(es.Kind) || st.Events[ResolvedKey(key)] != nil {
			continue
		}
		switch {
		case es.Cleared != nil:
			if es.Rounds > 0 {
				e.w.submit(clearOf(es, es.Cleared.Reason, es.Cleared.At))
			}
		case es.Rounds > 0 || es.Acked != nil:
			ev, ok := eventFromKey(es)
			if !ok {
				e.report(fmt.Errorf("notify: %s cannot be checked for an all-clear: its key holds the digest of an ID, not the ID", key))
				continue
			}
			cur, reason, cerr := e.current(ctx, ev, st)
			if cerr != nil {
				errs = append(errs, cerr)
				continue
			}
			if cur {
				continue
			}
			if es.Acked != nil {
				e.w.submit(clearOf(es, reason, e.cfg.now()))
			} else if _, rerr := e.round(ctx, ev); rerr != nil { // writes cleared, and queues the all-clear
				errs = append(errs, rerr)
			}
		}
	}
	return errors.Join(errs...)
}

// clearOf is the all-clear event for es, an event that stopped being current for reason, as of at.
func clearOf(es *NotifyEvent, reason string, at time.Time) notifyEvent {
	return notifyEvent{key: ResolvedKey(es.Key), kind: NotificationResolved, runID: es.RunID,
		origKey: es.Key, origKind: NotificationKind(es.Kind), reason: reason, eventAt: at}
}

// resumeResolved queues again the all-clear that es, an observed and unacknowledged resolved event
// in st, records, with the reason its record holds.
func (e *notifyEngine) resumeResolved(es *NotifyEvent, st *NotifyState) {
	origKey := strings.TrimPrefix(es.Key, "resolved:")
	orig := st.Events[origKey]
	if orig == nil {
		e.report(fmt.Errorf("notify: %s clears %s, which the trail does not hold", es.Key, origKey))
		return
	}
	e.w.submit(clearOf(orig, es.Reason, es.EventAt))
}
