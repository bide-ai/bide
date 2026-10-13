package agent

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// This file is the public surface of the built-in notifier: a Notifier is told, at least once and
// with a stable ID, when a run needs a person. See docs/design/notifier.md for the design and the
// guarantee. In short: events are emitted where a drive ends, delivery state lives in a
// NotifyTrail (never in the run's journal), and a delivery is at least once.

// NotificationKind says what a Notification is about.
type NotificationKind string

const (
	// NotificationOutcomeUnknown: a drive ended with *OutcomeUnknown.
	NotificationOutcomeUnknown NotificationKind = "outcome_unknown"
	// NotificationApprovalPending: a drive ended with *ApprovalPending.
	NotificationApprovalPending NotificationKind = "approval_pending"
	// NotificationRunAborted: the run reached the run:aborted end marker.
	NotificationRunAborted NotificationKind = "run_aborted"
	// NotificationRecoveryFailing: recovery has kept failing for a run for longer than the configured time.
	NotificationRecoveryFailing NotificationKind = "recovery_failing"
	// NotificationOverdue: an event was not acknowledged by its deadline.
	NotificationOverdue NotificationKind = "notification_overdue"
)

// NotifyHalt is the structured detail of a NotificationOutcomeUnknown notification.
type NotifyHalt struct {
	Op          OpRef
	Cause       HaltCause
	AttemptedAt time.Time
}

// NotifyApproval is the structured detail of a NotificationApprovalPending notification.
type NotifyApproval struct {
	ToolUseID string
	ToolName  string
	// Need and Of are the gate's policy, k of n. Both are 0 for a 1-of-1 gate.
	Need, Of  int
	Approvers []string // the gate's eligible approvers; nil for a 1-of-1 gate
	Approved  int      // approvals counted when the event was observed
}

// Notification is one delivery attempt of an event to a Notifier. It holds structured fields only:
// no tool arguments, results, or error text travel in it, except Err, which is for an in-process
// Notifier and which a Notifier that sends the notification elsewhere must not forward by default.
type Notification struct {
	// ID is the hex SHA-256 of Key: the same on every attempt, in every process and after every
	// restart. A sink that can drop duplicates should key on it.
	ID   string
	Key  string
	Kind NotificationKind
	// RunID is the root run, whose notification trail holds the event.
	RunID string
	// EventAt is when the run entered the state: the attempt marker's time for a halt, else the
	// time the event was first observed. Observed is when a notifier process first saw it.
	EventAt, Observed time.Time
	// Deadline is when the notification is overdue, if not acknowledged by then.
	Deadline time.Time
	// Attempt counts the delivery attempts this process has made for the event, from 1.
	Attempt int

	Halt      *NotifyHalt     // for NotificationOutcomeUnknown
	Approval  *NotifyApproval // for NotificationApprovalPending
	Principal *Principal      // the run's principal, nil if it has none
	// Reason is a code from a closed set, never error text: for NotificationRecoveryFailing the cause
	// class ("resume_error", "store_error", "not_resumable"), for NotificationOverdue "deadline".
	Reason string
	// Err is the typed error that raised the event, for an in-process Notifier.
	Err error
}

// Notifier delivers one Notification. Notify must return nil only once the sink has accepted it.
// It is called again for the same ID after an error, a crash or a lost reply, so a sink that can
// dedupe should use Notification.ID to do so. Return an error wrapping ErrNotifyPermanent for a
// failure that retrying within the same round cannot fix (a 4xx other than 429); the event is
// retried in a later round all the same, since a receiver's configuration can change.
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

// ErrNotifyPermanent marks a Notifier error as one a retry within the round cannot fix.
var ErrNotifyPermanent = errors.New("agent: notification refused by the sink")

// NotifyAck is what a Notifier reports about an accepted delivery, recorded in the trail beside the
// acknowledgement as audit evidence of how it was authenticated.
type NotifyAck struct {
	Status int         // the sink's HTTP status, 0 if not HTTP
	Auth   *NotifyAuth // how the delivery was authenticated, nil if it was not
}

type ackKey struct{}

// ReportNotifyAck is called by a Notifier, with the context it was given, to say how a delivery it
// is about to acknowledge was authenticated and answered. A call outside Notifier.Notify is a
// no-op.
func ReportNotifyAck(ctx context.Context, ack NotifyAck) {
	if p, ok := ctx.Value(ackKey{}).(*NotifyAck); ok && p != nil {
		*p = ack
	}
}

