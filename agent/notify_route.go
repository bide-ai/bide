package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// This file is routing and escalation in the notifier: which receiver an event goes to, and who is
// told when it is not acknowledged (an approval: not decided) in time. See docs/design/notifier.md,
// "Routing and escalation".
//
// Both live in the engine, over the one trail of a run, and not in a Notifier that fans out, so that
// every receiver has its own delivery in the audited trail: its own observed, claimed and acked
// records, its own retries and its own overdue. A Notifier that sent to several sinks would have one
// acknowledgement for all of them, and a trail that cannot say which one a person was told through.

// maxEscalationSteps bounds an escalation chain.
const maxEscalationSteps = 4

var receiverName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// EscalationStep is one step of an escalation chain: the receiver to tell, and how long it has,
// from the moment the step is observed, before the next step is due.
type EscalationStep struct {
	// Receiver is the name of a receiver: DefaultReceiver or one given to WithNotifyReceiver.
	Receiver string
	// Deadline is the step's own deadline. It must be positive.
	Deadline time.Duration
}

// routable kinds: the events a run raises. A resolved, overdue or escalated event follows the
// receiver of the event it is about (an escalated event, its step's).
func routableKind(k NotificationKind) bool {
	switch k {
	case NotificationOutcomeUnknown, NotificationApprovalPending, NotificationRunAborted, NotificationRecoveryFailing:
		return true
	}
	return false
}

// WithNotifyReceiver names a receiver besides the one WithNotifier is given (DefaultReceiver), for
// WithNotifyRoute, WithNotifyApproverRoute and WithNotifyEscalation. A name is 1 to 32 lower-case
// letters, digits, '_' or '-', and each receiver is a Notifier of its own (typically a webhook
// notifier with its own URL, secret and relay). Every receiver has its own claim, acknowledgement,
// retries and overdue in the notification trail.
func WithNotifyReceiver(name string, n Notifier) NotifyOption {
	return notifyOption(func(c *notifyConfig) error {
		switch {
		case !receiverName.MatchString(name) || name == DefaultReceiver:
			return fmt.Errorf("WithNotifyReceiver(%q): a name is 1 to 32 of a-z, 0-9, '_' and '-', and %q is taken: %w", name, DefaultReceiver, ErrConfig)
		case isNil(n):
			return fmt.Errorf("WithNotifyReceiver(%q): nil Notifier: %w", name, ErrConfig)
		}
		if _, dup := c.receivers[name]; dup {
			return fmt.Errorf("WithNotifyReceiver(%q): a receiver of that name is already set: %w", name, ErrConfig)
		}
		if c.receivers == nil {
			c.receivers = map[string]Notifier{}
		}
		c.receivers[name] = n
		return nil
	})
}

// WithNotifyRoute sends events of kind to the named receiver instead of the default one. kind is
// one a run raises: NotificationOutcomeUnknown, NotificationApprovalPending,
// NotificationRunAborted or NotificationRecoveryFailing. The all-clear of an event goes to the
// receiver the event went to; an overdue notice too, unless WithOverdueNotifier says otherwise.
func WithNotifyRoute(kind NotificationKind, receiver string) NotifyOption {
	return notifyOption(func(c *notifyConfig) error {
		if !routableKind(kind) {
			return fmt.Errorf("WithNotifyRoute(%q): not a kind a run raises: %w", kind, ErrConfig)
		}
		if c.routes == nil {
			c.routes = map[NotificationKind]string{}
		}
		c.routes[kind] = receiver
		return nil
	})
}

// WithNotifyApproverRoute sends the approval_pending event of a gate whose approvers include
// approver to the named receiver. A gate with approvers routed to several receivers is delivered
// to each of them, as a delivery of its own (key "to:<receiver>:<key>", its own ID, claim and
// acknowledgement), so that one receiver's acknowledgement is never taken for another's. A gate
// with none of its approvers routed, and a 1-of-1 gate (which names none), follows
// WithNotifyRoute(NotificationApprovalPending, ...).
func WithNotifyApproverRoute(approver, receiver string) NotifyOption {
	return notifyOption(func(c *notifyConfig) error {
		if approver == "" {
			return fmt.Errorf("WithNotifyApproverRoute: empty approver: %w", ErrConfig)
		}
		if c.approverRoutes == nil {
			c.approverRoutes = map[string]string{}
		}
		c.approverRoutes[approver] = receiver
		return nil
	})
}

