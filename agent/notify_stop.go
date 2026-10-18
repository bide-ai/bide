package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// This file is the kill switch's side of the notifier: a stop that is set, and a stop that is lifted, are
// events a person must hear of. Both are observed from the control run (never raised by a drive), so
// delivery has the notification trail's claim, acknowledgement, retry, overdue and escalation, and survives
// the death of the process that set the stop. Two paths observe the control run: NotifyPending, given
// WithNotifyStops, reads it on each sweep (the durable path), and a StopSwitch's observer
// (WithStopObserver, StopNotifier.Observe) sees a change within the switch's poll interval. Both name an event
// by its key, so the trail's claim keeps any number of processes, and both paths, to one notification.

const (
	// NotificationStopSet: a stop was set. It is current while the stop is in force; when the stop is lifted
	// the usual all-clear (NotificationResolved, reason "lifted") ends it. Notification.Stop describes the stop.
	NotificationStopSet NotificationKind = "stop_set"
	// NotificationStopLifted: a stop was lifted, by the lift rule in force (an audited stop needs its quorum,
	// so one partial lift raises nothing). It is a fact and never stops being current.
	NotificationStopLifted NotificationKind = "stop_lifted"
)

// NotifyStop is the structured detail of a NotificationStopSet or NotificationStopLifted notification. It
// holds no key, signature or secret. Reason is the operator's text (at most MaxStopReason bytes, cleaned
// of control and bidirectional characters when the stop was recorded).
type NotifyStop struct {
	ID           string
	Scope        StopScope
	Reason       string
	Compensation string // CompensationAllow or CompensationBlock
	// By is the setter, and Host the host the setter named: asserted in basic mode, authenticated by the
	// signature in audited mode (Signed). Mode is the mode the stop was recorded under.
	By, Host string
	Mode     StopMode
	Signed   bool
	// LiftedBy are the lifters whose records met the lift rule, for a NotificationStopLifted.
	LiftedBy []string
}

// StopSetKey is the event key of the stop with the given ID being set (NotificationStopSet).
func StopSetKey(id string) string { return "stop:" + id }

// StopLiftedKey is the event key of the stop with the given ID being lifted (NotificationStopLifted).
func StopLiftedKey(id string) string { return "stop:" + id + ":lifted" }

// stopIDOfKey is the stop a stop event key (StopSetKey, StopLiftedKey; a receiver prefix allowed) names.
func stopIDOfKey(key string) (id string, lifted, ok bool) {
	rest, found := strings.CutPrefix(stripTo(key), "stop:")
	if !found {
		return "", false, false
	}
	if base, l := strings.CutSuffix(rest, ":lifted"); l {
		rest, lifted = base, true
	}
	if rest == "" || strings.Contains(rest, ":") {
		return "", false, false
	}
	return rest, lifted, true
}

// defaultStopNotifyWithin is how far back a sweep looks for stops to notify of: an older stop is history,
// not news, and a notifier started for the first time must not announce a year of stops.
const defaultStopNotifyWithin = 7 * 24 * time.Hour

// WithNotifyStops makes NotifyPending also read ctl's control run and notify of each stop that was set, and
// each that was lifted, within the last WithNotifyStopsWithin (default 7 days; the set and the lift are judged
// by their own times). Without it a sweep raises no stop events, and it leaves the stop_set events on the trail
// alone: it cannot tell whether they still stand (it reports that once, to WithNotifyErrors).
func WithNotifyStops(ctl *StopControl) NotifyOption {
	return notifyOption(func(c *notifyConfig) error {
		if ctl == nil {
			return fmt.Errorf("WithNotifyStops: nil control: %w", ErrConfig)
		}
		c.stops = ctl
		return nil
	})
}

// WithNotifyStopsWithin sets how far back stop events are raised (see WithNotifyStops); at least one minute.
func WithNotifyStopsWithin(d time.Duration) NotifyOption {
	return notifyOption(func(c *notifyConfig) error {
		if d < time.Minute {
			return fmt.Errorf("WithNotifyStopsWithin(%v): at least one minute: %w", d, ErrConfig)
		}
		c.stopWithin = d
		return nil
	})
}

// ownedStop is one stop of a control run state and which of its two events are owed.
type ownedStop struct {
	entry         StopEntry
	set, lifted   bool // the stop_set and the stop_lifted are owed
	setAt, liftAt time.Time
}

