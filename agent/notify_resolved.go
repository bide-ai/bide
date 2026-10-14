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

// operation names the operation or gate an event is about, for its observed record: the run whose
// journal holds it, the kind ("tool" or "step" for a halt, "gate" for an approval) and its ID.
func (ev notifyEvent) operation() (run, kind, id string) {
	run = ev.opRun
	if run == "" {
		run = ev.runID
	}
	switch {
	case ev.halt != nil:
		return run, string(ev.halt.Op.Kind), ev.halt.Op.ID
	case ev.approval != nil:
		return run, "gate", ev.approval.ToolUseID
	}
	return "", "", ""
}

// eventFromKey rebuilds from a trail record of an event the part of the event that "is it still
// current" reads: the run and the operation or gate. It reads the operation fields of the observed
// record, and for a record from before them the key, which holds the encoded IDs. ok is false for
// such a record whose key holds the digest of an ID (see encodeID).
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
		if es.OpID != "" && es.OpKind != "gate" {
			ev.opRun, ev.halt = es.OpRun, &NotifyHalt{Op: OpRef{Kind: OpKind(es.OpKind), ID: es.OpID}}
			return ev, true
		}
		run, ok1 := dec(1)
		kind, ok2 := dec(2)
		op, ok3 := dec(3)
		if len(parts) != 5 || parts[0] != "halted" || !ok1 || !ok2 || !ok3 {
			return notifyEvent{}, false
		}
		ev.opRun, ev.halt = run, &NotifyHalt{Op: OpRef{Kind: OpKind(kind), ID: op}}
	case NotificationApprovalPending: // approval:<run>:<call>:<reminder>
		if es.OpID != "" && es.OpKind == "gate" {
			ev.opRun, ev.approval = es.OpRun, &NotifyApproval{ToolUseID: es.OpID}
			return ev, true
		}
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
	var err error
	switch ev.runID {
	case "":
		err = e.scanOpen(ctx)
	case "@backfill":
		err = e.backfillOpen(ctx)
	default:
		err = e.clearScan(ctx, ev.runID)
	}
	if err != nil {
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
	return e.clearScanFold(ctx, FoldNotify(recs))
}

