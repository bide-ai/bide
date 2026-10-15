package agent

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

// minClaimTTL is the shortest claim WithNotifyClaimTTL accepts.
const minClaimTTL = 30 * time.Second

// notifyConfig is what NotifyOptions write, over the defaults newNotifyEngine sets.
type notifyConfig struct {
	deadline      time.Duration
	claimTTL      time.Duration
	backoffBase   time.Duration
	backoffMax    time.Duration
	failingAfter  int
	recoveryAfter time.Duration
	haltAge       time.Duration
	abortedWithin time.Duration
	upgradeWindow time.Duration // how long after the first backfill the second pass runs
	skew          time.Duration // margin added to another holder's claim before it counts as lapsed
	maxInflight   int           // rounds in flight at once in the worker
	holder        string
	overdue       Notifier
	onError       func(error)

	receivers      map[string]Notifier                   // named receivers besides the default
	routes         map[NotificationKind]string           // receiver by kind
	approverRoutes map[string]string                     // receiver by approver
	escalation     map[NotificationKind][]EscalationStep // chain by kind

	// Not options: tests set them.
	roundAttempts  int           // requests per round
	attemptBackoff time.Duration // before the second request of a round, doubling
	ackBackoff     time.Duration // before the first retry of the acked append, doubling to 8s
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
		haltAge:        5 * time.Minute,
		abortedWithin:  24 * time.Hour,
		upgradeWindow:  24 * time.Hour,
		holder:         defaultHolder(),
		roundAttempts:  3,
		attemptBackoff: time.Second,
		ackBackoff:     500 * time.Millisecond,
		skew:           2 * time.Minute,
		maxInflight:    8,
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

	// keyFn, when set, computes the event's real key in the worker, off the drive's path (a halt on
	// an untimed marker needs a read of the journal); key is a provisional one until then.
	keyFn func(context.Context) (string, error)

	episode  int              // for recovery_failing: the episode number
	origKey  string           // for an overdue notice, an all-clear or an escalation: the event it is about
	origKind NotificationKind // for an all-clear or an escalation: that event's kind

	receiver string        // the receiver the event is to: "" for the default (see receiverID)
	level    int           // for an escalation: its step of the chain, from 1
	deadline time.Duration // when set, the event's own deadline, in place of the notifier's

	// chase marks the escalation chase of base (see escRound), not an event: key is chaseKey.
	chase bool
	base  *notifyEvent

	// scan marks the catch-up job of runID's trail (see clearScan), not an event: key is scanKey.
	scan bool
}

// scanKey is the worker key of the catch-up scan of run's trail.
func scanKey(run string) string { return "scan:" + run }

// notifyEngine runs the notify-once protocol of docs/design/notifier.md over a trail: observe,
// claim, deliver, acknowledge, with the trail's append-at-position as the only synchronization
// between processes.
type notifyEngine struct {
	j     *Journal
	n     Notifier
	trail NotifyTrail
	cfg   notifyConfig
	// holder is the name this engine's claims carry (see newNotifyEngine).
	holder string

	mu       sync.Mutex
	unacked  map[string]pendingAck // deliveries the sink accepted whose acknowledgement could not be recorded
	keyMu    map[string]*keyLock   // one round at a time per key, in this engine; present while one is held or waited for
	attempts map[string]int        // delivery attempts made by this process, per key
	fails    map[string]int        // failed attempts made by this process, per key
	firstAt  map[string]time.Time  // when this process first sent each key
	stuck    map[string]int        // Notify calls started and not returned (abandoned ones included), per event key
	globalAt time.Time             // when a recovery pass last signed the global trails again
	stuckAll int                   // and in all
	w        *notifyWorker
}

// newNotifyEngine builds an engine for deliverer kind ("drive", "recover", "sweep", "flow"). Its
// claims name a holder that is unique to this engine instance and to its kind: the configured
// holder name, the kind, and a random nonce, so two deliverers in one process (a drive's notifier
// and a sweep, say) or a restarted process never read each other's live claim as their own.
func newNotifyEngine(j *Journal, n Notifier, trail NotifyTrail, opts []NotifyOption, kind string) (*notifyEngine, error) {
	cfg := defaultNotifyConfig()
	if err := applyOptions("WithNotifier", &cfg, opts, NotifyOption.applyNotify); err != nil {
		return nil, err
	}
	if cfg.claimTTL <= 0 || cfg.deadline <= 0 {
		return nil, fmt.Errorf("WithNotifier: non-positive deadline or claim TTL: %w", ErrConfig)
	}
	if err := cfg.validateRouting(); err != nil {
		return nil, err
	}
	e := &notifyEngine{j: j, n: n, trail: trail, cfg: cfg,
		holder:   fmt.Sprintf("%s/%s/%016x", cfg.holder, kind, rand.Uint64()),
		attempts: map[string]int{}, fails: map[string]int{}, firstAt: map[string]time.Time{},
		stuck: map[string]int{}, unacked: map[string]pendingAck{}, keyMu: map[string]*keyLock{}}
	e.w = newNotifyWorker(e)
	return e, nil
}

