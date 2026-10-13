package agent

import (
	"context"
	"sync"
	"time"
)

// notifyWorker delivers a notifyEngine's events on a goroutine of its own, so a slow sink never
// delays Run. It holds the events in memory only; the trail is what makes them durable, and a
// RecoverLoop pass or NotifyPending finds again any event a dead process had not recorded. Rounds
// run one at a time.
type notifyWorker struct {
	e *notifyEngine

	mu      sync.Mutex
	pending map[string]*pendingNotify
	running bool
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	cancel  context.CancelFunc
}

type pendingNotify struct {
	ev       notifyEvent
	due      time.Time
	inflight bool
}

func newNotifyWorker(e *notifyEngine) *notifyWorker {
	return &notifyWorker{e: e, pending: map[string]*pendingNotify{}, wake: make(chan struct{}, 1)}
}

// submit queues ev for delivery. An event already queued, or in flight, is not queued twice.
func (w *notifyWorker) submit(ev notifyEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.pending[ev.key]; !ok {
		w.pending[ev.key] = &pendingNotify{ev: ev, due: w.e.cfg.now()}
	}
	if !w.running { // also restarts a worker FlushNotifications stopped with events still queued
		ctx, cancel := context.WithCancel(context.Background())
		w.running, w.cancel = true, cancel
		w.stop, w.done = make(chan struct{}), make(chan struct{})
		go w.loop(ctx, w.stop, w.done)
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// idle reports whether nothing is queued or in flight.
func (w *notifyWorker) idle() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.pending) == 0
}

func (w *notifyWorker) loop(ctx context.Context, stop, done chan struct{}) {
	defer func() {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
		close(done)
	}()
	for {
		select {
		case <-stop:
			return
		default:
		}
		w.mu.Lock()
		var next *pendingNotify
		for _, p := range w.pending {
			if !p.inflight && (next == nil || p.due.Before(next.due)) {
				next = p
			}
		}
		var wait time.Duration = -1
		if next != nil {
			if d := next.due.Sub(w.e.cfg.now()); d > 0 {
				wait, next = d, nil
			} else {
				next.inflight = true
			}
		}
		w.mu.Unlock()

		if next == nil {
			var timer <-chan time.Time
			if wait >= 0 {
				t := time.NewTimer(wait)
				defer t.Stop()
				timer = t.C
			}
			select {
			case <-w.wake:
			case <-timer:
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
			continue
		}

		res, err := w.e.round(ctx, next.ev)
		w.e.report(err)
		w.mu.Lock()
		if res.done {
			delete(w.pending, next.ev.key)
		} else {
			next.inflight = false
			next.due = res.next
			if next.due.IsZero() {
				next.due = w.e.cfg.now().Add(w.e.backoff(1))
			}
		}
		w.mu.Unlock()
	}
}

func (w *notifyWorker) isRunning() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running
}

// settled reports whether no event is in flight or due: what remains is waiting out a backoff.
func (w *notifyWorker) settled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.e.cfg.now()
	for _, p := range w.pending {
		if p.inflight || !p.due.After(now) {
			return false
		}
	}
	return true
}

// flush waits until every event that is in flight or due has had its round, then stops the worker.
// Events waiting out a backoff are left for a later process. It cancels the round if ctx ends first.
func (e *notifyEngine) flush(ctx context.Context) error {
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
			return ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return nil
	}
	stop, done, cancel := w.stop, w.done, w.cancel
	select {
	case <-stop:
	default:
		close(stop)
	}
	w.mu.Unlock()
	<-done
	cancel()
	return nil
}