// NotifyOption configures the notifier WithNotifier and NotifyPending build.
type NotifyOption interface {
	applyNotify(*notifyConfig) error
}

type notifyOption func(*notifyConfig) error

func (f notifyOption) applyNotify(c *notifyConfig) error { return f(c) }

// WithNotifyDeadline sets how long after an event is observed it may go unacknowledged before it is
// overdue (default 5 minutes). Past it, an overdue record is written, a NotificationOverdue event is
// raised, and the original keeps being retried. d must be positive.
func WithNotifyDeadline(d time.Duration) NotifyOption {
	return notifyOption(func(c *notifyConfig) error {
		if d <= 0 {
			return fmt.Errorf("WithNotifyDeadline(%v): must be positive: %w", d, ErrConfig)
		}
		c.deadline = d
		return nil
	})
}

// WithNotifyClaimTTL sets how long a process's claim on an event lasts (default 2 minutes). A
// crashed deliverer's claim lapses after it, which is how another process takes over, so it bounds
// the delay of a takeover; it must exceed one round's work. d must be positive.
func WithNotifyClaimTTL(d time.Duration) NotifyOption {
	return notifyOption(func(c *notifyConfig) error {
		if d <= 0 {
			return fmt.Errorf("WithNotifyClaimTTL(%v): must be positive: %w", d, ErrConfig)
		}
		c.claimTTL = d
		return nil
	})
}

// WithNotifyRetry sets the backoff between rounds of a failing delivery: base, doubling per
// failure, up to max (defaults 1 second and 15 minutes). A delivery is retried for as long as the
// process runs; this only paces it. Both must be positive, and base at most max.
func WithNotifyRetry(base, max time.Duration) NotifyOption {
	return notifyOption(func(c *notifyConfig) error {
		if base <= 0 || max < base {
			return fmt.Errorf("WithNotifyRetry(%v, %v): need 0 < base <= max: %w", base, max, ErrConfig)
		}
		c.backoffBase, c.backoffMax = base, max
		return nil
	})
}

// WithNotifyFailingAfter sets how many failed delivery attempts make a delivery failing (default
// 8). A round makes up to 3 attempts, so the count is taken in rounds, ceil(n/3), and from the
// trail: rounds that claimed the event and never acknowledged it, whichever process ran them and
// however they ended (a crash included). A failing record is written, once, and the failure is
// reported to the handler of WithNotifyErrors. Delivery continues. n must be at least 1.
func WithNotifyFailingAfter(n int) NotifyOption {
	return notifyOption(func(c *notifyConfig) error {
		if n < 1 {
			return fmt.Errorf("WithNotifyFailingAfter(%d): must be at least 1: %w", n, ErrConfig)
		}
		c.failingAfter = n
		return nil
	})
}

// WithNotifyRecoveryFailingAfter sets how long recovery must keep failing for a run before a
// NotificationRecoveryFailing event is raised (default 15 minutes). The time runs from the first failing
// pass, recorded in the trail, so it counts across restarts and processes. d must be positive.
func WithNotifyRecoveryFailingAfter(d time.Duration) NotifyOption {
	return notifyOption(func(c *notifyConfig) error {
		if d <= 0 {
			return fmt.Errorf("WithNotifyRecoveryFailingAfter(%v): must be positive: %w", d, ErrConfig)
		}
		c.recoveryAfter = d
		return nil
	})
}

// WithNotifyHaltAge sets how old an attempt marker with no result must be before NotifyPending
// reports it as a halt (default 5 minutes). A sweep reads the journal and cannot tell a halted
// effect from one still running in a process it cannot see, so it waits: set it longer than your
// longest tool call or Step, or a slow call is reported as halted. A marker with no recorded time
// is not reported. It does not apply to a drive, which knows. d must be positive.
func WithNotifyHaltAge(d time.Duration) NotifyOption {
	return notifyOption(func(c *notifyConfig) error {
		if d <= 0 {
			return fmt.Errorf("WithNotifyHaltAge(%v): must be positive: %w", d, ErrConfig)
		}
		c.haltAge = d
		return nil
	})
}