// stopClockSkew is how far past now a record's time may be before it is read as unknown: a time the writer
// chose and that is in the future is no evidence of when the record was written.
const stopClockSkew = 5 * time.Minute

// stopsOwed says, for each stop of st, which events a notifier owes at now with a window of within (0 is the
// default). A record's time (StopRecord.At) is the writer's own, so no event is made to depend on it alone:
//
//   - every stop in force is owed its stop_set, whatever its time says (a backdated or future-dated stop is
//     still a stop nobody may be left uninformed of);
//   - a lifted stop is owed its stop_lifted while the lift is recent, and also when its stop_set is on the
//     trail (observed): a stop a notifier told of is owed its end, however its lift is dated. A stop lifted
//     long ago and never told of is history and owes nothing, and a stop that is lifted and was never
//     observed owes no stop_set (nothing was current to tell) unless its set is recent;
//   - a time in the future past stopClockSkew is read as unknown (the zero time), so it neither hides an event
//     as "not yet due" nor dates it.
//
// observed says whether a key is on the control run's notification trail (nil: none is). The notifier and
// bide audit notifications judge by this one function.
func stopsOwed(st StopState, now time.Time, within time.Duration, observed func(string) bool) []ownedStop {
	if within <= 0 {
		within = defaultStopNotifyWithin
	}
	if observed == nil {
		observed = func(string) bool { return false }
	}
	parse := func(stamp string) (time.Time, bool) {
		t, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil || t.After(now.Add(stopClockSkew)) {
			return time.Time{}, false
		}
		return t, true
	}
	recent := func(stamp string) bool { // a time that does not parse, or lies in the future, is not old
		t, ok := parse(stamp)
		return !ok || now.Sub(t) <= within
	}
	var out []ownedStop
	for _, s := range st.Stops {
		told := observed(StopSetKey(s.ID))
		o := ownedStop{entry: s}
		o.setAt, _ = parse(s.At)
		o.liftAt, _ = parse(s.LiftedAt)
		o.set = !s.Lifted || recent(s.At) || told
		o.lifted = s.Lifted && (recent(cmp.Or(s.LiftedAt, s.At)) || told)
		if o.set || o.lifted {
			out = append(out, o)
		}
	}
	return out
}

// StopNotifyEvents are the events a state of the control run owes a notifier at now, with a window of within
// (0: the notifier's default, 7 days), as the journal-side facts audit.VerifyNotifications compares with the
// trail of the control run: a stop_set for each stop in force (and each lifted one that is owed, see stopsOwed;
// Current while the stop stands, with the time it was lifted as ResolvedAt otherwise), and a stop_lifted for
// each lift owed. observed says whether a key is on that trail. It is what the notifier itself raises
// (WithNotifyStops).
func StopNotifyEvents(st StopState, now time.Time, within time.Duration, observed func(key string) bool) []JournalNotifyEvent {
	var out []JournalNotifyEvent
	for _, o := range stopsOwed(st, now, within, observed) {
		if o.set {
			out = append(out, JournalNotifyEvent{Key: StopSetKey(o.entry.ID), Kind: NotificationStopSet, RunID: StopControlRun, At: o.setAt, Current: !o.entry.Lifted, ResolvedAt: o.liftAt})
		}
		if o.lifted {
			out = append(out, JournalNotifyEvent{Key: StopLiftedKey(o.entry.ID), Kind: NotificationStopLifted, RunID: StopControlRun, At: o.liftAt, Current: true})
		}
	}
	return out
}

// StopClockAnomalies are the records of a control run, in store order, whose time is not believable: dated
// more than skew after now, or more than skew before an earlier record of the log. A record's time is its
// writer's own, so an anomaly is a finding, not an answer to when it was written.
func StopClockAnomalies(recs []StopRecord, now time.Time, skew time.Duration) []string {
	var out []string
	var latest time.Time
	for i, r := range recs {
		t, err := time.Parse(time.RFC3339Nano, r.At)
		if err != nil {
			continue
		}
		switch {
		case t.After(now.Add(skew)):
			out = append(out, fmt.Sprintf("record %d (%s by %s) is dated %s, in the future", i+1, r.Kind, r.By, r.At))
		case !latest.IsZero() && t.Before(latest.Add(-skew)):
			out = append(out, fmt.Sprintf("record %d (%s by %s) is dated %s, more than %s before an earlier record of the control run", i+1, r.Kind, r.By, r.At, skew))
		}
		if t.After(latest) && !t.After(now.Add(skew)) {
			latest = t
		}
	}
	return out
}