// WithNotifyEscalation sets the escalation chain of events of kind: NotificationOutcomeUnknown,
// NotificationApprovalPending or NotificationRecoveryFailing. When such an event has not been
// acknowledged by its deadline (WithNotifyDeadline), or, for an approval, has not been decided by
// then, the first step's receiver is told in a NotificationEscalated event of its own, with its
// own claim and acknowledgement in the trail and its own deadline, the step's. When that deadline
// passes with the event still unacknowledged (an approval: undecided) the next step is told, up to
// the last. An event that stops being current stops the chain, and the receivers already told get
// its all-clear. The original keeps being delivered. 1 to 4 steps.
//
// Escalation runs in a process that is alive at the deadline: a RecoverLoop, a long-lived Agent or
// a periodic NotifyPending finds the event again in the trail and continues the chain.
func WithNotifyEscalation(kind NotificationKind, steps ...EscalationStep) NotifyOption {
	return notifyOption(func(c *notifyConfig) error {
		switch kind {
		case NotificationOutcomeUnknown, NotificationApprovalPending, NotificationRecoveryFailing:
		default:
			return fmt.Errorf("WithNotifyEscalation(%q): only an outcome_unknown, approval_pending or recovery_failing event can stop being current, so only those can be escalated: %w", kind, ErrConfig)
		}
		if len(steps) < 1 || len(steps) > maxEscalationSteps {
			return fmt.Errorf("WithNotifyEscalation(%q): %d steps; need 1 to %d: %w", kind, len(steps), maxEscalationSteps, ErrConfig)
		}
		for i, s := range steps {
			if s.Deadline <= 0 {
				return fmt.Errorf("WithNotifyEscalation(%q): step %d has a non-positive deadline: %w", kind, i+1, ErrConfig)
			}
		}
		if _, dup := c.escalation[kind]; dup {
			return fmt.Errorf("WithNotifyEscalation(%q): set twice: %w", kind, ErrConfig)
		}
		if c.escalation == nil {
			c.escalation = map[NotificationKind][]EscalationStep{}
		}
		c.escalation[kind] = slices.Clone(steps)
		return nil
	})
}

// validateRouting checks that every receiver a route or a step names exists.
func (c *notifyConfig) validateRouting() error {
	known := func(r string) bool { _, ok := c.receivers[r]; return ok || r == DefaultReceiver }
	for k, r := range c.routes {
		if !known(r) {
			return fmt.Errorf("WithNotifyRoute(%q, %q): no such receiver: %w", k, r, ErrConfig)
		}
	}
	for a, r := range c.approverRoutes {
		if !known(r) {
			return fmt.Errorf("WithNotifyApproverRoute(%q, %q): no such receiver: %w", a, r, ErrConfig)
		}
	}
	for k, steps := range c.escalation {
		for i, s := range steps {
			if !known(s.Receiver) {
				return fmt.Errorf("WithNotifyEscalation(%q): step %d names %q: no such receiver: %w", k, i+1, s.Receiver, ErrConfig)
			}
		}
	}
	return nil
}

// receiverID is the receiver recorded in the trail and held by a notifyEvent: "" for the default.
func receiverID(name string) string {
	if name == DefaultReceiver {
		return ""
	}
	return name
}

// receiverLabel is the receiver's name in a Notification.
func receiverLabel(id string) string {
	if id == "" {
		return DefaultReceiver
	}
	return id
}

// ToKey is the event key of key's delivery to receiver, for an event that goes to several receivers
// at once (an approval whose approvers are routed to different ones): each has a key, and so an
// ID, claim and acknowledgement, of its own.
func ToKey(receiver, key string) string { return "to:" + receiver + ":" + key }

// EscalationKey is the event key of step level (from 1) of the escalation chain of the event with
// key.
func EscalationKey(key string, level int) string {
	return "escalated:" + strconv.Itoa(level) + ":" + key
}

