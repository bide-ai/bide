// pause.go groups the runtime's durable-pause primitives: human-in-the-loop
// (Interrupt/Resume), durable timers (Sleep/WaitUntil) and the Waker that fires them, and
// signals (Signal/Await) that deliver external events into a run. They share one mechanism:
// a named durable step (Durable.Do) plus a typed pause error the agent loop propagates, so a
// paused run resumes deterministically after a crash and each pause resolves at most once.

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ===========================================================================
// Human-in-the-loop: Interrupt / Resume
// ===========================================================================

// Interrupted is returned by Run when a tool called Interrupt and no resume value has
// been recorded for that key yet. The run has paused durably at the interrupt point.
// Inspect Prompt to decide what to ask the human, record an answer with Resume (same
// Key), then re-invoke Run with the same runID to continue.
type Interrupted struct {
	RunID string
	// RootRunID is the run to re-invoke to continue: the top-level run. It differs from RunID
	// when the signal comes from inside a sub-agent, whose journal is RunID. Record the answer
	// against RunID (Resume, Approve, ResolveHalt, Signal), then run RootRunID with the root agent.
	RootRunID string
	Key       string
	Prompt    any // caller-defined payload for the human: a question, options, current state
}

func (e *Interrupted) Error() string {
	return fmt.Sprintf("run %s interrupted at %q awaiting input", e.RunID, e.Key)
}

// Interrupt pauses the current run to request typed human input, identified by key. Call
// it from inside a tool (the agent loop supplies the run context). On first encounter it
// returns the zero T and an *Interrupted error that propagates out of Run, pausing the
// run durably. After Resume records a value for the same key and Run is re-invoked,
// Interrupt returns that value and execution continues past this point. This generalizes
// approve/deny (a bool) to an arbitrary typed answer.
//
// Interrupt must be called from a retry-safe tool (Safety.ReadOnly or Idempotent): on
// resume the tool re-runs from the top until the interrupt resolves, so everything
// before the Interrupt call must be safe to repeat. Use distinct keys for multiple
// interrupt points; each pauses and resumes independently.
func Interrupt[T any](ctx context.Context, key string, prompt any) (T, error) {
	var zero T
	d, runID, ok := runContext(ctx)
	if !ok {
		return zero, fmt.Errorf("agent: Interrupt called outside a running agent: %w", ErrConfig)
	}
	recs, err := d.History(ctx, runID)
	if err != nil {
		return zero, fmt.Errorf("agent: interrupt %q: %w (%w)", key, err, ErrStorage)
	}
	name := interruptStep(key)
	for _, r := range recs {
		if r.Kind == StepValue && r.Name == name {
			var v T
			if len(r.Result) > 0 {
				if err := json.Unmarshal(r.Result, &v); err != nil {
					return zero, fmt.Errorf("agent: decode resume value for %q: %w (%w)", key, err, ErrProtocol)
				}
			}
			return v, nil
		}
	}
	return zero, &Interrupted{RunID: runID, RootRunID: rootRunID(ctx, runID), Key: key, Prompt: prompt}
}

// Resume records the typed value a paused run is waiting for at key (see Interrupt), then
// re-invoke Run with the same runID to continue. Idempotent: the first value for a
// (runID, key) wins. The value survives a crash: it is a journaled step.
func Resume[T any](ctx context.Context, d Durable, runID, key string, value T) error {
	if runID == "" {
		return fmt.Errorf("Resume: empty runID: %w", ErrConfig)
	}
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("agent: encode resume value for %q: %w (%w)", key, err, ErrConfig)
	}
	_, err = d.Do(ctx, runID, interruptStep(key), func(context.Context) (Record, error) {
		return Record{Kind: StepValue, Result: b}, nil
	})
	return err
}

func interruptStep(key string) string { return "interrupt:" + key }

// ===========================================================================
// Durable timers: Sleep / WaitUntil
// ===========================================================================

// Sleeping is returned by Run when a tool called Sleep or WaitUntil and the wake time has not yet
// passed. The run has paused durably at the timer: its wake time is journaled, so the pause
// survives a restart. Re-invoke Run with the same runID at or after FireAt to resume (a Waker does
// this automatically; otherwise the deployment re-invokes on its own schedule).
type Sleeping struct {
	RunID string
	// RootRunID is the run to re-invoke to continue: the top-level run. It differs from RunID
	// when the signal comes from inside a sub-agent, whose journal is RunID. Record the answer
	// against RunID (Resume, Approve, ResolveHalt, Signal), then run RootRunID with the root agent.
	RootRunID string
	Name      string
	FireAt    time.Time
}

func (e *Sleeping) Error() string {
	return fmt.Sprintf("run %s sleeping at %q until %s", e.RunID, e.Name, e.FireAt.Format(time.RFC3339))
}