func (e *notifyEngine) report(err error) {
	if reportGuard != nil {
		reportGuard()
	}
	if e.cfg.onError != nil && err != nil {
		e.cfg.onError(err)
	}
}

// pendingAck is what an engine owes the trail for a delivery the sink accepted: the next round
// records it instead of sending again.
type pendingAck struct {
	first    time.Time
	attempts int
	ack      NotifyAck
}

// failingRounds is how many rounds that claimed and never acknowledged make a delivery failing:
// failingAfter attempts at roundAttempts a round.
func (e *notifyEngine) failingRounds() int {
	return (e.cfg.failingAfter + e.cfg.roundAttempts - 1) / e.cfg.roundAttempts
}

// roundResult is the outcome of one round for an event.
type roundResult struct {
	done bool      // the event is closed (acked or cleared): forget it
	next time.Time // when to try again, if not done
	// deferred says why a round that delivered nothing ended without an error: it found another
	// deliverer's live claim, or the event's pace. FlushNotifications reports it as undelivered.
	deferred error
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
	if ev.scan {
		return e.scanRound(ctx, ev)
	}
	if ev.chase {
		return e.escRound(ctx, ev)
	}
	e.mu.Lock()
	km := e.keyMu[ev.key]
	if km == nil {
		km = &keyLock{}
		e.keyMu[ev.key] = km
	}
	km.refs++
	e.mu.Unlock()
	km.mu.Lock()
	defer func() {
		km.mu.Unlock()
		e.mu.Lock()
		if km.refs--; km.refs == 0 {
			delete(e.keyMu, ev.key)
		}
		e.mu.Unlock()
	}()
	res, err := e.roundLocked(ctx, ev)
	if res.done {
		e.forget(ev.key)
		if err == nil && ev.kind == NotificationResolved && ev.origKey != "" {
			// Nothing more is owed for the event: unlist it (a scan does it too, if this fails).
			if uerr := e.unwatch(ctx, ev.origKey); uerr != nil {
				e.report(uerr)
			}
		}
	}
	return res, err
}

// maxStuckCalls bounds the Notify calls an engine has running at once, abandoned or not, and an
// event has one: a Notifier that never returns cannot make it start goroutines without end, and
// a deaf event does not starve the others.
const maxStuckCalls = 256

// keyLock is the mutex of one event key and the number of rounds holding or waiting for it.
type keyLock struct {
	mu   sync.Mutex
	refs int
}

// callTimeout bounds one call of a Notifier. A round makes at most roundAttempts+1 calls (the
// overdue notice is one more), so with an eighth of the claim for each, the calls take at most half
// of it; the backoff between them and the acknowledgements' retries (an eighth for the event's, an
// eighth for the notice's) bring a round to six eighths of a claim and a few seconds, and a round
// starts only when more than that remains (see roundBudget), so it ends inside the claim it holds.
func (e *notifyEngine) callTimeout() time.Duration { return e.cfg.claimTTL / 8 }

// roundBudget is the claim a round must have left to start under it: the worst a round can take.
// A round makes roundAttempts+1 calls (the overdue notice is one) of callTimeout each, sleeps
// attemptBackoff, then twice that, between its attempts, and spends an eighth of the claim on the
// acknowledgement, and another eighth on the overdue notice's acknowledgement. With the call limit
// an eighth of the claim, that is six eighths of it and the backoff, still inside a fresh claim.
func (e *notifyEngine) roundBudget() time.Duration {
	calls := time.Duration(e.cfg.roundAttempts+1) * e.callTimeout()
	sleeps := e.cfg.attemptBackoff * time.Duration(1<<(e.cfg.roundAttempts-1)-1)
	return calls + sleeps + 2*(e.cfg.claimTTL/8)
}

// noticeClaimTTL is how long a peer's claim on an overdue notice's own key lasts: long enough for
// the notice's one call and its acknowledgement's retries (an eighth of the event's claim each),
// with half a minute to spare, and at least two minutes, but never longer than the event's claim.
// A shorter notice claim could lapse while its call is still running and let another peer send the
// notice again.
func (e *notifyEngine) noticeClaimTTL() time.Duration {
	return min(e.cfg.claimTTL, max(2*time.Minute, 2*(e.cfg.claimTTL/8)+30*time.Second))
}

