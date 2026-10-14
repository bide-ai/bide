package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// notifyWorker delivers a notifyEngine's events on goroutines of its own, so a slow sink never
// delays Run. It holds the events in memory only; the trail is what makes them durable, and a
// RecoverLoop pass or NotifyPending finds again any event a dead process had not recorded.
//
// A dispatcher goroutine runs while any event is queued or in flight, and exits when none is (the
// next submit starts it again), so an idle notifier holds no goroutine. Each event's round runs on a
// goroutine of its own, at most maxInflight at once, so one event whose sink hangs (a call is
// bounded by the engine, but a Notifier may ignore its context) does not hold up the others.
type notifyWorker struct {
	e *notifyEngine

	mu       sync.Mutex
	pending  map[string]*pendingNotify
	inflight int
	running  bool
	stopping bool // flush has asked the dispatcher to stop
	restart  bool // an event was queued while it was stopping
	wake     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	cancel   context.CancelFunc
}

type pendingNotify struct {
	ev       notifyEvent
	due      time.Time
	inflight bool
	lastErr  error // the error of the last round, nil if it ended cleanly
	deferred bool  // lastErr is a deferral to another deliverer, not a failure of this engine's
}

func newNotifyWorker(e *notifyEngine) *notifyWorker {
	return &notifyWorker{e: e, pending: map[string]*pendingNotify{}, wake: make(chan struct{}, 1)}
}

// signal wakes the dispatcher.
func (w *notifyWorker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// submit queues ev for delivery. An event already queued, or in flight, is not queued twice.
func (w *notifyWorker) submit(ev notifyEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.pending[ev.key]; !ok {
		w.pending[ev.key] = &pendingNotify{ev: ev, due: w.e.cfg.now()}
	}
	switch {
	case !w.running:
		w.startLocked()
	case w.stopping:
		w.restart = true // the dispatcher is on its way out: it starts another
	}
	w.signal()
}

// startLocked starts the dispatcher. The caller holds w.mu.
func (w *notifyWorker) startLocked() {
	ctx, cancel := context.WithCancel(context.Background())
	w.running, w.stopping, w.restart = true, false, false
	w.cancel = cancel
	w.stop, w.done = make(chan struct{}), make(chan struct{})
	go w.loop(ctx, w.stop, w.done)
}

// idle reports whether nothing is queued or in flight.
func (w *notifyWorker) idle() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.pending) == 0
}

func (w *notifyWorker) loop(ctx context.Context, stop, done chan struct{}) {
	defer func() {
		w.exited()
		close(done)
	}()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		default:
		}
		w.mu.Lock()
		if len(w.pending) == 0 {
			w.stopping = true // idle: leaving. A submit before the deferred exit sets restart.
			w.mu.Unlock()
			return
		}
		now := w.e.cfg.now()
		wait := time.Duration(-1)
		for _, p := range w.pending {
			if p.inflight {
				continue
			}
			if d := p.due.Sub(now); d > 0 {
				if wait < 0 || d < wait {
					wait = d
				}
				continue
			}
			if w.inflight >= w.e.cfg.maxInflight {
				continue
			}
			p.inflight = true
			w.inflight++
			go w.runRound(ctx, p)
		}
		w.mu.Unlock()

		var timer <-chan time.Time
		var t *time.Timer
		if wait >= 0 {
			t = time.NewTimer(wait)
			timer = t.C
		}
		select {
		case <-w.wake:
		case <-timer:
		case <-stop:
		case <-ctx.Done():
		}
		if t != nil {
			t.Stop()
		}
	}
}

// exited records that the dispatcher is gone, and starts another if an event was queued while it
// was leaving.
func (w *notifyWorker) exited() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.running, w.stopping = false, false
	if w.restart && len(w.pending) > 0 {
		w.startLocked()
	}
	w.restart = false
}

