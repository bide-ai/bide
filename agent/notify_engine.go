package agent

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

// notifyConfig is what NotifyOptions write, over the defaults newNotifyEngine sets.
type notifyConfig struct {
	deadline      time.Duration
	claimTTL      time.Duration
	backoffBase   time.Duration
	backoffMax    time.Duration
	failingAfter  int
	recoveryAfter time.Duration
	holder        string
	overdue       Notifier
	onError       func(error)

	// Not options: tests set them.
	roundAttempts  int           // requests per round
	attemptBackoff time.Duration // before the second request of a round, doubling
	ackRetries     int           // in-place retries of the acked append
	now            func() time.Time
	sleep          func(context.Context, time.Duration) error
	jitter         func(time.Duration) time.Duration
}

func defaultNotifyConfig() notifyConfig {
	return notifyConfig{
		deadline:       5 * time.Minute,
		claimTTL:       2 * time.Minute,
		backoffBase:    time.Second,
		backoffMax:     15 * time.Minute,
		failingAfter:   8,
		recoveryAfter:  15 * time.Minute,
		holder:         defaultHolder(),
		roundAttempts:  3,
		attemptBackoff: time.Second,
		ackRetries:     3,
		now:            time.Now,
		sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		jitter: func(d time.Duration) time.Duration {
			// +-20%, so a fleet of processes does not retry in step.
			return d + time.Duration((rand.Float64()*0.4-0.2)*float64(d))
		},
	}
}

// notifyEvent is an event a notifier owes delivery of.
type notifyEvent struct {
	key     string
	kind    NotificationKind
	runID   string // the root run, whose trail holds the event
	opRun   string // the run whose journal holds the halted operation or the gate, if different
	eventAt time.Time

	halt      *NotifyHalt
	approval  *NotifyApproval
	principal *Principal
	reason    string
	err       error

	episode int    // for recovery_failing: the episode number
	origKey string // for an overdue notice: the event it is about
}

// notifyEngine runs the notify-once protocol of docs/design/notifier.md over a trail: observe,
// claim, deliver, acknowledge, with the trail's append-at-position as the only synchronization
// between processes.
type notifyEngine struct {
	j     *Journal
	n     Notifier
	trail NotifyTrail
	cfg   notifyConfig

	mu       sync.Mutex
	attempts map[string]int       // delivery attempts made by this process, per key
	fails    map[string]int       // failed attempts made by this process, per key
	firstAt  map[string]time.Time // when this process first sent each key
	w        *notifyWorker
}

func newNotifyEngine(j *Journal, n Notifier, trail NotifyTrail, opts []NotifyOption) (*notifyEngine, error) {
	cfg := defaultNotifyConfig()
	if err := applyOptions("WithNotifier", &cfg, opts, NotifyOption.applyNotify); err != nil {
		return nil, err
	}
	if cfg.claimTTL <= 0 || cfg.deadline <= 0 {
		return nil, fmt.Errorf("WithNotifier: non-positive deadline or claim TTL: %w", ErrConfig)
	}
	e := &notifyEngine{j: j, n: n, trail: trail, cfg: cfg,
		attempts: map[string]int{}, fails: map[string]int{}, firstAt: map[string]time.Time{}}
	e.w = newNotifyWorker(e)
	return e, nil
}

func (e *notifyEngine) report(err error) {
	if e.cfg.onError != nil && err != nil {
		e.cfg.onError(err)
	}
}

// roundResult is the outcome of one round for an event.
type roundResult struct {
	done bool      // the event is closed (acked or cleared): forget it
	next time.Time // when to try again, if not done
}

// maxTrailRetries bounds how often a round reloads after losing an append race before it gives up
// and leaves the event for the next round.
const maxTrailRetries = 8

// appendRec appends rec at seq. conflict is true when another process took the position (reload
// and decide again); err is any other failure.
func (e *notifyEngine) appendRec(ctx context.Context, trail string, seq int, rec NotifyRecord) (conflict bool, err error) {
	err = e.trail.Append(ctx, trail, seq, rec)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, ErrTrailConflict):
		return true, nil
	default:
		return false, err
	}
}