// ParseStopKey is the stop and the kind an event key names: StopSetKey is a stop_set, StopLiftedKey a
// stop_lifted. ok is false for any other key.
func ParseStopKey(key string) (id string, kind NotificationKind, ok bool) {
	id, lifted, ok := stopIDOfKey(key)
	switch {
	case !ok:
		return "", "", false
	case lifted:
		return id, NotificationStopLifted, true
	}
	return id, NotificationStopSet, true
}

// stopEvents are the events a state of the control run owes: a stop_set for each stop whose set is within
// the window, and a stop_lifted for each lift within it. The state is read as a process judges it, so a record
// it rejects raises nothing.
func (e *notifyEngine) stopEvents(st StopState, observed func(string) bool) []notifyEvent {
	now := e.cfg.now()
	var out []notifyEvent
	for _, o := range stopsOwed(st, now, e.cfg.stopWithin, observed) {
		s := o.entry
		info := &NotifyStop{ID: s.ID, Scope: s.Scope, Reason: s.Reason, Compensation: s.Compensation, By: s.By, Host: s.Host, Mode: st.Mode, Signed: s.Signed}
		if o.set {
			out = append(out, notifyEvent{key: StopSetKey(s.ID), kind: NotificationStopSet, runID: StopControlRun, eventAt: o.setAt, stop: info})
		}
		if o.lifted {
			lifted := *info
			lifted.LiftedBy = append([]string(nil), s.LiftedBy...)
			out = append(out, notifyEvent{key: StopLiftedKey(s.ID), kind: NotificationStopLifted, runID: StopControlRun, eventAt: now, stop: &lifted})
		}
	}
	return out
}

// stopsOff reports, once, that this process saw a stop event it cannot judge because it was not given the
// control (WithNotifyStops, NewStopNotifier): its stop notifications are off, and a process that has the
// control handles them.
func (e *notifyEngine) stopsOff() {
	e.stopsOffOnce.Do(func() {
		e.report(errors.New("notify: the trail holds stop events and this process was not given the stop control (WithNotifyStops): it leaves them to a process that was"))
	})
}

// stopStillSet reports whether the stop id is in force in the control run now.
func (e *notifyEngine) stopStillSet(ctx context.Context, id string) (bool, error) {
	if e.cfg.stops == nil {
		return true, nil // nothing to judge by: it stands
	}
	st, err := e.cfg.stops.State(ctx)
	if err != nil {
		return false, err
	}
	for _, s := range st.Stops {
		if s.ID == id {
			return !s.Lifted, nil
		}
	}
	return false, nil // not accepted (rejected, or not in this view): it does not stand
}

// observeTimeout bounds the read of the control run's trail that Observe makes (it runs on the observer's goroutine,
// off every check point).
const observeTimeout = 10 * time.Second

// observedStops says whether a key is on the notification trail of the control run: what stopsOwed asks to know
// which lifted stops were told of. A trail that cannot be read counts as holding nothing (and is reported).
func (e *notifyEngine) observedStops(ctx context.Context) func(string) bool {
	recs, err := e.trail.Load(ctx, NotifyTrailName(StopControlRun))
	if err != nil {
		e.report(fmt.Errorf("notify: read the stop trail: %w", err))
		return nil
	}
	st := FoldNotify(recs)
	return func(key string) bool { return st.Events[key] != nil }
}

// sweepStops is the part of NotifyPending that reads the control run.
func (e *notifyEngine) sweepStops(ctx context.Context) []error {
	if e.cfg.stops == nil {
		return nil
	}
	var errs []error
	st, err := e.cfg.stops.State(ctx)
	if err != nil {
		return append(errs, fmt.Errorf("notification sweep: read the stop control run: %w", err))
	}
	// No scan is queued here: the sweep's run listing reaches the control run (an unfinished run) and queues
	// its trail's scan, and each stop event below is judged again by its round, which writes the all-clear
	// of a stop that has been lifted.
	for _, ev := range e.stopEvents(st, e.observedStops(ctx)) {
		if err := e.roundRouted(ctx, ev); err != nil {
			errs = append(errs, fmt.Errorf("notification sweep: stop %s: %w", ev.stop.ID, err))
		}
	}
	return errs
}

