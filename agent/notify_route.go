package agent

import (
	"context"
	"encoding/json"
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
	case NotificationOutcomeUnknown, NotificationApprovalPending, NotificationApprovalExpired, NotificationRunAborted, NotificationRecoveryFailing:
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
// NotificationApprovalExpired, NotificationRunAborted or NotificationRecoveryFailing. The all-clear of an event goes to the
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
// acknowledgement), so that one receiver's acknowledgement is never taken for another's. An approver
// with no route is reached through the route of the kind (WithNotifyRoute(NotificationApprovalPending,
// ...), else the default receiver), which is delivered to as well whenever any approver of the gate
// has no route; a gate with none of its approvers routed, and a 1-of-1 gate (which names none), go
// to the route of the kind alone.
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

// SplitToKey splits the key of a fan-out delivery, "to:<receiver>:<key>" (see ToKey), into the
// receiver and the key of the event it delivers. ok is false for any other key.
func SplitToKey(key string) (receiver, base string, ok bool) {
	rest, ok := strings.CutPrefix(key, "to:")
	if !ok {
		return "", "", false
	}
	receiver, base, ok = strings.Cut(rest, ":")
	if !ok || !receiverName.MatchString(receiver) || base == "" {
		return "", "", false
	}
	return receiver, base, true
}

// ParseEscalationKey splits the key of an escalation step, "escalated:<level>:<key>" (see
// EscalationKey), into its level (from 1) and the key of the event it escalates. ok is false for any
// other key.
func ParseEscalationKey(key string) (level int, orig string, ok bool) {
	parts := strings.SplitN(key, ":", 3)
	if len(parts) != 3 || parts[0] != "escalated" || parts[2] == "" {
		return 0, "", false
	}
	level, err := strconv.Atoi(parts[1])
	if err != nil || level < 1 || strconv.Itoa(level) != parts[1] {
		return 0, "", false
	}
	return level, parts[2], true
}

// ValidReceiver reports whether name is one a trail may record for a receiver other than the
// default: what WithNotifyReceiver accepts, and not DefaultReceiver, which a trail records as no name.
func ValidReceiver(name string) bool {
	return receiverName.MatchString(name) && name != DefaultReceiver
}

// MaxEscalationSteps is the most steps an escalation chain may have.
const MaxEscalationSteps = maxEscalationSteps

// stripTo returns key without the "to:<receiver>:" prefix of a fan-out delivery.
func stripTo(key string) string {
	if _, base, ok := SplitToKey(key); ok {
		return base
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
	case strings.HasPrefix(key, "expired:"):
		return NotificationApprovalExpired
	}
	return ""
}

// routed returns the deliveries of ev: one, to the receiver its kind names, or, for an approval
// whose approvers are routed to several receivers, one for each, under a key of its own.
func (e *notifyEngine) routed(ev notifyEvent) []notifyEvent {
	var approvers []string
	if ev.approval != nil {
		approvers = ev.approval.Approvers
	}
	recvs := e.cfg.routing().Receivers(ev.kind, approvers)
	if len(recvs) == 1 {
		ev.receiver = receiverID(recvs[0])
		return []notifyEvent{ev}
	}
	out := make([]notifyEvent, 0, len(recvs))
	for _, r := range recvs {
		d := ev
		d.receiver, d.key = receiverID(r), ToKey(r, ev.key)
		out = append(out, d)
	}
	return out
}

// submit routes ev and queues each delivery, and the escalation chase of each one that has a chain.
func (e *notifyEngine) submit(ev notifyEvent) {
	for _, d := range e.routed(ev) {
		e.w.submit(d)
		if w, ok := e.chaseFor(d); ok {
			e.w.submit(w)
		}
	}
}

// roundRouted runs a round for each delivery of ev, in this goroutine (the sweep's way), and queues
// the escalation chase of each. The errors are joined.
func (e *notifyEngine) roundRouted(ctx context.Context, ev notifyEvent) error {
	var errs []error
	for _, d := range e.routed(ev) {
		if _, err := e.round(ctx, d); err != nil {
			errs = append(errs, err)
		}
		if w, ok := e.chaseFor(d); ok {
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

// chaseFor is the escalation chase of ev, if its kind has a chain.
func (e *notifyEngine) chaseFor(ev notifyEvent) (notifyEvent, bool) {
	if len(e.cfg.escalation[ev.kind]) == 0 {
		return notifyEvent{}, false
	}
	return chaseOf(ev), true
}

// chaseOf is the worker job that chases ev's deadline and escalates it. It is a job, not an event:
// it writes nothing to the trail until a step is due.
func chaseOf(ev notifyEvent) notifyEvent {
	b := ev
	return notifyEvent{key: chaseKey(ev.key), chase: true, runID: ev.runID, base: &b}
}

// chaseKey is the worker key of the escalation chase of the event with key.
func chaseKey(key string) string { return "escchase:" + key }

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
	d.chase, d.base, d.scan = false, nil, false
	return d
}

// escRound is one pass of the escalation chase of w.base: it brings the chain as far as the trail
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
			e.fillDetail(ctx, &base)                // a base rebuilt from the trail has its key's detail only
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
	e.fillDetail(ctx, &base)
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

// fillDetail completes ev, an event rebuilt from the trail (its key and operation fields), with the
// detail its own notifications carry that the journal holds: a gate's tool, policy and approvers, a
// halted call's tool name. A failed read leaves the event as it is: the escalation is sent without
// the detail, which is in the journal and in the original's notification.
func (e *notifyEngine) fillDetail(ctx context.Context, ev *notifyEvent) {
	if e.j == nil {
		return
	}
	run := ev.opRun
	if run == "" {
		run = ev.runID
	}
	switch {
	case ev.approval != nil && ev.approval.ToolName == "":
		call := ev.approval.ToolUseID
		var rec Record
		var ok bool
		var err error
		if reask := ev.approval.Reask; reask == "" {
			rec, ok, err = e.j.Get(ctx, run, ApprovalPendingStep(call))
		} else {
			// A re-ask event shows the policy the gate asks again under: its own note, if the journal
			// proves it (ValidReaskNote).
			recs, herr := e.j.History(ctx, run)
			if herr != nil {
				return
			}
			byName := valuesByName(recs)
			if rec, ok = byName[ApprovalReaskStep(call, reask == PendingSoDReapproval)]; ok {
				var rw ApprovalWait
				ok = json.Unmarshal(rec.Result, &rw) == nil && ValidReaskNote(byName, call, rec.Name, rw)
			}
		}
		if err != nil || !ok {
			return
		}
		var w ApprovalWait
		if json.Unmarshal(rec.Result, &w) != nil {
			return
		}
		ev.approval.ToolName = w.ToolName
		if !w.Single {
			ev.approval.Need, ev.approval.Of = w.Need, len(w.Approvers)
			ev.approval.Approvers = slices.Clone(w.Approvers)
		}
	case ev.halt != nil && ev.halt.Op.ToolName == "" && ev.halt.Op.Kind == OpTool:
		recs, err := e.j.History(ctx, run)
		if err != nil {
			return
		}
		if _, call, ok := FindToolCall(recs, ev.halt.Op.ID); ok {
			ev.halt.Op.ToolName = call.Name
		}
	}
}