// round does one round for ev: bring its trail up to date, decide, and deliver if this process
// holds the claim. It returns when the event is closed, when another process holds a live claim,
// or after a round of failed attempts.
func (e *notifyEngine) round(ctx context.Context, ev notifyEvent) (roundResult, error) {
	name := NotifyTrailName(ev.runID)
	for range maxTrailRetries {
		recs, err := e.trail.Load(ctx, name)
		if err != nil {
			return roundResult{next: e.cfg.now().Add(e.backoff(e.failCount(ev.key)))}, err
		}
		st := FoldNotify(recs)
		now := e.cfg.now()
		es := st.Events[ev.key]

		if es == nil {
			at := ev.eventAt
			if at.IsZero() {
				at = now
			}
			rec := NotifyRecord{Kind: NotifyObserved, Key: ev.key, RunID: ev.runID, At: now,
				EventKind: string(ev.kind), EventAt: at, DeadlineAt: now.Add(e.cfg.deadline)}
			if _, err := e.appendRec(ctx, name, len(recs), rec); err != nil {
				return roundResult{next: now.Add(e.backoff(e.failCount(ev.key)))}, err
			}
			continue // reload: the position may have been taken, and the fold decides either way
		}
		if !es.Open() {
			return roundResult{done: true}, nil
		}

		cur, reason, err := e.current(ctx, ev, st)
		if err != nil {
			return roundResult{next: now.Add(e.backoff(e.failCount(ev.key)))}, err
		}
		if !cur {
			rec := NotifyRecord{Kind: NotifyCleared, Key: ev.key, RunID: ev.runID, At: now, Reason: reason}
			if _, err := e.appendRec(ctx, name, len(recs), rec); err != nil {
				return roundResult{next: now.Add(e.backoff(e.failCount(ev.key)))}, err
			}
			continue
		}

		if ev.kind != NotificationOverdue && es.OverdueDue(now) {
			rec := NotifyRecord{Kind: NotifyOverdue, Key: ev.key, RunID: ev.runID, At: now, Latency: now.Sub(es.ObservedAt)}
			conflict, err := e.appendRec(ctx, name, len(recs), rec)
			if err != nil {
				return roundResult{next: now.Add(e.backoff(e.failCount(ev.key)))}, err
			}
			if !conflict {
				e.w.submit(notifyEvent{key: OverdueKey(ev.key), kind: NotificationOverdue, runID: ev.runID,
					origKey: ev.key, reason: "deadline", principal: ev.principal, eventAt: now})
			}
			continue
		}

		live := es.ClaimLive(now)
		if live && es.Claim.Holder != e.cfg.holder {
			return roundResult{next: es.Claim.Until}, nil // another process is delivering it
		}
		if !live || es.Claim.Until.Sub(now) <= e.cfg.claimTTL/2 {
			rec := NotifyRecord{Kind: NotifyClaimed, Key: ev.key, RunID: ev.runID, At: now,
				Holder: e.cfg.holder, Until: now.Add(e.cfg.claimTTL)}
			conflict, err := e.appendRec(ctx, name, len(recs), rec)
			if err != nil {
				return roundResult{next: now.Add(e.backoff(e.failCount(ev.key)))}, err
			}
			if conflict {
				continue // someone else claimed first: reload and see
			}
		}
		return e.deliver(ctx, ev, es, name)
	}
	return roundResult{next: e.cfg.now().Add(e.backoff(e.failCount(ev.key)))},
		fmt.Errorf("notify: trail %s: lost the append race %d times in a row for %s", name, maxTrailRetries, ev.key)
}

func (e *notifyEngine) failCount(key string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.fails[key]
}

// backoff is the wait before the next round after fails failures.
func (e *notifyEngine) backoff(fails int) time.Duration {
	d := e.cfg.backoffBase
	for i := 1; i < fails && d < e.cfg.backoffMax; i++ {
		d *= 2
	}
	return e.cfg.jitter(min(d, e.cfg.backoffMax))
}

// deliver makes the attempts of one round and, on success, records the acknowledgement.
func (e *notifyEngine) deliver(ctx context.Context, ev notifyEvent, es *NotifyEvent, name string) (roundResult, error) {
	if ev.principal == nil {
		ev.principal = e.principalOf(ctx, ev.runID)
	}
	var lastErr error
	for i := range e.cfg.roundAttempts {
		e.mu.Lock()
		e.attempts[ev.key]++
		attempt := e.attempts[ev.key]
		if _, ok := e.firstAt[ev.key]; !ok {
			e.firstAt[ev.key] = e.cfg.now()
		}
		first := e.firstAt[ev.key]
		e.mu.Unlock()

		var ack NotifyAck
		nctx := context.WithValue(ctx, ackKey{}, &ack)
		lastErr = e.notifierFor(ev).Notify(nctx, e.notification(ev, es, attempt))
		if lastErr == nil {
			return e.ack(ctx, ev, name, first, attempt, ack)
		}
		e.mu.Lock()
		e.fails[ev.key]++
		f := e.fails[ev.key]
		e.mu.Unlock()
		if f == e.cfg.failingAfter {
			e.markFailing(ctx, ev, name, f, lastErr)
		}
		if ctx.Err() != nil || errors.Is(lastErr, ErrNotifyPermanent) {
			break
		}
		if i < e.cfg.roundAttempts-1 {
			if err := e.cfg.sleep(ctx, e.cfg.attemptBackoff<<i); err != nil {
				break
			}
		}
	}
	return roundResult{next: e.cfg.now().Add(e.backoff(e.failCount(ev.key)))}, lastErr
}