type clockKey struct{}

// WithClock binds a clock to ctx for the durable timer to read "now". Deployments leave it unset
// (defaulting to time.Now); tests inject a controllable clock to advance time deterministically.
func WithClock(ctx context.Context, now func() time.Time) context.Context {
	return context.WithValue(ctx, clockKey{}, now)
}

func clockFrom(ctx context.Context) func() time.Time {
	if now, ok := ctx.Value(clockKey{}).(func() time.Time); ok && now != nil {
		return now
	}
	return time.Now
}

// Sleep pauses the current run until d has elapsed from the FIRST time this timer was reached,
// durably. Call it from inside a retry-safe tool (the agent loop supplies the run context). On the
// first encounter it journals the wake time (now + d) and returns *Sleeping, pausing the run; on a
// later resume it returns nil once the wake time has passed, and continues past this point. The
// wake time is fixed on the first call and memoized, so a resumed or crash-recovered run waits to
// the same absolute instant rather than restarting the clock. Use distinct names for distinct
// timers. Sleep requires a retry-safe tool (Safety.ReadOnly or Idempotent), like Interrupt.
func Sleep(ctx context.Context, name string, d time.Duration) error {
	return waitUntil(ctx, name, func(now time.Time) time.Time { return now.Add(d) })
}

// WaitUntil pauses the current run until the absolute time `until`, durably. It is the deadline form
// of Sleep: same semantics, but the wake time is the given instant rather than a relative delay.
func WaitUntil(ctx context.Context, name string, until time.Time) error {
	return waitUntil(ctx, name, func(time.Time) time.Time { return until })
}

func waitUntil(ctx context.Context, name string, fireAtFrom func(now time.Time) time.Time) error {
	d, runID, ok := runContext(ctx)
	if !ok {
		return fmt.Errorf("agent: Sleep called outside a running agent: %w", ErrConfig)
	}
	now := clockFrom(ctx)

	// Journal the wake time once (at-most-once by name), so it is stable across resume and restart.
	rec, err := d.Do(ctx, runID, timerStep(name), func(context.Context) (Record, error) {
		b, err := json.Marshal(fireAtFrom(now()))
		if err != nil {
			return Record{}, fmt.Errorf("agent: encode wake time for %q: %w (%w)", name, err, ErrConfig)
		}
		return Record{Kind: StepValue, Result: b}, nil
	})
	if err != nil {
		return fmt.Errorf("agent: timer %q: %w (%w)", name, err, ErrStorage)
	}
	var fireAt time.Time
	if err := json.Unmarshal(rec.Result, &fireAt); err != nil {
		return fmt.Errorf("agent: decode wake time for %q: %w (%w)", name, err, ErrProtocol)
	}

	if !now().Before(fireAt) {
		return nil // due: the wait is over, continue
	}
	// Not yet due: schedule a wake if a Waker is bound, then pause durably.
	if w := wakerFrom(ctx); w != nil {
		// Wake the top-level run: re-running it re-enters any sub-agent down to this Sleep,
		// while the sub-run alone cannot be driven by the root agent's resume callback.
		w.Schedule(rootRunID(ctx, runID), name, fireAt)
	}
	return &Sleeping{RunID: runID, RootRunID: rootRunID(ctx, runID), Name: name, FireAt: fireAt}
}

func timerStep(name string) string { return "timer:" + name }

// ===========================================================================
// Waker: the pluggable wake trigger
// ===========================================================================

// Waker is the pluggable trigger that re-invokes a sleeping run when its durable timer is due. Sleep
// registers a wake with the Waker bound to the run's context (WithWaker); the Waker later calls back
// to resume the run. The SDK provides the durable, at-most-once timer and its resume safety; what
// re-invokes the run at the wake time is deployment policy (an in-process loop, a cron, a queue),
// exactly as the inbound trigger for an event-driven run is (see docs/guides/messaging.md). MemWaker is the
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
//
// A resume that fails (the model provider or the store was briefly unavailable) leaves the run where
// it was, so its due timers are put back and the next Fire retries it; without that, one transient
// failure would leave the run asleep with nothing to wake it. A resume that ends in a durable pause
// (the run slept again, or awaits approval, an interrupt, a signal, or a halt resolution) is not
// retried: the run is waiting on something else, and waking it every tick would only spin.
func (w *MemWaker) Fire(ctx context.Context, now time.Time) (int, error) {
	type dueTimer struct {
		key string
		s   scheduled
	}
	w.mu.Lock()
	var runIDs []string
	due := map[string][]dueTimer{}
	for key, s := range w.timers {
		if !now.Before(s.fireAt) {
			delete(w.timers, key)
			if _, ok := due[s.runID]; !ok {
				runIDs = append(runIDs, s.runID)
			}
			due[s.runID] = append(due[s.runID], dueTimer{key, s})
		}
	}
	w.mu.Unlock()

	var errs []error
	for _, runID := range runIDs {
		err := w.resume(ctx, runID)
		if err == nil {
			continue
		}
		errs = append(errs, err)
		if isPause(err) {
			continue
		}
		w.mu.Lock()
		for _, t := range due[runID] {
			if _, rescheduled := w.timers[t.key]; !rescheduled {
				w.timers[t.key] = t.s
			}
		}
		w.mu.Unlock()
	}
	return len(runIDs), errors.Join(errs...)
}