// callNotify makes one call of n, bounded by callTimeout whether or not n honors its context: the
// call runs on a goroutine and is abandoned when the time is up, counting as failed (a Notifier
// that ignores its context must not hold a round, its key and a worker slot for good). An abandoned
// call that later succeeds means the sink got the notification and the next attempt repeats it,
// within the duplicate bound.
func (e *notifyEngine) callNotify(ctx context.Context, n Notifier, note Notification) (NotifyAck, error) {
	e.mu.Lock()
	if e.stuck[note.Key] > 0 || e.stuckAll >= maxStuckCalls {
		e.mu.Unlock()
		return NotifyAck{}, fmt.Errorf("notify: an earlier call for %s was abandoned at its timeout and has not returned (or %d calls are in that state); no new call is made until it does", note.Key, maxStuckCalls)
	}
	e.stuck[note.Key]++
	e.stuckAll++
	e.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, e.callTimeout())
	defer cancel()
	type result struct {
		ack NotifyAck
		err error
	}
	out := make(chan result, 1)
	go func() {
		// The call is no longer in flight once the Notifier has returned, and that is recorded
		// before the answer is sent: a caller that has its answer may make the next call at once,
		// and must not find this one still counted (and refuse its own call as abandoned). finish
		// runs once, from whichever exit comes first.
		var once sync.Once
		finish := func(r result) {
			once.Do(func() {
				e.mu.Lock()
				if e.stuck[note.Key]--; e.stuck[note.Key] == 0 {
					delete(e.stuck, note.Key)
				}
				e.stuckAll--
				e.mu.Unlock()
				out <- r
			})
		}
		defer func() {
			if r := recover(); r != nil { // a Notifier that panics fails the call, not the process
				finish(result{NotifyAck{}, fmt.Errorf("notify: the Notifier panicked: %v", r)})
				return
			}
			// Reached with no answer sent: the Notifier left through runtime.Goexit (a sink that
			// calls t.FailNow does). The call is over and failed; the caller must not wait for it,
			// nor the key be blocked by a call counted as in flight for good.
			finish(result{NotifyAck{}, errors.New("notify: the Notifier exited without returning")})
		}()
		var ack NotifyAck
		err := n.Notify(context.WithValue(cctx, ackKey{}, &ack), note)
		finish(result{ack, err})
	}()
	late := func(err error) error {
		if err != nil && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("notify: the call took longer than %v (the call limit is an eighth of the claim, %v; raise WithNotifyClaimTTL if the sink needs longer): %w", e.callTimeout(), e.cfg.claimTTL, err)
		}
		return err
	}
	select {
	case r := <-out:
		return r.ack, late(r.err)
	case <-cctx.Done():
		select {
		case r := <-out: // it returned as the time ran out: take its answer
			return r.ack, late(r.err)
		default:
		}
		return NotifyAck{}, late(cctx.Err())
	}
}

// claimLive reports whether es has a claim that another deliverer must respect at now. A holder's
// own claim is live until its Until; another holder's until Until plus the skew margin, because
// Until was written by that holder's clock and read by this one's, and a peer running ahead by less
// than the margin plus what is left of the claim must not take a live claim over. The tolerated skew
// is about claimTTL + skew minus the round's elapsed time: 2.5 minutes at the defaults.
func (e *notifyEngine) claimLive(es *NotifyEvent, now time.Time) bool {
	if !es.Open() || es.Claim == nil {
		return false
	}
	if es.Claim.Holder == e.holder {
		return now.Before(es.Claim.Until)
	}
	return now.Before(es.Claim.Until.Add(e.cfg.skew))
}

// pace is the earliest time a new round for es may start: the last claim plus the backoff for the
// rounds the trail records, with no jitter so every deliverer agrees. It is what bounds the claims
// of an undelivered event to the backoff cap however many deliverers or sweeps look at it.
func (e *notifyEngine) pace(es *NotifyEvent) time.Time {
	if es.Claim == nil {
		return time.Time{}
	}
	d := e.cfg.backoffBase
	for i := 1; i < es.Rounds && d < e.cfg.backoffMax; i++ {
		d *= 2
	}
	return es.Claim.At.Add(min(d, e.cfg.backoffMax))
}

func (e *notifyEngine) roundLocked(ctx context.Context, ev notifyEvent) (roundResult, error) {
	name := NotifyTrailName(ev.runID)
	triedFailing := false
	conflicts := 0
	for steps := 0; steps < maxTrailSteps && conflicts < maxTrailRetries; steps++ {
		if conflicts > 0 {
			e.conflictBackoff(ctx, conflicts) // another process took the position: do not retry in step
		}
		res, err, act := e.roundStep(ctx, ev, name, &triedFailing, &conflicts)
		if act == stepReturn {
			return res, err
		}
	}
	return roundResult{next: e.cfg.now().Add(e.backoff(e.failCount(ev.key)))},
		fmt.Errorf("notify: trail %s: lost the append race %d times in a row for %s", name, maxTrailRetries, ev.key)
}

// roundStepAct says what a step of a round leaves the round to do.
type roundStepAct int