func (e *notifyEngine) notifierFor(ev notifyEvent) Notifier {
	if ev.kind == NotificationOverdue && e.cfg.overdue != nil {
		return e.cfg.overdue
	}
	return e.n
}

func (e *notifyEngine) notification(ev notifyEvent, es *NotifyEvent, attempt int) Notification {
	return Notification{
		ID: NotifyID(ev.key), Key: ev.key, Kind: ev.kind, RunID: ev.runID,
		EventAt: es.EventAt, Observed: es.ObservedAt, Deadline: es.DeadlineAt, Attempt: attempt,
		Halt: ev.halt, Approval: ev.approval, Principal: ev.principal, Reason: ev.reason, Err: ev.err,
	}
}

// ack records the sink's acknowledgement. The sink has accepted by now, so a failure to record it is
// retried in place: the cheaper window to close than the duplicate it would cause.
func (e *notifyEngine) ack(ctx context.Context, ev notifyEvent, name string, first time.Time, attempts int, ack NotifyAck) (roundResult, error) {
	var lastErr error
	for range e.cfg.ackRetries + 1 {
		recs, err := e.trail.Load(ctx, name)
		if err != nil {
			lastErr = err
		} else {
			if es := FoldNotify(recs).Events[ev.key]; es != nil && !es.Open() {
				return roundResult{done: true}, nil // closed meanwhile (cleared, or acked by a racing process)
			}
			rec := NotifyRecord{Kind: NotifyAcked, Key: ev.key, RunID: ev.runID, At: e.cfg.now(),
				FirstSentAt: first, Attempts: attempts, Status: ack.Status, Auth: ack.Auth}
			conflict, aerr := e.appendRec(ctx, name, len(recs), rec)
			if aerr == nil && !conflict {
				e.forget(ev.key)
				return roundResult{done: true}, nil
			}
			lastErr = aerr // nil on a lost race: reload and decide again
		}
		if ctx.Err() != nil {
			break
		}
	}
	// Delivered but not recorded: the next round delivers it again (a duplicate), as designed.
	return roundResult{next: e.cfg.now().Add(e.backoff(1))},
		fmt.Errorf("notify: %s delivered but its acknowledgement was not recorded: %w", ev.key, lastErr)
}

func (e *notifyEngine) forget(key string) {
	e.mu.Lock()
	delete(e.attempts, key)
	delete(e.fails, key)
	delete(e.firstAt, key)
	e.mu.Unlock()
}

// markFailing records, once, that delivery of ev is failing, and reports it.
func (e *notifyEngine) markFailing(ctx context.Context, ev notifyEvent, name string, failures int, cause error) {
	e.report(fmt.Errorf("notify: delivery of %s is failing after %d attempts (it keeps retrying): %w", ev.key, failures, cause))
	for range maxTrailRetries {
		recs, err := e.trail.Load(ctx, name)
		if err != nil {
			return
		}
		st := FoldNotify(recs)
		if es := st.Events[ev.key]; es == nil || !es.Open() || es.Failing != nil {
			return
		}
		rec := NotifyRecord{Kind: NotifyFailing, Key: ev.key, RunID: ev.runID, At: e.cfg.now(), Failures: failures}
		if conflict, err := e.appendRec(ctx, name, len(recs), rec); err != nil || !conflict {
			return
		}
	}
}

// current reports whether ev still stands, and if not why (the reason a cleared record carries).
// It is read before every round, and races with delivery: a halt resolved mid-send can still yield
// one stale alert.
func (e *notifyEngine) current(ctx context.Context, ev notifyEvent, st NotifyState) (bool, string, error) {
	switch ev.kind {
	case NotificationRecoveryFailing:
		if st.FailingSince == nil || st.Episodes != ev.episode {
			return false, "recovered", nil
		}
		return true, "", nil
	case NotificationOverdue:
		if o := st.Events[ev.origKey]; o == nil || !o.Open() {
			return false, "resolved", nil
		}
		return true, "", nil
	case NotificationRunAborted:
		return true, "", nil
	}
	status, err := Status(ctx, e.j, ev.runID)
	if err != nil {
		return false, "", err
	}
	switch status.State {
	case RunCompleted, RunAborted, RunCancelled:
		return false, "ended", nil
	}
	opRun := ev.opRun
	if opRun == "" {
		opRun = ev.runID
	}
	var result string
	switch {
	case ev.halt != nil && ev.halt.Op.Kind == OpTool:
		result = ToolResultStep(ev.halt.Op.ID)
	case ev.halt != nil:
		result = ev.halt.Op.ID
	case ev.approval != nil:
		result = ToolResultStep(ev.approval.ToolUseID)
	default:
		return true, "", nil
	}
	_, ok, err := e.j.Get(ctx, opRun, result)
	if err != nil {
		return false, "", err
	}
	if ok {
		if ev.halt != nil {
			return false, "resolved", nil
		}
		return false, "decided", nil
	}
	return true, "", nil
}