// runRound runs one round of p's event and records its outcome.
func (w *notifyWorker) runRound(ctx context.Context, p *pendingNotify) {
	ev := p.ev
	var res roundResult
	var err error
	if ev.keyFn != nil {
		var k string
		if k, err = ev.keyFn(ctx); err == nil {
			ev.key, ev.keyFn = k, nil
		} else {
			err = fmt.Errorf("notify: %s: its key could not be computed (tried again later): %w", p.ev.key, err)
			res.next = w.e.cfg.now().Add(w.e.backoff(1))
		}
	}
	if err == nil {
		res, err = w.e.round(ctx, ev)
	}
	if err != nil && ctx.Err() == nil {
		w.e.report(err)
	}
	w.mu.Lock()
	w.inflight--
	p.inflight = false
	p.lastErr = err
	p.deferred = false
	if err == nil && !res.done {
		p.lastErr, p.deferred = res.deferred, res.deferred != nil
	}
	if res.done {
		delete(w.pending, p.ev.key)
	} else {
		p.due = res.next
		if p.due.IsZero() {
			p.due = w.e.cfg.now().Add(w.e.backoff(1))
		}
	}
	w.mu.Unlock()
	w.signal()
}

// settled reports whether no event is in flight or due: what remains is waiting out a backoff.
func (w *notifyWorker) settled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inflight > 0 {
		return false
	}
	now := w.e.cfg.now()
	for _, p := range w.pending {
		if !p.due.After(now) {
			return false
		}
	}
	return true
}

func (w *notifyWorker) isRunning() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running
}

// undelivered returns the last error of every event still queued: what flush could not deliver. An
// event whose last round only deferred to another deliverer is looked up in the trail first, and
// dropped if that deliverer has acknowledged (or cleared) it since.
func (w *notifyWorker) undelivered(ctx context.Context) error {
	type item struct {
		err      error
		ev       notifyEvent
		deferred bool
	}
	w.mu.Lock()
	var items []item
	for _, p := range w.pending {
		if p.lastErr != nil {
			items = append(items, item{p.lastErr, p.ev, p.deferred})
		}
	}
	w.mu.Unlock()
	// One budget for the whole recheck, or the caller's deadline if sooner: a hung trail must not
	// hold the flush for 2s per deferred event. Once it ends the rest are reported as they are.
	lctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var errs []error
	for _, it := range items {
		if it.deferred && lctx.Err() == nil {
			recs, err := w.e.trail.Load(lctx, NotifyTrailName(it.ev.runID))
			if err == nil {
				key := it.ev.key
				if es := FoldNotify(recs).Events[key]; es != nil && !es.Open() {
					continue // the deliverer it deferred to has finished
				}
			}
		}
		errs = append(errs, it.err)
	}
	return errors.Join(errs...)
}

// flush waits until every event that is in flight or due has had its round, then stops the
// dispatcher. Events waiting out a backoff are left for a later process, and their last errors are
// what it returns, so a caller that exits after it knows what was not delivered. If ctx ends first
// it cancels the rounds in flight (a Notifier that honors its context returns) and returns ctx's
// error without waiting for one that does not.
func (e *notifyEngine) flush(ctx context.Context) error {
	err := e.flushDeliveries(ctx)
	// The deliveries have written their last records: have a trail that signs them sign them now,
	// within ctx, so a process that stops cleanly leaves nothing unattested. A failure is reported,
	// not returned: it is not an undelivered event.
	e.anchorTrails(ctx)
	return err
}

func (e *notifyEngine) flushDeliveries(ctx context.Context) error {
	w := e.w
	for w.isRunning() && !w.settled() {
		select {
		case <-ctx.Done():
			w.mu.Lock()
			cancel := w.cancel
			w.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			return errors.Join(ctx.Err(), w.undelivered(ctx))
		case <-time.After(2 * time.Millisecond):
		}
	}
	w.mu.Lock()
	if w.running {
		w.stopping = true
		select {
		case <-w.stop:
		default:
			close(w.stop)
		}
	}
	done, cancel := w.done, w.cancel
	running := w.running
	w.mu.Unlock()
	if running {
		select {
		case <-done:
		case <-ctx.Done():
			cancel()
			return errors.Join(ctx.Err(), w.undelivered(ctx))
		}
	}
	return w.undelivered(ctx)
}