const (
	stepRetry  roundStepAct = iota // reload the trail and decide again
	stepReturn                     // the round is over: return the step's result
)

// maxTrailSteps bounds the steps of one round, whatever their outcome: a step that made an append
// (an observation, an overdue record) is progress and not a lost race, so it does not count against
// maxTrailRetries, but a round must still end.
const maxTrailSteps = 32

// roundStep is one pass of a round: load the trail, decide, and make at most one append. The events
// of a run share its trail, and rounds of different keys in this process (a drive's notifier, a
// sweep, the worker) would lose the position to each other over and over if they raced, so one pass
// at a time runs per trail in the process (lockTrail); the lock is released before anything slow (a
// delivery, an acknowledgement). A position lost to another process counts in conflicts.
func (e *notifyEngine) roundStep(ctx context.Context, ev notifyEvent, name string, triedFailing *bool, conflicts *int) (roundResult, error, roundStepAct) {
	unlock, lerr := lockTrail(ctx, name)
	if lerr != nil {
		return roundResult{next: e.cfg.now().Add(e.backoff(e.failCount(ev.key)))}, lerr, stepReturn
	}
	var released bool
	release := func() {
		if !released {
			released = true
			unlock()
		}
	}
	defer release()
	fail := func(err error, now time.Time) (roundResult, error, roundStepAct) {
		return roundResult{next: now.Add(e.backoff(e.failCount(ev.key)))}, err, stepReturn
	}
	// appended records the outcome of an append that is retried whatever happens: a lost position
	// counts as a conflict; any other error ends the round.
	appended := func(conflict bool, err error, now time.Time) (roundResult, error, roundStepAct) {
		if err != nil {
			return fail(err, now)
		}
		if conflict {
			*conflicts++
		}
		return roundResult{}, nil, stepRetry
	}

	recs, err := e.trail.Load(ctx, name)
	if err != nil {
		return roundResult{next: e.cfg.now().Add(e.backoff(e.failCount(ev.key)))}, err, stepReturn
	}
	st := FoldNotify(recs)
	now := e.cfg.now()
	es := st.Events[ev.key]

	if es == nil {
		if ev.kind == NotificationEscalated {
			// A step is observed only if it is due on the trail this append is made at (the
			// append at len(recs) fails if the trail has moved), and its event still stands.
			due, err := e.escalationDue(ctx, ev, st, now)
			if err != nil {
				return fail(err, now)
			}
			if !due {
				return roundResult{done: true}, nil, stepReturn // the watch decides again when it is
			}
		}
		at := ev.eventAt
		if at.IsZero() {
			at = now
		}
		// An event that can stop being current is listed before it is observed, so no event that
		// is delivered is missing from the list a sweep reads (see NotifyOpenTrail).
		if resolvable(string(ev.kind)) {
			if err := e.watch(ctx, ev.key, ev.runID); err != nil {
				return fail(err, now)
			}
		}
		dl := e.cfg.deadline
		if ev.deadline > 0 {
			dl = ev.deadline
		}
		rec := NotifyRecord{Kind: NotifyObserved, Key: ev.key, RunID: ev.runID, At: now,
			EventKind: string(ev.kind), EventAt: at, DeadlineAt: now.Add(dl), Receiver: ev.receiver, Level: ev.level}
		rec.OpRun, rec.OpKind, rec.OpID = ev.operation()
		if ev.kind == NotificationResolved {
			// A clear is never overdue, so it has no deadline; it records why the event ended.
			rec.DeadlineAt, rec.Reason = time.Time{}, ev.reason
		}
		conflict, aerr := e.appendRec(ctx, name, len(recs), rec)
		return appended(conflict, aerr, now) // reload: the position may have been taken, and the fold decides either way
	}
	if !es.Open() {
		// An event cleared after a delivery was attempted may have reached its receiver: its
		// all-clear is owed. Whoever sees it cleared queues it, so a death after the clearing
		// loses nothing; the all-clear's own records make it once.
		if es.Cleared != nil && es.Rounds > 0 && resolvable(es.Kind) && st.Events[ResolvedKey(ev.key)] == nil {
			e.w.submit(clearOf(es, es.Cleared.Reason, es.Cleared.At))
		}
		return roundResult{done: true}, nil, stepReturn
	}

	cur, reason, err := e.current(ctx, ev, st)
	if err != nil {
		return fail(err, now)
	}
	if !cur {
		rec := NotifyRecord{Kind: NotifyCleared, Key: ev.key, RunID: ev.runID, At: now, Reason: reason}
		conflict, aerr := e.appendRec(ctx, name, len(recs), rec)
		return appended(conflict, aerr, now)
	}

	// A delivery this process made and could not record: record it, do not send again.
	e.mu.Lock()
	pa, owed := e.unacked[ev.key]
	e.mu.Unlock()
	if owed {
		release()
		res, err := e.ack(ctx, ev, name, pa.first, pa.attempts, pa.ack)
		return res, err, stepReturn
	}

	if ev.kind != NotificationOverdue && ev.kind != NotificationResolved && ev.kind != NotificationEscalated && es.OverdueDue(now) {
		rec := NotifyRecord{Kind: NotifyOverdue, Key: ev.key, RunID: ev.runID, At: now, Latency: now.Sub(es.ObservedAt)}
		conflict, aerr := e.appendRec(ctx, name, len(recs), rec)
		return appended(conflict, aerr, now) // reload: the round that delivers the event sends the notice
	}
	// Rounds that claimed the event and never acknowledged it, crashed ones included, make it
	// failing before another round starts.
	if es.Failing == nil && es.Rounds >= e.failingRounds() && !*triedFailing {
		*triedFailing = true
		release()
		e.markFailing(ctx, ev, name)
		return roundResult{}, nil, stepRetry // reload: the trail has a record more
	}

	live := e.claimLive(es, now)
	if live && es.Claim.Holder != e.holder {
		release()
		// The holder sends the overdue notice in its own rounds. If it claimed before the event
		// was overdue it has not had the chance, and may be gone for the whole claim: a peer sends
		// the notice, under a short claim of its own on the notice's key, so that peers do not
		// send it together. A holder that is only slow skips the notice while the peer's claim on it is
		// live, and sends it in a later round if the peer's send never lands.
		if ev.kind != NotificationOverdue && es.Overdue != nil && es.Claim.At.Before(es.Overdue.At) {
			if ev.principal == nil {
				ev.principal = e.principalOf(ctx, ev.runID)
			}
			e.deliverOverdueNotice(ctx, ev, es, name, true)
		}
		return roundResult{next: es.Claim.Until.Add(e.cfg.skew), deferred: fmt.Errorf("notify: %s is held by another deliverer's claim until %s; not delivered by this process", ev.key, es.Claim.Until.Format(time.RFC3339))}, nil, stepReturn // another deliverer has it
	}
	if !live || es.Claim.Until.Sub(now) <= e.roundBudget() {
		if pace := e.pace(es); now.Before(pace) && !live {
			return roundResult{next: pace, deferred: fmt.Errorf("notify: %s is waiting out its backoff until %s; not delivered by this process", ev.key, pace.Format(time.RFC3339))}, nil, stepReturn // too soon after the last round, whoever ran it
		}
		rec := NotifyRecord{Kind: NotifyClaimed, Key: ev.key, RunID: ev.runID, At: now,
			Holder: e.holder, Until: now.Add(e.cfg.claimTTL)}
		conflict, aerr := e.appendRec(ctx, name, len(recs), rec)
		if aerr != nil {
			return fail(aerr, now)
		}
		if conflict {
			*conflicts++ // someone else claimed first: reload and see
			return roundResult{}, nil, stepRetry
		}
	}
	release()
	res, derr := e.deliver(ctx, ev, es, name)
	return res, derr, stepReturn
}