// StopNotifier tells a notifier when a stop is set or lifted, from a StopSwitch's observations of the control
// run. It is the WithNotifier option bound to a journal and a StopControl, and safe for concurrent use.
//
//	n, _ := agent.NewStopNotifier(journal, agent.WithNotifier(webhook, trail), ctl)
//	sw, _ := agent.NewStopSwitch(ctl, agent.WithStopObserver(n.Observe))
//
// Observe never blocks and never fails the switch: it hands the events to the notifier's own worker. A
// notifier that blocks or panics costs the notifier its delivery, which NotifyPending (WithNotifyStops) then
// finds again, and nothing else. Several processes may each have one: the trail's claim on an event's key
// keeps it to one notification.
type StopNotifier struct {
	e *notifyEngine
}

// NewStopNotifier binds o, a WithNotifier option, to j and ctl. A nil j, ctl or o is ErrConfig.
func NewStopNotifier(j *Journal, o NotifierOption, ctl *StopControl) (*StopNotifier, error) {
	no, ok := o.(notifierOption)
	if !ok || j == nil || ctl == nil {
		return nil, fmt.Errorf("NewStopNotifier: a nil journal or control, or an option that WithNotifier did not return: %w", ErrConfig)
	}
	e, err := no.engine(j, "stop")
	if err != nil {
		return nil, err
	}
	e.cfg.stops = ctl
	return &StopNotifier{e: e}, nil
}

// Observe queues the events st owes. It is the function to give WithStopObserver, which calls it off every check
// point, on a goroutine of the switch's own (it reads the control run's trail, bounded by observeTimeout).
func (n *StopNotifier) Observe(st StopState) {
	ctx, cancel := context.WithTimeout(context.Background(), observeTimeout)
	defer cancel()
	for _, ev := range n.e.stopEvents(st, n.e.observedStops(ctx)) {
		n.e.submit(ev)
	}
	n.e.queueScanTrail(StopControlRun)
}

// Flush waits for the deliveries in flight to finish their round and stops the worker, as
// Agent.FlushNotifications does.
func (n *StopNotifier) Flush(ctx context.Context) error { return n.e.flush(ctx) }

// stopObserver hands a StopSwitch's observations of the control run to fn, off the switch's path: a
// single slot holding the latest state, and one goroutine that drains it. A state that arrives while fn is
// running replaces the one waiting, so a slow or blocked fn never grows a queue and never delays a read; each
// state is a whole picture of the control run, so nothing is lost by dropping an older one. A panic of fn ends
// that call only.
type stopObserver struct {
	fn func(StopState)

	mu      sync.Mutex
	next    *StopState
	running bool
	last    string // the signature of the last state handed over
}

// signature names what the observer cares about: which stops exist and which are lifted.
func stopSignature(st StopState) string {
	var b strings.Builder
	for _, s := range st.Stops {
		b.WriteString(s.ID)
		if s.Lifted {
			b.WriteByte('!')
		}
		b.WriteByte(' ')
	}
	return b.String()
}

// observe is called by the switch after a successful read. It does nothing when no stop appeared or was
// lifted since the last call.
func (o *stopObserver) observe(st StopState) {
	sig := stopSignature(st)
	o.mu.Lock()
	if sig == o.last {
		o.mu.Unlock()
		return
	}
	o.last = sig
	o.next = &st
	if o.running {
		o.mu.Unlock()
		return
	}
	o.running = true
	o.mu.Unlock()
	go o.drain()
}

func (o *stopObserver) drain() {
	for {
		o.mu.Lock()
		st := o.next
		o.next = nil
		if st == nil {
			o.running = false
			o.mu.Unlock()
			return
		}
		o.mu.Unlock()
		o.call(*st)
	}
}

func (o *stopObserver) call(st StopState) {
	defer func() { _ = recover() }() // an observer that panics fails its own call, never the switch
	o.fn(st)
}

// WithStopObserver gives a StopSwitch a function that is told, with the control run's state, each time a stop
// has appeared or been lifted since it last was (the first successful read included). It runs on a goroutine of
// the switch's, never on a check point's path, and never delays or fails one: the state is handed over through a
// slot that keeps only the latest, so a function that blocks costs only its own later calls, and a panic ends
// only its call. The function may be called from one goroutine at a time. StopNotifier.Observe is the one the
// notifier gives.
func WithStopObserver(fn func(StopState)) StopSwitchOption {
	return switchOption(func(s *StopSwitch) error {
		if fn == nil {
			return fmt.Errorf("WithStopObserver: nil function: %w", ErrConfig)
		}
		s.observer = &stopObserver{fn: fn}
		return nil
	})
}