// stripTo returns key without the "to:<receiver>:" prefix of a fan-out delivery.
func stripTo(key string) string {
	if rest, ok := strings.CutPrefix(key, "to:"); ok {
		if _, k, ok := strings.Cut(rest, ":"); ok {
			return k
		}
	}
	return key
}

// kindOfKey is the kind of the event a key (receiver prefix allowed) names, "" if it is not one a
// run raises and that can stop being current.
func kindOfKey(key string) NotificationKind {
	key = stripTo(key)
	switch {
	case strings.HasPrefix(key, "halted:"):
		return NotificationOutcomeUnknown
	case strings.HasPrefix(key, "approval:"):
		return NotificationApprovalPending
	case strings.HasPrefix(key, "recovery:"):
		return NotificationRecoveryFailing
	}
	return ""
}

// routed returns the deliveries of ev: one, to the receiver its kind names, or, for an approval
// whose approvers are routed to several receivers, one for each, under a key of its own.
func (e *notifyEngine) routed(ev notifyEvent) []notifyEvent {
	kindRecv := receiverID(DefaultReceiver)
	if r, ok := e.cfg.routes[ev.kind]; ok {
		kindRecv = receiverID(r)
	}
	if ev.kind == NotificationApprovalPending && ev.approval != nil && len(e.cfg.approverRoutes) > 0 {
		var recvs []string
		for _, a := range ev.approval.Approvers {
			if r, ok := e.cfg.approverRoutes[a]; ok && !slices.Contains(recvs, receiverID(r)) {
				recvs = append(recvs, receiverID(r))
			}
		}
		if len(recvs) > 0 {
			slices.Sort(recvs)
			if len(recvs) == 1 {
				ev.receiver = recvs[0]
				return []notifyEvent{ev}
			}
			out := make([]notifyEvent, 0, len(recvs))
			for _, r := range recvs {
				d := ev
				d.receiver, d.key = r, ToKey(receiverLabel(r), ev.key)
				out = append(out, d)
			}
			return out
		}
	}
	ev.receiver = kindRecv
	return []notifyEvent{ev}
}

// submit routes ev and queues each delivery, and the escalation watch of each one that has a chain.
func (e *notifyEngine) submit(ev notifyEvent) {
	for _, d := range e.routed(ev) {
		e.w.submit(d)
		if w, ok := e.watchFor(d); ok {
			e.w.submit(w)
		}
	}
}

// roundRouted runs a round for each delivery of ev, in this goroutine (the sweep's way), and queues
// the escalation watch of each. The errors are joined.
func (e *notifyEngine) roundRouted(ctx context.Context, ev notifyEvent) error {
	var errs []error
	for _, d := range e.routed(ev) {
		if _, err := e.round(ctx, d); err != nil {
			errs = append(errs, err)
		}
		if w, ok := e.watchFor(d); ok {
			e.w.submit(w)
		}
	}
	return errors.Join(errs...)
}

// notifierOf is the Notifier of the receiver ev is to.
func (e *notifyEngine) notifierOf(ev notifyEvent) Notifier {
	if ev.receiver != "" {
		if n, ok := e.cfg.receivers[ev.receiver]; ok {
			return n
		}
	}
	return e.n
}

// watchFor is the escalation watch of ev, if its kind has a chain.
func (e *notifyEngine) watchFor(ev notifyEvent) (notifyEvent, bool) {
	if len(e.cfg.escalation[ev.kind]) == 0 {
		return notifyEvent{}, false
	}
	return watchOf(ev), true
}

// watchOf is the worker job that watches ev's deadline and escalates it. It is a job, not an event:
// it writes nothing to the trail until a step is due.
func watchOf(ev notifyEvent) notifyEvent {
	b := ev
	return notifyEvent{key: watchKey(ev.key), escWatch: true, runID: ev.runID, base: &b}
}

// watchKey is the worker key of the escalation watch of the event with key.
func watchKey(key string) string { return "escwatch:" + key }