// conflictBackoff waits a little before a round retries after losing a position to another process:
// exponential from a millisecond, capped, with jitter, so processes that keep colliding drift
// apart. The round's own claim and budget are unaffected (it holds none yet).
func (e *notifyEngine) conflictBackoff(ctx context.Context, conflicts int) {
	d := time.Millisecond << min(conflicts, 6)
	_ = e.cfg.sleep(ctx, e.cfg.jitter(d))
}

// trailLocks serialize the load-decide-append passes on one trail within the process, across every
// engine in it (a drive's notifier, a sweep, a recovery loop). An entry lives only while a pass
// holds or waits for it (refcounted, deleted at zero), so the map is the size of the trails in use at
// the moment, not of every run the process has seen. A waiter gives up when its context ends, so a
// store that hangs on one trail holds up that trail's rounds only, and only until they are cancelled.
// A pass holds it across the store's load and append and the journal reads that decide, never across
// a Notifier call or a callback.
// reportGuard, set only by tests, runs at every report to the user's error handler: the handler
// is user code, so it must never run under a trail lock.
var reportGuard func()

var trailLocks = struct {
	mu sync.Mutex
	m  map[string]*trailLock
}{m: map[string]*trailLock{}}

type trailLock struct {
	held chan struct{} // capacity one: a send takes the lock
	refs int
}