// clearScanFold is clearScan over a trail already loaded and folded. It also unlists, from the
// NotifyOpenTrail, each event for which nothing more is owed.
func (e *notifyEngine) clearScanFold(ctx context.Context, st NotifyState) error {
	var errs []error
	for _, key := range st.Keys {
		es := st.Events[key]
		if NotificationKind(es.Kind) == NotificationResolved {
			if es.Open() {
				e.resumeResolved(es, &st)
			} else if es.Acked != nil {
				if err := e.unwatch(ctx, strings.TrimPrefix(key, "resolved:")); err != nil {
					errs = append(errs, err)
				}
			}
			continue
		}
		if !resolvable(es.Kind) {
			continue
		}
		if es.Cleared != nil && es.Rounds == 0 { // nobody was told: nothing is owed
			if err := e.unwatch(ctx, key); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if st.Events[ResolvedKey(key)] != nil {
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

// orphanWatchAfter is how long an event may be listed without an observed record before a scan
// unlists it: the process that listed it died before it observed the event. If the event is
// observed later, the round lists it again.
const orphanWatchAfter = time.Hour

// watch lists key, of an event on runID's trail, on the NotifyOpenTrail unless it is listed.
func (e *notifyEngine) watch(ctx context.Context, key, runID string) error {
	return e.setWatch(ctx, key, runID, true)
}

// unwatch unlists key unless it is not listed.
func (e *notifyEngine) unwatch(ctx context.Context, key string) error {
	return e.setWatch(ctx, key, "", false)
}

func (e *notifyEngine) setWatch(ctx context.Context, key, runID string, want bool) error {
	for range maxTrailRetries {
		name, recs, err := e.openHead(ctx)
		if err != nil {
			return err
		}
		var run string
		listed := false
		for _, w := range FoldWatch(recs) {
			if w.Key == key {
				listed, run = true, w.RunID
			}
		}
		if listed == want {
			return nil
		}
		rec := NotifyRecord{Kind: NotifyWatch, Key: key, RunID: runID, At: e.cfg.now()}
		if !want {
			rec.Kind, rec.RunID = NotifyUnwatch, run
		}
		conflict, err := e.appendRec(ctx, name, len(recs), rec)
		if err != nil {
			return err
		}
		if !conflict {
			return nil
		}
	}
	return fmt.Errorf("notify: trail %s: lost the append race %d times in a row for %s", NotifyOpenTrail, maxTrailRetries, key)
}

// indexYearsBack is how many years back indexLast looks for an index when this year's has no record
// yet. It is walked only by the first lookup of a new year, so a gap of years costs that many loads
// once.
const indexYearsBack = 50

// indexLast returns the segment the newest open-trail index ends with, "" if there is none, and the
// year of that index. It reads this year's index (and the next year's in December, for a peer whose
// clock is ahead), then earlier years if this year's has no record.
func (e *notifyEngine) indexLast(ctx context.Context) (string, int, error) {
	now := e.cfg.now().UTC()
	y := now.Year()
	var years []int
	if now.Month() == time.December {
		years = append(years, y+1)
	}
	for i := 0; i <= indexYearsBack; i++ {
		years = append(years, y-i)
	}
	for _, yr := range years {
		recs, err := e.trail.Load(ctx, OpenIndex(yr))
		if err != nil {
			return "", 0, err
		}
		if seg := LastSegment(recs); seg != "" {
			return seg, yr, nil
		}
	}
	return "", y, nil
}

// indexSegment appends name to the index of this year unless the index already ends with it.
func (e *notifyEngine) indexSegment(ctx context.Context, name string) error {
	idx := OpenIndex(e.cfg.now().UTC().Year())
	for range maxTrailRetries {
		recs, err := e.trail.Load(ctx, idx)
		if err != nil {
			return err
		}
		if LastSegment(recs) == name {
			return nil
		}
		conflict, err := e.appendRec(ctx, idx, len(recs), NotifyRecord{Kind: NotifySegment, At: e.cfg.now(), Next: name})
		if err != nil {
			return err
		}
		if !conflict {
			return nil
		}
	}
	return fmt.Errorf("notify: trail %s: lost the append race indexing %s", idx, name)
}

// openHead returns the open-trail segment writes go to and its records. It reads the index for the
// newest segment, follows seals to the end of the chain, rolls a segment from an earlier day forward
// (a seal on it, then a checkpoint of its set as the first record of today's, then an index record),
// and repairs the index if a writer died before indexing the segment it created. That order is what
// makes a crash at any step recoverable: after the seal the chain names the next segment; a
// segment reached through a seal that is still empty gets its checkpoint; a segment reached through
// the chain but not in the index gets indexed; and the index names an empty segment only for the
// very first one, which has nothing to carry. A lookup is O(1) loads however long the gap.
func (e *notifyEngine) openHead(ctx context.Context) (string, []NotifyRecord, error) {
	today := OpenSegment(e.cfg.now())
	for range 4 * maxTrailRetries {
		last, _, err := e.indexLast(ctx)
		if err != nil {
			return "", nil, err
		}
		if last == "" { // the first segment: index it before it is created
			if err := e.indexSegment(ctx, today); err != nil {
				return "", nil, err
			}
			continue
		}
		name := last
		recs, err := e.trail.Load(ctx, name)
		if err != nil {
			return "", nil, err
		}
		var prev *OpenState // the sealed segment name continues, once the chain has been followed
		sealedNow := false
		for hops := 0; ; hops++ {
			if hops > 4*maxTrailRetries {
				return "", nil, fmt.Errorf("notify: trail %s: a chain of seals too long to follow", NotifyOpenTrail)
			}
			if len(recs) == 0 {
				cp := NotifyRecord{Kind: NotifyCheckpoint, At: e.cfg.now()}
				if prev != nil {
					cp.Watching, cp.Backfilled = prev.Watched, prev.Backfilled
				}
				if _, aerr := e.appendRec(ctx, name, 0, cp); aerr != nil {
					return "", nil, aerr
				}
				if recs, err = e.trail.Load(ctx, name); err != nil {
					return "", nil, err
				}
				continue
			}
			st := FoldOpen(recs)
			if st.Sealed == "" {
				break
			}
			prev = &st
			name = st.Sealed
			if recs, err = e.trail.Load(ctx, name); err != nil {
				return "", nil, err
			}
		}
		if name < today { // an earlier day: seal it, and the next pass follows the seal
			if _, err := e.sealSegment(ctx, name, recs, today); err != nil {
				return "", nil, err
			}
			sealedNow = true
		}
		if name != last { // reached through the chain: the index is behind
			if err := e.indexSegment(ctx, name); err != nil {
				return "", nil, err
			}
		}
		if sealedNow {
			continue
		}
		return name, recs, nil
	}
	return "", nil, fmt.Errorf("notify: trail %s: could not find or roll the open trail's head", NotifyOpenTrail)
}

// sealSegment appends a seal to segment name (records recs) naming next, and returns its records
// afterwards (reloaded if the append lost a race).
func (e *notifyEngine) sealSegment(ctx context.Context, name string, recs []NotifyRecord, next string) ([]NotifyRecord, error) {
	if _, err := e.appendRec(ctx, name, len(recs), NotifyRecord{Kind: NotifySeal, At: e.cfg.now(), Next: next}); err != nil {
		return nil, err
	}
	return e.trail.Load(ctx, name)
}

// queueBackfill queues the one-time listing of the events observed before the open trail existed.
func (e *notifyEngine) queueBackfill() {
	e.w.submit(notifyEvent{key: scanKey("@backfill"), scan: true, runID: "@backfill"})
}

// queueScanOpen queues the scan of the NotifyOpenTrail.
func (e *notifyEngine) queueScanOpen() {
	e.w.submit(notifyEvent{key: scanKey("@open"), scan: true})
}

// scanOpen scans the trail of every run that has an event on the NotifyOpenTrail: the way a sweep
// or a recovery pass reaches the all-clear of a run that is not unfinished (it completed while
// nobody scanned), without reading every finished run. An event listed for longer than
// orphanWatchAfter whose run's trail has no record of it is unlisted.
func (e *notifyEngine) scanOpen(ctx context.Context) error {
	_, recs, err := e.openHead(ctx)
	if err != nil {
		return err
	}
	byRun := map[string][]Watched{}
	var runs []string
	for _, w := range FoldWatch(recs) {
		if _, ok := byRun[w.RunID]; !ok {
			runs = append(runs, w.RunID)
		}
		byRun[w.RunID] = append(byRun[w.RunID], w)
	}
	var errs []error
	for _, run := range runs {
		trail, lerr := e.trail.Load(ctx, NotifyTrailName(run))
		if lerr != nil {
			errs = append(errs, lerr)
			continue
		}
		st := FoldNotify(trail)
		for _, w := range byRun[run] {
			if st.Events[w.Key] == nil && e.cfg.now().Sub(w.At) > orphanWatchAfter {
				if uerr := e.unwatch(ctx, w.Key); uerr != nil {
					errs = append(errs, uerr)
				}
			}
		}
		if cerr := e.clearScanFold(ctx, st); cerr != nil {
			errs = append(errs, cerr)
		}
	}
	return errors.Join(errs...)
}

// backfillOpen lists, once, the events observed before the open trail existed: every event in a
// run's trail that can still owe an all-clear. It reads each run's trail one time (the sweep's
// Lister enumerates every run, finished ones included), and records that it is done, so it is
// skipped from then on; one that is interrupted starts over, which is safe because listing is
// idempotent.
func (e *notifyEngine) backfillOpen(ctx context.Context) error {
	lister, ok := Capability[Lister](e.j.store)
	if !ok {
		return nil // a store that cannot list runs has none to list
	}
	_, recs, err := e.openHead(ctx)
	if err != nil {
		return err
	}
	if FoldOpen(recs).Backfilled {
		return nil
	}
	for runID, lerr := range lister.Runs(ctx, RunFilter{}) {
		if lerr != nil {
			return lerr
		}
		if IsSubRun(runID) || IsSessionRun(runID) {
			continue
		}
		trail, terr := e.trail.Load(ctx, NotifyTrailName(runID))
		if terr != nil {
			return terr
		}
		st := FoldNotify(trail)
		for _, key := range st.Keys {
			es := st.Events[key]
			if !resolvable(es.Kind) || es.Cleared != nil && es.Rounds == 0 {
				continue
			}
			if r := st.Events[ResolvedKey(key)]; r != nil && r.Acked != nil {
				continue
			}
			if werr := e.watch(ctx, key, es.RunID); werr != nil {
				return werr
			}
		}
	}
	for range maxTrailRetries {
		name, recs, herr := e.openHead(ctx)
		if herr != nil {
			return herr
		}
		if FoldOpen(recs).Backfilled {
			return nil
		}
		conflict, aerr := e.appendRec(ctx, name, len(recs), NotifyRecord{Kind: NotifyBackfilled, At: e.cfg.now()})
		if aerr != nil {
			return aerr
		}
		if !conflict {
			return nil
		}
	}
	return fmt.Errorf("notify: trail %s: lost the append race recording the backfill", NotifyOpenTrail)
}
