package agent

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Waker is the pluggable trigger that re-invokes a sleeping run when its durable timer is due. Sleep
// registers a wake with the Waker bound to the run's context (WithWaker); the Waker later calls back
// to resume the run. The SDK provides the durable, at-most-once timer and its resume safety; what
// re-invokes the run at the wake time is deployment policy (an in-process loop, a cron, a queue),
// exactly as the inbound trigger for an event-driven run is (see docs/MESSAGING.md). MemWaker is the
// reference in-process implementation.
type Waker interface {
	// Schedule registers that runID should be resumed at fireAt, idempotent per (runID, name).
	Schedule(runID, name string, fireAt time.Time)
}

type wakerKey struct{}

// WithWaker binds a Waker to ctx so a durable Sleep registers its wake automatically. With no Waker
// bound, Sleep still pauses durably; the deployment is then responsible for re-invoking the run at
// or after the wake time on its own schedule.
func WithWaker(ctx context.Context, w Waker) context.Context {
	return context.WithValue(ctx, wakerKey{}, w)
}

func wakerFrom(ctx context.Context) Waker {
	w, _ := ctx.Value(wakerKey{}).(Waker)
	return w
}

// MemWaker is an in-process Waker: it holds pending timers and, when Fire is called with the
// current time (or via a Start ticker), resumes every run whose timer is due by calling the resume
// function it was built with. resume is how the deployment re-invokes a run, typically
// agent.Run(ctx, runID, savedInput); a resumed run whose wait is over proceeds, and one that sleeps
// again re-registers a new wake here. MemWaker is a reference and a local-dev default, not a
// durable scheduler: a process exit loses its in-memory timer set, so the wake times must also live
// in the durable journal (they do, Sleep journals them), and a restarted deployment rebuilds
// pending wakes by scanning runs, or an external scheduler owns the trigger.
type MemWaker struct {
	mu     sync.Mutex
	timers map[string]scheduled
	resume func(ctx context.Context, runID string) error
}

type scheduled struct {
	runID  string
	fireAt time.Time
}

// NewMemWaker builds an in-process waker that resumes due runs via resume.
func NewMemWaker(resume func(ctx context.Context, runID string) error) *MemWaker {
	return &MemWaker{timers: map[string]scheduled{}, resume: resume}
}

// Schedule registers a wake, idempotent per (runID, name): re-scheduling the same timer overwrites
// its fire time rather than adding a duplicate.
func (w *MemWaker) Schedule(runID, name string, fireAt time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.timers[runID+"\x00"+name] = scheduled{runID: runID, fireAt: fireAt}
}

// Fire resumes every run that has a timer due at or before now, each run once even if several of its
// timers are due, drops the fired timers, and returns how many runs it resumed. Resume callbacks run
// without the lock held, so a resumed run may register a new wake. Errors from resume are joined.
func (w *MemWaker) Fire(ctx context.Context, now time.Time) (int, error) {
	w.mu.Lock()
	var runIDs []string
	seen := map[string]bool{}
	for key, s := range w.timers {
		if !now.Before(s.fireAt) {
			delete(w.timers, key)
			if !seen[s.runID] {
				seen[s.runID] = true
				runIDs = append(runIDs, s.runID)
			}
		}
	}
	w.mu.Unlock()

	var errs []error
	for _, runID := range runIDs {
		if err := w.resume(ctx, runID); err != nil {
			errs = append(errs, err)
		}
	}
	return len(runIDs), errors.Join(errs...)
}

// Start runs Fire on a ticker every `every` until ctx is cancelled, so sleeping runs wake on their
// own. Resume errors go to onError if non-nil. It is the turnkey local loop; a production deployment
// may prefer an external scheduler that owns the trigger and durable timer set.
func (w *MemWaker) Start(ctx context.Context, every time.Duration, onError func(error)) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := w.Fire(ctx, time.Now()); err != nil && onError != nil {
					onError(err)
				}
			}
		}
	}()
}