// lockTrail takes the process-wide lock of the trail called name and returns its release, or ctx's
// error if ctx ends first.
func lockTrail(ctx context.Context, name string) (func(), error) {
	trailLocks.mu.Lock()
	l := trailLocks.m[name]
	if l == nil {
		l = &trailLock{held: make(chan struct{}, 1)}
		trailLocks.m[name] = l
	}
	l.refs++
	trailLocks.mu.Unlock()
	leave := func() {
		trailLocks.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(trailLocks.m, name)
		}
		trailLocks.mu.Unlock()
	}
	select {
	case l.held <- struct{}{}:
	case <-ctx.Done():
		leave()
		return nil, ctx.Err()
	}
	forget := noteLockTaken()
	return func() {
		if forget != nil {
			forget()
		}
		<-l.held
		leave()
	}, nil
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
	if es.Overdue != nil {
		e.deliverOverdueNotice(ctx, ev, es, name, false)
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
		ack, lastErr = e.callNotify(ctx, e.notifierFor(ev), e.notification(ev, es, attempt))
		if lastErr == nil {
			return e.ack(ctx, ev, name, first, attempt, ack)
		}
		e.mu.Lock()
		e.fails[ev.key]++
		e.mu.Unlock()
		if ctx.Err() != nil || errors.Is(lastErr, ErrNotifyPermanent) {
			break
		}
		if i < e.cfg.roundAttempts-1 {
			if err := e.cfg.sleep(ctx, e.cfg.attemptBackoff<<i); err != nil {
				break
			}
		}
	}
	e.markFailing(ctx, ev, name) // this round has failed: it may be the one that makes the delivery failing
	return roundResult{next: e.cfg.now().Add(e.backoff(e.failCount(ev.key)))}, lastErr
}

func (e *notifyEngine) notifierFor(ev notifyEvent) Notifier {
	if ev.kind == NotificationOverdue && e.cfg.overdue != nil {
		return e.cfg.overdue
	}
	return e.notifierOf(ev)
}

// deliverOverdueNotice sends the notice that es, which is overdue, is not acknowledged. The deliverer
// that holds the event's claim sends it inside its round, as part of it (claim is false): the notice
// needs no claim of its own then, so an undelivered event writes one claim per round however many notices it owes, and the
// notice is derived from the trail on every round (the event open, its overdue record present, no
// acknowledged notice), so it survives a death after the overdue record, a failed delivery and a
// restart. It is a single attempt; the next round tries again.
func (e *notifyEngine) deliverOverdueNotice(ctx context.Context, ev notifyEvent, es *NotifyEvent, name string, claim bool) {
	key := OverdueKey(ev.key)
	e.mu.Lock()
	pa, owed := e.unacked[key]
	e.mu.Unlock()
	if owed { // delivered earlier and not recorded: record it, do not send again
		if _, err := e.ack(ctx, notifyEvent{key: key, runID: ev.runID}, name, pa.first, pa.attempts, pa.ack); err == nil {
			e.forget(key)
		}
		return
	}
	notice := notifyEvent{key: key, kind: NotificationOverdue, runID: ev.runID, origKey: ev.key, reason: "deadline",
		principal: ev.principal, eventAt: es.Overdue.At, receiver: ev.receiver}
	for range maxTrailRetries {
		recs, err := e.trail.Load(ctx, name)
		if err != nil {
			e.report(err)
			return
		}
		ne := FoldNotify(recs).Events[key]
		if ne != nil && !ne.Open() {
			return
		}
		if ne == nil {
			now := e.cfg.now()
			rec := NotifyRecord{Kind: NotifyObserved, Key: key, RunID: ev.runID, At: now,
				EventKind: string(NotificationOverdue), EventAt: es.Overdue.At, DeadlineAt: now.Add(e.cfg.deadline), Receiver: ev.receiver}
			if _, err := e.appendRec(ctx, name, len(recs), rec); err != nil {
				e.report(err)
				return
			}
			continue
		}
		if claim { // a peer sending it: exclusive with other peers, under a short claim of its own
			now := e.cfg.now()
			if ne.Claim != nil && (e.claimLive(ne, now) || now.Before(e.pace(ne))) {
				return
			}
			rec := NotifyRecord{Kind: NotifyClaimed, Key: key, RunID: ev.runID, At: now, Holder: e.holder,
				Until: now.Add(e.noticeClaimTTL())}
			conflict, aerr := e.appendRec(ctx, name, len(recs), rec)
			if aerr != nil {
				e.report(aerr)
				return
			}
			if conflict {
				continue
			}
		} else if ne.Claim != nil && ne.Claim.Holder != e.holder && e.claimLive(ne, e.cfg.now()) {
			return // the holder in its own round: a peer is sending the notice under its claim
		}
		e.mu.Lock()
		e.attempts[key]++
		attempt := e.attempts[key]
		if _, ok := e.firstAt[key]; !ok {
			e.firstAt[key] = e.cfg.now()
		}
		first := e.firstAt[key]
		e.mu.Unlock()
		ack, err := e.callNotify(ctx, e.notifierFor(notice), e.notification(notice, ne, attempt))
		if err != nil {
			e.report(fmt.Errorf("notify: the overdue notice for %s was not delivered (tried again next round): %w", ev.key, err))
			return
		}
		if _, aerr := e.ack(ctx, notice, name, first, attempt, ack); aerr != nil {
			e.report(aerr)
		} else {
			e.forget(key)
		}
		return
	}
}