// WithNotifyAbortedWithin sets how recent an aborted run must be for NotifyPending to raise its
// run_aborted event (default 24 hours), by the time the run started, which is the only time its
// journal records. It stops the first sweep after a notifier is enabled from paging for every run
// that ever aborted. A run whose run:start records no time is not raised. d must be positive.
func WithNotifyAbortedWithin(d time.Duration) NotifyOption {
	return notifyOption(func(c *notifyConfig) error {
		if d <= 0 {
			return fmt.Errorf("WithNotifyAbortedWithin(%v): must be positive: %w", d, ErrConfig)
		}
		c.abortedWithin = d
		return nil
	})
}

// WithNotifyHolder names this process in the claims it writes (default host, pid and a random
// number). Two processes must not share a name.
func WithNotifyHolder(id string) NotifyOption {
	return notifyOption(func(c *notifyConfig) error {
		if id == "" {
			return fmt.Errorf("WithNotifyHolder: empty holder: %w", ErrConfig)
		}
		c.holder = id
		return nil
	})
}

// WithOverdueNotifier sends NotificationOverdue events to n instead of the main Notifier, for the usual
// case where the main sink being down is why the event is overdue.
func WithOverdueNotifier(n Notifier) NotifyOption {
	return notifyOption(func(c *notifyConfig) error {
		if isNil(n) {
			return fmt.Errorf("WithOverdueNotifier: nil Notifier: %w", ErrConfig)
		}
		c.overdue = n
		return nil
	})
}

// WithNotifyErrors sets the function that receives the notifier's own failures: a trail or store
// error, a delivery that has become failing, a trail position it kept losing. Without it they are
// dropped (delivery retries regardless). It may be called from several goroutines.
func WithNotifyErrors(fn func(error)) NotifyOption {
	return notifyOption(func(c *notifyConfig) error {
		c.onError = fn
		return nil
	})
}

// NotifierOption is the result of WithNotifier: it configures an Agent (New, With), RecoverLoop and
// Recover.
type NotifierOption interface {
	Option
	RecoverOption
	RecoverLoopOption
}

type notifierOption struct {
	n     Notifier
	trail NotifyTrail
	opts  []NotifyOption
}

// WithNotifier tells n, at least once and with a stable ID, when a run needs a person: it halts on
// an unknown outcome, waits for approval, is aborted, or cannot be recovered. Delivery state is
// written to trail, which must be durable and shared by every process that drives runs for the
// guarantee to hold across restarts and processes (audit.NewNotifyTrail over a durable
// audit.EventStore); a nil trail is ErrConfig. The same value applies to an Agent, where the
// notification is raised at the end of every drive, and to RecoverLoop and Recover, where it also
// raises recovery_failing and covers a Resumer that drives no Agent with this option.
//
// A delivery runs on a goroutine the notifier owns, so a slow sink never delays Run. Without
// RecoverLoop or a call of NotifyPending, an event whose process dies before the first record of
// it is not found again: the guarantee needs one of them. FlushNotifications waits for deliveries
// in flight.
func WithNotifier(n Notifier, trail NotifyTrail, opts ...NotifyOption) NotifierOption {
	return notifierOption{n: n, trail: trail, opts: opts}
}

func (o notifierOption) engine(j *Journal, kind string) (*notifyEngine, error) {
	if isNil(o.n) {
		return nil, fmt.Errorf("WithNotifier: nil Notifier: %w", ErrConfig)
	}
	if isNil(o.trail) {
		return nil, fmt.Errorf("WithNotifier: nil NotifyTrail (the guarantee needs a durable trail): %w", ErrConfig)
	}
	return newNotifyEngine(j, o.n, o.trail, o.opts, kind)
}

func (o notifierOption) applyAgent(c *agentConfig) error {
	e, err := o.engine(c.a.store, "drive")
	if err != nil {
		return err
	}
	c.a.notify = e
	return nil
}

func (o notifierOption) applyRecover(c *recoverConfig) error {
	return o.applyRecoverLoop(c)
}

func (o notifierOption) applyRecoverLoop(c *recoverConfig) error {
	c.notifier = &o
	return nil
}

// FlushNotifications waits for the deliveries of a's notifier that are in flight to finish their
// round, and stops its worker; a later event starts it again. It returns ctx's error if ctx ends
// first. It does nothing for an agent with no WithNotifier. Events waiting out a backoff are not
// waited for: they are retried by the next process that drives or sweeps.
func (a *Agent) FlushNotifications(ctx context.Context) error {
	if a.notify == nil {
		return nil
	}
	return a.notify.flush(ctx)
}