// escalationOf is the delivery of step level of ev's chain.
func (e *notifyEngine) escalationOf(base notifyEvent, level int) notifyEvent {
	step := e.cfg.escalation[base.kind][level-1]
	d := base
	d.key, d.kind, d.keyFn = EscalationKey(base.key, level), NotificationEscalated, nil
	d.origKey, d.origKind, d.level = base.key, base.kind, level
	d.receiver, d.deadline = receiverID(step.Receiver), step.Deadline
	d.reason = "deadline"
	if base.kind == NotificationApprovalPending {
		d.reason = "undecided"
	}
	d.escWatch, d.base, d.scan = false, nil, false
	return d
}

// escRound is one pass of the escalation watch of w.base: it brings the chain as far as the trail
// and the clock say it is due, and returns when to look again.
func (e *notifyEngine) escRound(ctx context.Context, w notifyEvent) (roundResult, error) {
	base := *w.base
	steps := e.cfg.escalation[base.kind]
	name := NotifyTrailName(base.runID)
	recs, err := e.trail.Load(ctx, name)
	if err != nil {
		return roundResult{next: e.cfg.now().Add(e.backoff(1))}, err
	}
	st := FoldNotify(recs)
	now := e.cfg.now()
	es := st.Events[base.key]
	if es == nil {
		return roundResult{next: now.Add(e.backoff(1))}, nil // not observed yet: its delivery round does that
	}
	if es.Cleared != nil {
		return roundResult{done: true}, nil
	}
	// The deepest step observed, and whether anyone has acknowledged: an unacknowledged event is
	// satisfied by an acknowledgement of itself or of a step (someone has it); an approval only by
	// its decision.
	level, acked := 0, es.Acked != nil
	for l := 1; l <= len(steps); l++ {
		if se := st.Events[EscalationKey(base.key, l)]; se != nil {
			level = l
			acked = acked || se.Acked != nil
		}
	}
	if acked && base.kind != NotificationApprovalPending {
		return roundResult{done: true}, nil
	}
	cur, _, err := e.current(ctx, base, st)
	if err != nil {
		return roundResult{next: now.Add(e.backoff(1))}, err
	}
	if !cur {
		return roundResult{done: true}, nil // the delivery rounds and the scan clear what is open
	}
	if level > 0 {
		if se := st.Events[EscalationKey(base.key, level)]; se.Open() {
			e.w.submit(e.escalationOf(base, level)) // a process that died after observing it: deliver it
		}
	}
	if level == len(steps) {
		return roundResult{done: true}, nil // the chain is at its end
	}
	due := es.DeadlineAt
	if level > 0 {
		due = st.Events[EscalationKey(base.key, level)].DeadlineAt
	}
	if now.Before(due) {
		return roundResult{next: due}, nil
	}
	e.w.submit(e.escalationOf(base, level+1)) // idempotent: its round observes it once
	return roundResult{next: now.Add(e.backoff(1))}, nil
}

// escalationDue reports whether the step ev (an escalation) may be observed on st, the trail as
// loaded: its event is observed and not cleared and still stands, neither it nor an earlier step has
// been acknowledged (an approval is satisfied only by being decided), its deadline (step 1) or the
// previous step's (later steps) has passed, and for a later step the previous one is observed.
func (e *notifyEngine) escalationDue(ctx context.Context, ev notifyEvent, st NotifyState, now time.Time) (bool, error) {
	base := st.Events[ev.origKey]
	if base == nil || base.Cleared != nil {
		return false, nil
	}
	if ev.origKind != NotificationApprovalPending {
		if base.Acked != nil {
			return false, nil
		}
		for l := 1; l < ev.level; l++ {
			if se := st.Events[EscalationKey(ev.origKey, l)]; se != nil && se.Acked != nil {
				return false, nil
			}
		}
	}
	due := base.DeadlineAt
	if ev.level > 1 {
		prev := st.Events[EscalationKey(ev.origKey, ev.level-1)]
		if prev == nil {
			return false, nil
		}
		due = prev.DeadlineAt
	}
	if now.Before(due) {
		return false, nil
	}
	cur, _, err := e.current(ctx, ev, st)
	return cur, err
}