func (e *notifyEngine) notification(ev notifyEvent, es *NotifyEvent, attempt int) Notification {
	n := Notification{
		ID: NotifyID(ev.key), Key: ev.key, Kind: ev.kind, RunID: ev.runID,
		EventAt: es.EventAt, Observed: es.ObservedAt, Deadline: es.DeadlineAt, Attempt: attempt,
		Halt: ev.halt, Approval: ev.approval, Principal: ev.principal, Reason: ev.reason, Err: ev.err,
		Receiver: receiverLabel(ev.receiver),
	}
	switch ev.kind {
	case NotificationResolved:
		n.Clears = &NotifyClears{Key: ev.origKey, ID: NotifyID(ev.origKey), Kind: ev.origKind}
	case NotificationEscalated:
		n.Escalates = &NotifyEscalates{Key: ev.origKey, ID: NotifyID(ev.origKey), Kind: ev.origKind, Level: ev.level}
	}
	return n
}

// ack records the sink's acknowledgement. The sink has accepted by now, so a failure to record it is
// retried in place, with a growing delay, for an eighth of the claim: the cheaper window to close than the duplicate it would cause. If the store stays down
// past that, this process remembers that it delivered and records the acknowledgement on its next
// round without sending again.
func (e *notifyEngine) ack(ctx context.Context, ev notifyEvent, name string, first time.Time, attempts int, ack NotifyAck) (roundResult, error) {
	var lastErr error
	limit := e.cfg.now().Add(e.cfg.claimTTL / 8)
	delay := e.cfg.ackBackoff
	for {
		recorded, err := func() (bool, error) {
			unlock, lerr := lockTrail(ctx, name) // one pass at a time on the trail in this process
			if lerr != nil {
				return false, lerr
			}
			defer unlock()
			recs, err := e.trail.Load(ctx, name)
			if err != nil {
				return false, err
			}
			if es := FoldNotify(recs).Events[ev.key]; es != nil && !es.Open() {
				return true, nil // closed meanwhile (cleared, or acked by a racing process)
			}
			rec := NotifyRecord{Kind: NotifyAcked, Key: ev.key, RunID: ev.runID, At: e.cfg.now(),
				FirstSentAt: first, Attempts: attempts, Status: ack.Status, Auth: ack.Auth}
			conflict, aerr := e.appendRec(ctx, name, len(recs), rec)
			return aerr == nil && !conflict, aerr // nil error on a lost race: reload and decide again
		}()
		if recorded {
			return roundResult{done: true}, nil
		}
		lastErr = err
		remaining := limit.Sub(e.cfg.now())
		if ctx.Err() != nil || remaining <= 0 {
			break
		}
		if err := e.cfg.sleep(ctx, min(delay, remaining)); err != nil { // never past the limit
			break
		}
		delay = min(delay*2, 8*time.Second)
	}
	// Delivered but not recorded. The next round of this process records the acknowledgement
	// without sending again; only a process that dies first repeats the delivery (the duplicate
	// window), and the claim it left lapses.
	e.mu.Lock()
	e.unacked[ev.key] = pendingAck{first: first, attempts: attempts, ack: ack}
	e.mu.Unlock()
	return roundResult{next: e.cfg.now().Add(e.backoff(1))},
		fmt.Errorf("notify: %s delivered but its acknowledgement was not recorded: %w", ev.key, lastErr)
}

func (e *notifyEngine) forget(key string) {
	e.mu.Lock()
	delete(e.unacked, key)
	delete(e.attempts, key)
	delete(e.fails, key)
	delete(e.firstAt, key)
	e.mu.Unlock()
}

// markFailing records, once, that delivery of ev is failing: its trail holds failingRounds rounds
// that claimed the event and never acknowledged it. It counts the trail, not this process's memory,
// so a deliverer that is restarted or dies in every round still raises it. It reports the failure to
// the handler of WithNotifyErrors when it is the process that writes the record.
func (e *notifyEngine) markFailing(ctx context.Context, ev notifyEvent, name string) {
	if rounds := e.writeFailing(ctx, ev, name); rounds > 0 {
		e.report(fmt.Errorf("notify: delivery of %s is failing after %d rounds without an acknowledgement (it keeps retrying)", ev.key, rounds))
	}
}