// Start runs Fire on a ticker every `every` until ctx is cancelled, so sleeping runs wake on their
// own. Resume errors go to onError if non-nil. It is the turnkey local loop; a production deployment
// may prefer an external scheduler that owns the trigger and durable timer set.
//
// The returned channel closes once the loop has stopped, after any Fire in flight when ctx was
// cancelled has returned. Wait on it before closing the store the resumed runs write to.
func (w *MemWaker) Start(ctx context.Context, every time.Duration, onError func(error)) <-chan struct{} {
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
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
	return stopped
}

// ===========================================================================
// Signals: external events delivered into a run
// ===========================================================================

// Awaiting is returned by Run when a tool called Await and no signal has been delivered for
// that name yet. The run has paused durably at the await point. Deliver a signal with
// Signal (same name), then re-invoke Run with the same runID to continue. Awaiting is the
// externally-pushed dual of Interrupted: Interrupt asks a human and resumes with their
// answer; Await waits for an event an outside system delivers.
type Awaiting struct {
	RunID string
	// RootRunID is the run to re-invoke to continue: the top-level run. It differs from RunID
	// when the signal comes from inside a sub-agent, whose journal is RunID. Record the answer
	// against RunID (Resume, Approve, ResolveHalt, Signal), then run RootRunID with the root agent.
	RootRunID string
	Name      string
	Prompt    any // optional caller payload describing what the run is waiting for
}

func (e *Awaiting) Error() string {
	return fmt.Sprintf("run %s awaiting signal %q", e.RunID, e.Name)
}

// Await blocks the current run until a single-shot signal named `name` is delivered, then
// returns its payload. Call it from inside a tool (the agent loop supplies the run context).
// On first encounter, with no signal recorded, it returns the zero T and an *Awaiting error
// that propagates out of Run, pausing the run durably. After Signal records a payload for
// the same name and Run is re-invoked, Await returns that payload and execution continues
// past this point. The payload survives a crash: it is a journaled step.
//
// Await is the externally-pushed counterpart of Interrupt. Like Interrupt and Sleep it must
// be called from a retry-safe tool (Safety.ReadOnly or Idempotent): on resume the tool
// re-runs from the top until the await resolves, so everything before the Await call must be
// safe to repeat. Use distinct names for distinct awaits; each pauses and resolves
// independently.
func Await[T any](ctx context.Context, name string) (T, error) {
	var zero T
	d, runID, ok := runContext(ctx)
	if !ok {
		return zero, fmt.Errorf("agent: Await called outside a running agent: %w", ErrConfig)
	}
	recs, err := d.History(ctx, runID)
	if err != nil {
		return zero, fmt.Errorf("agent: await %q: %w (%w)", name, err, ErrStorage)
	}
	step := signalStep(name)
	for _, r := range recs {
		if r.Kind == StepSignal && r.Name == step {
			var v T
			if len(r.Result) > 0 {
				if err := json.Unmarshal(r.Result, &v); err != nil {
					return zero, fmt.Errorf("agent: decode signal %q: %w (%w)", name, err, ErrProtocol)
				}
			}
			return v, nil
		}
	}
	return zero, &Awaiting{RunID: runID, RootRunID: rootRunID(ctx, runID), Name: name}
}

// Signal delivers a single-shot signal to a run, journaled at-most-once by name: a
// redelivery (a retried webhook, an at-least-once queue) is a no-op and the first payload
// wins. This turns at-least-once transport into exactly-once application to the run. Safe to
// call from any process; the store's primary-key / ON CONFLICT is the cross-process dedup.
//
// Signal only records the payload. After delivering, re-invoke Run with the same runID to
// resume the awaiting run: directly, or via a Waker scheduled at the current time.
func Signal[T any](ctx context.Context, d Durable, runID, name string, payload T) error {
	if runID == "" {
		return fmt.Errorf("Signal: empty runID: %w", ErrConfig)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("agent: encode signal %q: %w (%w)", name, err, ErrConfig)
	}
	_, err = d.Do(ctx, runID, signalStep(name), func(context.Context) (Record, error) {
		return Record{Kind: StepSignal, Result: b}, nil
	})
	return err
}

func signalStep(name string) string { return "signal:" + name }
