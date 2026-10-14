package agent

import "time"

// NotifyEvent is the state of one event, folded from a notification trail.
type NotifyEvent struct {
	Key                 string
	Kind                string // the NotificationKind recorded at observation
	RunID               string
	EventAt             time.Time
	ObservedAt          time.Time
	DeadlineAt          time.Time
	OpRun, OpKind, OpID string // recorded at observation for a halt or an approval (see NotifyRecord)
	Reason              string // recorded at observation: for a resolved event, why the event it clears ended

	Claim   *NotifyRecord // the latest claimed record, nil if never claimed
	Rounds  int           // how many claimed records the event has
	Acked   *NotifyRecord
	Cleared *NotifyRecord
	Failing *NotifyRecord
	Overdue *NotifyRecord
}

// Open reports whether delivery of the event is still owed: it has been neither acknowledged nor
// cleared.
func (e *NotifyEvent) Open() bool { return e.Acked == nil && e.Cleared == nil }

// ClaimLive reports whether some process holds a live claim at now. A claim lapses at its Until.
func (e *NotifyEvent) ClaimLive(now time.Time) bool {
	return e.Open() && e.Claim != nil && now.Before(e.Claim.Until)
}

// OverdueDue reports whether the event is open, past its deadline at now, and not yet marked
// overdue. An event observed with no deadline (a resolved event, which is never overdue) is not.
func (e *NotifyEvent) OverdueDue(now time.Time) bool {
	return e.Open() && e.Overdue == nil && !e.DeadlineAt.IsZero() && now.After(e.DeadlineAt)
}

// NotifyState is a notification trail folded to what a notifier needs to decide its next step.
type NotifyState struct {
	Events map[string]*NotifyEvent
	Keys   []string // event keys in the order they were observed

	// FailingSince is the open recovery failure episode of the run, nil if none.
	FailingSince *NotifyRecord
	// Episodes counts closed recovery failure episodes (recovered records that closed one).
	Episodes int
}

// FoldNotify folds a trail, in append order, to its state. It is a pure function of recs, and
// tolerant of a trail it did not write: a record that does not apply is ignored (a claim for an
// event never observed, a second observation of a key, any record after an event is acknowledged
// or cleared, a recovered with no open episode). The first terminal record (acked or cleared)
// ends an event, and the later one is ignored. Verification of a trail is audit's job, not the
// fold's.
func FoldNotify(recs []NotifyRecord) NotifyState {
	st := NotifyState{Events: map[string]*NotifyEvent{}}
	for i := range recs {
		r := recs[i]
		switch r.Kind {
		case NotifyObserved:
			if _, dup := st.Events[r.Key]; dup {
				continue
			}
			st.Events[r.Key] = &NotifyEvent{Key: r.Key, Kind: r.EventKind, RunID: r.RunID, EventAt: r.EventAt, ObservedAt: r.At, DeadlineAt: r.DeadlineAt, Reason: r.Reason,
				OpRun: r.OpRun, OpKind: r.OpKind, OpID: r.OpID}
			st.Keys = append(st.Keys, r.Key)
		case NotifyClaimed:
			if e := st.openEvent(r.Key); e != nil {
				e.Claim = &r
				e.Rounds++
			}
		case NotifyAcked:
			if e := st.openEvent(r.Key); e != nil {
				e.Acked = &r
			}
		case NotifyCleared:
			if e := st.openEvent(r.Key); e != nil {
				e.Cleared = &r
			}
		case NotifyFailing:
			if e := st.openEvent(r.Key); e != nil && e.Failing == nil {
				e.Failing = &r
			}
		case NotifyOverdue:
			if e := st.openEvent(r.Key); e != nil && e.Overdue == nil {
				e.Overdue = &r
			}
		case NotifyFailingSince:
			if st.FailingSince == nil {
				st.FailingSince = &r
			}
		case NotifyRecovered:
			if st.FailingSince != nil {
				st.FailingSince = nil
				st.Episodes++
			}
		}
	}
	return st
}

// openEvent returns the event with key if it exists and is still open, else nil.
func (s *NotifyState) openEvent(key string) *NotifyEvent {
	if e := s.Events[key]; e != nil && e.Open() {
		return e
	}
	return nil
}

// RecoveryFailingDue reports whether the open recovery failure episode has lasted longer than d at
// now, so a recovery_failing event is due. It is false with no open episode.
func (s *NotifyState) RecoveryFailingDue(now time.Time, d time.Duration) bool {
	return s.FailingSince != nil && now.Sub(s.FailingSince.At) > d
}

// Watched is an event on the open trail whose all-clear may still be owed.
type Watched struct {
	Key   string    `json:"key"`
	RunID string    `json:"run_id"`
	At    time.Time `json:"at"` // when it was listed
}

// OpenState is one open-trail segment folded.
type OpenState struct {
	Watched    []Watched // the events listed, in the order they were
	Backfilled bool      // the events observed before the open trail existed were listed
	Sealed     string    // the segment that continues this one, "" if it is not sealed
}

// FoldOpen folds an open-trail segment, in append order. A checkpoint is read only as the first
// record; a watch for a key already listed is ignored; an unwatch removes the key, and a later watch
// lists it again; nothing after a seal counts; a record of another kind is ignored.
func FoldOpen(recs []NotifyRecord) OpenState {
	var st OpenState
	var order []string
	open := map[string]Watched{}
	for i, r := range recs {
		if st.Sealed != "" {
			break
		}
		switch r.Kind {
		case NotifyCheckpoint:
			if i != 0 {
				continue
			}
			for _, w := range r.Watching {
				if _, ok := open[w.Key]; !ok {
					order = append(order, w.Key)
				}
				open[w.Key] = w
			}
			st.Backfilled = r.Backfilled
		case NotifyBackfilled:
			st.Backfilled = true
		case NotifyWatch:
			if _, ok := open[r.Key]; !ok {
				open[r.Key] = Watched{Key: r.Key, RunID: r.RunID, At: r.At}
				order = append(order, r.Key)
			}
		case NotifyUnwatch:
			delete(open, r.Key)
		case NotifySeal:
			st.Sealed = r.Next
		}
	}
	for _, k := range order {
		if w, ok := open[k]; ok {
			st.Watched = append(st.Watched, w)
			delete(open, k) // a key listed, unlisted and listed again appears once
		}
	}
	return st
}

// FoldWatch is the events FoldOpen lists.
func FoldWatch(recs []NotifyRecord) []Watched { return FoldOpen(recs).Watched }

// LastSegment is the segment named by the last NotifySegment record of an open-trail index, "" if
// there is none.
func LastSegment(recs []NotifyRecord) string {
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Kind == NotifySegment {
			return recs[i].Next
		}
	}
	return ""
}