// writeFailing appends the failing record under the trail's lock, and returns the rounds it records,
// 0 if it wrote none; the report to the user's handler is made by the caller, outside the lock.
func (e *notifyEngine) writeFailing(ctx context.Context, ev notifyEvent, name string) int {
	unlock, lerr := lockTrail(ctx, name)
	if lerr != nil {
		return 0
	}
	defer unlock()
	for range maxTrailRetries {
		recs, err := e.trail.Load(ctx, name)
		if err != nil {
			return 0
		}
		es := FoldNotify(recs).Events[ev.key]
		if es == nil || !es.Open() || es.Failing != nil || es.Rounds < e.failingRounds() {
			return 0
		}
		rec := NotifyRecord{Kind: NotifyFailing, Key: ev.key, RunID: ev.runID, At: e.cfg.now(), Failures: es.Rounds}
		conflict, err := e.appendRec(ctx, name, len(recs), rec)
		if err != nil {
			return 0
		}
		if !conflict {
			return es.Rounds
		}
	}
	return 0
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
	case NotificationRunAborted, NotificationResolved:
		return true, "", nil
	case NotificationEscalated:
		// A step stands while the event it escalates does: it is not acknowledged by a receiver
		// being told, and a chain ends only when the event stops being current.
		o := st.Events[ev.origKey]
		if o == nil || o.Cleared != nil {
			return false, "resolved", nil
		}
		base, ok := eventFromKey(o)
		if !ok {
			return true, "", nil
		}
		return e.current(ctx, base, st)
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
	if ev.halt != nil {
		result := ev.halt.Op.ID
		if ev.halt.Op.Kind == OpTool {
			result = ToolResultStep(ev.halt.Op.ID)
		}
		if ok, err := e.hasRecord(ctx, opRun, result); err != nil || ok {
			return false, "resolved", err
		}
		return true, "", nil
	}
	if ev.approval != nil {
		decided, err := e.gateDecided(ctx, opRun, ev.approval.ToolUseID, ev.approval.Reask)
		if err != nil || decided {
			if err == nil && e.gateExpired(ctx, opRun, ev.approval.ToolUseID) {
				return false, "expired", nil
			}
			return false, "decided", err
		}
	}
	return true, "", nil
}

// gateExpired reports whether the gate of call on run ended in its expiry: the gate's expiry
// record, or a decision recorded after its deadline, for a one-decision gate; an expired tally
// (any of its steps); or the call's result the model reads for an expiry. A read error counts as
// not expired: the all-clear then says "decided", which is also true.
func (e *notifyEngine) gateExpired(ctx context.Context, run, call string) bool {
	if r, ok, err := e.j.Get(ctx, run, ToolResultStep(call)); err == nil && ok && string(r.Result) == ExpiredResult {
		return true
	}
	for _, name := range []string{ApprovalTallyStep(call), ApprovalRetallyStep(call), ApprovalSoDRetallyStep(call)} {
		if r, ok, err := e.j.Get(ctx, run, name); err == nil && ok {
			if t, err := decodeTally(run, r); err == nil && t.Expired {
				return true
			}
		}
	}
	var expiresAt time.Time
	dl, hasDeadline, err := e.j.Get(ctx, run, ApprovalDeadlineStep(call))
	if err == nil && hasDeadline {
		_, expiresAt, hasDeadline = decodeDeadline(dl, call)
	}
	for _, name := range []string{approvalStep(call), approvalAfterRequestStep(call)} {
		if r, ok, err := e.j.Get(ctx, run, name); err == nil && ok && r.Approver() == "" && singleExpired(r, expiresAt, hasDeadline) {
			return true
		}
	}
	return false
}

func (e *notifyEngine) hasRecord(ctx context.Context, run, name string) (bool, error) {
	_, ok, err := e.j.Get(ctx, run, name)
	return ok, err
}

// gateDecided reports whether the gate of call on run has what the gate itself reads: a single
// approver's decision (approval:<call>), an m-of-n gate's terminal tally (approval-tally:<call>),
// the tool's result, or, for a plan flow's node whose key is call, the node's own result.
//
// For a re-ask (reask set), the gate's re-tally or the call's result instead: the recorded tally it
// asks again over is what it does not act on.
func (e *notifyEngine) gateDecided(ctx context.Context, run, call, reask string) (bool, error) {
	names := gateDecidedNames(call)
	if reask != "" {
		names = reaskDecidedNames(call, reask == PendingSoDReapproval)
	}
	for _, name := range names {
		if ok, err := e.hasRecord(ctx, run, name); err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

// gateDecidedNames are the journal names whose presence ends the gate of call.
func gateDecidedNames(call string) []string {
	return []string{approvalStep(call), approvalAfterRequestStep(call), ApprovalTallyStep(call), ToolResultStep(call), call}
}

// reaskDecidedNames are the records that end a re-ask of call's gate (see ApprovalReaskStep): its
// re-tally, or the call's result.
func reaskDecidedNames(call string, sod bool) []string {
	re := ApprovalRetallyStep(call)
	if sod {
		re = ApprovalSoDRetallyStep(call)
	}
	return []string{re, ToolResultStep(call), call}
}
