package agent

import (
	"context"
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

// WithNotifyStops makes NotifyPending also read ctl's control run and notify of each stop that was set or
// lifted within the last WithNotifyStopsWithin (default 7 days). Without it a sweep raises no stop events.
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

// stopEvents are the events a state of the control run owes: a stop_set for each stop within the window, and a
// stop_lifted for each of them that is lifted. The state is read as a process judges it, so a record it
// rejects raises nothing.
func (e *notifyEngine) stopEvents(st StopState) []notifyEvent {
	within := e.cfg.stopWithin
	if within <= 0 {
		within = defaultStopNotifyWithin
	}
	now := e.cfg.now()
	var out []notifyEvent
	for _, s := range st.Stops {
		at, err := time.Parse(time.RFC3339Nano, s.At)
		if err == nil && now.Sub(at) > within {
			continue
		}
		info := &NotifyStop{ID: s.ID, Scope: s.Scope, Reason: s.Reason, Compensation: s.Compensation, By: s.By, Host: s.Host, Mode: st.Mode, Signed: s.Signed}
		out = append(out, notifyEvent{key: StopSetKey(s.ID), kind: NotificationStopSet, runID: StopControlRun, eventAt: at, stop: info})
		if s.Lifted {
			lifted := *info
			lifted.LiftedBy = append([]string(nil), s.LiftedBy...)
			out = append(out, notifyEvent{key: StopLiftedKey(s.ID), kind: NotificationStopLifted, runID: StopControlRun, eventAt: now, stop: &lifted})
		}
	}
	return out
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
	e.queueScanTrail(StopControlRun) // a stop delivered earlier may have been lifted
	for _, ev := range e.stopEvents(st) {
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

// Observe queues the events st owes and returns at once. It is the function to give WithStopObserver.
func (n *StopNotifier) Observe(st StopState) {
	for _, ev := range n.e.stopEvents(st) {
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
