// pause.go groups the runtime's durable-pause primitives: the sealed Pause contract every
// pause error satisfies, human-in-the-loop (Interrupt/AnswerInterrupt), durable timers
// (Sleep/WaitUntil) and the Waker that fires them, and signals (Signal/Await) that deliver
// external events into a run. They share one mechanism: a named journal step
// plus a typed pause error the agent loop propagates, so a paused run resumes
// deterministically after a crash and each pause resolves at most once.

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
// The pause contract
// ===========================================================================

// Pause is the error a run returns when it has stopped durably and is waiting on something
// outside it: a human's approval (*ApprovalPending), an answer to an interrupt
// (*InterruptPending), a signal or a channel message (*SignalPending), a durable timer
// (*TimerPending), or a verdict on a side effect whose outcome is unknown (*OutcomeUnknown).
// A pause is not a failure: the run's journal is intact, and once the condition is met the
// run is re-invoked with Paused().RootRunID and carries on.
//
// The set is sealed: only this package's five types implement Pause, so a type switch over
// them is exhaustive. Test for a pause with IsPause and read it with AsPause, which see
// through wrapping.
type Pause interface {
	error
	// Paused names the paused run: RunID is the journal the pause lives in (a sub-agent's
	// sub-run for a pause raised inside a sub-agent) and RootRunID is the run to re-invoke.
	Paused() RunRef
	pause() // declared on each concrete type, never on RunRef, so embedding RunRef does not satisfy Pause
}

// RunRef names a paused run. RunID is the journal the pause lives in: record the answer
// against it (Approve, AnswerInterrupt, Signal, ResolveHalt). RootRunID is the top-level
// run to re-invoke to continue; it differs from RunID when the pause comes from inside a
// sub-agent, whose journal is RunID.
type RunRef struct {
	RunID     string
	RootRunID string
}

// Paused returns r. Every pause type embeds a RunRef and so reports it through Pause.
func (r RunRef) Paused() RunRef { return r }

// IsPause reports whether err, or any error it wraps, is a Pause: the run stopped durably
// and is waiting, rather than failed.
func IsPause(err error) bool {
	_, ok := AsPause(err)
	return ok
}

// AsPause returns the first Pause in err's chain, if there is one.
func AsPause(err error) (Pause, bool) {
	var p Pause
	if errors.As(err, &p) {
		return p, true
	}
	return nil, false
}

// ===========================================================================
// Human-in-the-loop: Interrupt / AnswerInterrupt
// ===========================================================================

// InterruptPending is returned by Run when a tool called Interrupt and no answer has been
// recorded for that name yet. The run has paused durably at the interrupt point. Inspect
// Prompt to decide what to ask the human, record an answer with AnswerInterrupt (same RunID
// and Name), then re-invoke Run with RootRunID to continue.
type InterruptPending struct {
	RunRef
	Name   string
	Prompt any // caller-defined payload for the human: a question, options, current state
}

// Error names the run and the interrupt point.
func (e *InterruptPending) Error() string {
	return fmt.Sprintf("run %s interrupted at %q awaiting input", e.RunID, e.Name)
}

func (*InterruptPending) pause() {}

// Interrupt pauses the current run to request typed human input, identified by name. Call
// it from inside a tool (the agent loop supplies the run context). On first encounter it
// returns the zero T and an *InterruptPending error that propagates out of Run, pausing the
// run durably. After AnswerInterrupt records a value for the same name and Run is
// re-invoked, Interrupt returns that value and execution continues past this point. This
// generalizes approve/deny (a bool) to an arbitrary typed answer.
//
// Interrupt must be called from a retry-safe tool (Safety.ReadOnly or Idempotent): on
// resume the tool re-runs from the top until the interrupt resolves, so everything
// before the Interrupt call must be safe to repeat. Use distinct names for multiple
// interrupt points; each pauses and resumes independently.
func Interrupt[T any](ctx context.Context, name string, prompt any) (T, error) {
	var zero T
	d, runID, ok := runContext(ctx)
	if !ok {
		return zero, fmt.Errorf("agent: Interrupt called outside a running agent: %w", ErrConfig)
	}
	recs, err := d.History(ctx, runID)
	if err != nil {
		return zero, fmt.Errorf("agent: interrupt %q: %w (%w)", name, err, ErrStorage)
	}
	step := interruptStep(name)
	for _, r := range recs {
		if r.Kind == StepValue && r.Name == step {
			var v T
			if len(r.Result) > 0 {
				if err := json.Unmarshal(r.Result, &v); err != nil {
					return zero, fmt.Errorf("agent: decode interrupt answer for %q: %w (%w)", name, err, ErrProtocol)
				}
			}
			return v, nil
		}
	}
	return zero, &InterruptPending{RunRef: RunRef{RunID: runID, RootRunID: rootRunID(ctx, runID)}, Name: name, Prompt: prompt}
}

// AnswerInterrupt records the typed value a paused run is waiting for at the interrupt
// point name (see Interrupt); then re-invoke Run with the pause's RootRunID to continue.
// It is idempotent: the first value for a (runID, name) wins. The value survives a crash:
// it is a journaled step.
func (j *Journal) AnswerInterrupt[T any](ctx context.Context, runID, name string, value T) error {
	if runID == "" {
		return fmt.Errorf("AnswerInterrupt: empty runID: %w", ErrConfig)
	}
	b, err := marshalJournal(value)
	if err != nil {
		return fmt.Errorf("agent: encode interrupt answer for %q: %w (%w)", name, err, ErrConfig)
	}
	_, err = j.do(ctx, runID, interruptStep(name), func(context.Context) (Record, error) {
		return Record{Kind: StepValue, Result: b}, nil
	})
	return err
}

func interruptStep(name string) string { return "interrupt:" + name }

// ===========================================================================
// Durable timers: Sleep, WaitUntil
// ===========================================================================

// TimerPending is returned by Run when a tool called Sleep or WaitUntil and the wake time has
// not yet passed. The run has paused durably at the timer: its wake time is journaled, so the
// pause survives a restart. Re-invoke Run with RootRunID at or after FireAt to resume (a Waker
// does this automatically; otherwise the deployment re-invokes on its own schedule).
type TimerPending struct {
	RunRef
	Name   string
	FireAt time.Time
}

// Error names the run, the timer, and its wake time.
func (e *TimerPending) Error() string {
	return fmt.Sprintf("run %s sleeping at %q until %s", e.RunID, e.Name, e.FireAt.Format(time.RFC3339))
}

func (*TimerPending) pause() {}

type clockKey struct{}

// contextWithClock binds a clock to ctx for the durable timers of the run driven with it to read
// "now". Deployments leave it unset (defaulting to time.Now); tests inject a controllable clock to
// advance time deterministically. A clock bound here takes precedence over the agent's WithClock
// option.
func contextWithClock(ctx context.Context, now func() time.Time) context.Context {
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
// first encounter it journals the wake time (now + d) and returns *TimerPending, pausing the run;
// on a later resume it returns nil once the wake time has passed, and continues past this point.
// The wake time is fixed on the first call and memoized, so a resumed or crash-recovered run waits
// to the same absolute instant rather than restarting the clock. Use distinct names for distinct
// timers. Sleep requires a retry-safe tool (Safety.ReadOnly or Idempotent), like Interrupt.
//
// With a Waker bound (the WithWaker option), Sleep schedules the wake before it pauses. If the Waker fails,
// Sleep returns an error wrapping ErrStorage instead of pausing: a pause with no wake scheduled
// could sleep forever. The run then fails and its tool call records nothing (the memoized wake
// time aside), so re-driving the run (RecoverLoop does, on its next pass) reaches this Sleep
// again and schedules again.
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
	rec, err := d.do(ctx, runID, timerStep(name), func(context.Context) (Record, error) {
		b, err := marshalJournal(fireAtFrom(now()))
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
	ref := RunRef{RunID: runID, RootRunID: rootRunID(ctx, runID)}
	if err := scheduleWake(ctx, Wake{RunID: ref.RunID, RootRunID: ref.RootRunID, Name: name, FireAt: fireAt}); err != nil {
		return err
	}
	return &TimerPending{RunRef: ref, Name: name, FireAt: fireAt}
}

func timerStep(name string) string { return "timer:" + name }

// ===========================================================================
// Waker: the pluggable wake trigger
// ===========================================================================

// Wake is one scheduled wake: re-invoke RootRunID at or after FireAt. RunID and Name identify
// the timer (a sub-agent's timer lives in its sub-run, RunID); the top-level run is what a
// deployment re-invokes, since re-running it re-enters any sub-agent down to the timer.
type Wake struct {
	RunID     string
	RootRunID string
	Name      string
	FireAt    time.Time
}

// Waker is the pluggable trigger that re-invokes a sleeping run when its durable timer is due. Sleep
// registers a wake with the run's Waker (WithWaker); the Waker later calls back
// to resume the run. The SDK provides the durable, at-most-once timer and its resume safety; what
// re-invokes the run at the wake time is deployment policy (an in-process loop, a cron, a queue),
// exactly as the inbound trigger for an event-driven run is (see docs/guides/messaging.md). MemWaker is the
// reference in-process implementation.
type Waker interface {
	// Schedule registers w, idempotent per (w.RunID, w.Name): scheduling the same timer again
	// replaces its fire time. An error means the wake may not be registered; the run then fails
	// with an error wrapping ErrStorage, records nothing, and schedules again when re-driven.
	Schedule(ctx context.Context, w Wake) error
}

type wakerKey struct{}

// contextWithWaker binds a Waker to ctx so a durable Sleep of the run driven with it registers its
// wake automatically. With no Waker bound (here or with the agent's WithWaker option), Sleep still
// pauses durably; the deployment is then responsible for re-invoking the run at or after the wake
// time on its own schedule. A Waker bound here takes precedence over the agent's.
func contextWithWaker(ctx context.Context, w Waker) context.Context {
	return context.WithValue(ctx, wakerKey{}, w)
}

func wakerFrom(ctx context.Context) Waker {
	w, _ := ctx.Value(wakerKey{}).(Waker)
	return w
}

// scheduleWake registers w with the Waker bound to ctx, if any. A failure is a *wakeError
// wrapping ErrStorage: the run stops with it and records nothing, so a re-drive schedules again.
func scheduleWake(ctx context.Context, w Wake) error {
	wk := wakerFrom(ctx)
	if wk == nil {
		return nil
	}
	if err := wk.Schedule(ctx, w); err != nil {
		return &wakeError{err: fmt.Errorf("agent: schedule wake %q for run %s: %w (%w)", w.Name, w.RootRunID, err, ErrStorage)}
	}
	return nil
}

// wakeError is a Waker.Schedule failure. The loop records no result for the tool call that
// raised it, as for a pause, and stops the run with it: the wake may not be registered, so the
// run must not pause (nothing might wake it) and must not record a failure either (a re-drive
// schedules again and pauses as it should have).
type wakeError struct{ err error }

func (e *wakeError) Error() string { return e.err.Error() }
func (e *wakeError) Unwrap() error { return e.err }

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
	runID  string // the run to resume: the wake's RootRunID
	fireAt time.Time
}

// NewMemWaker builds an in-process waker that resumes due runs via resume.
func NewMemWaker(resume func(ctx context.Context, runID string) error) *MemWaker {
	return &MemWaker{timers: map[string]scheduled{}, resume: resume}
}

// Schedule registers a wake, idempotent per (w.RunID, w.Name): re-scheduling the same timer
// overwrites its fire time rather than adding a duplicate. Timers of the same name in different
// sub-runs of one root are distinct, and the root is resumed once when either is due. An empty
// RootRunID resumes RunID. An empty RunID is ErrConfig.
func (w *MemWaker) Schedule(_ context.Context, wk Wake) error {
	if wk.RunID == "" {
		return fmt.Errorf("MemWaker.Schedule: empty RunID: %w", ErrConfig)
	}
	root := wk.RootRunID
	if root == "" {
		root = wk.RunID
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.timers[stepKey(wk.RunID, wk.Name)] = scheduled{runID: root, fireAt: wk.FireAt}
	return nil
}

// Fire resumes every run that has a timer due at or before now, each run once even if several of its
// timers are due, drops the fired timers, and returns how many runs it resumed. Resume callbacks run
// without the lock held, so a resumed run may register a new wake. Errors from resume are joined.
//
// A resume that fails (the model provider or the store was briefly unavailable) leaves the run where
// it was, so its due timers are put back and the next Fire retries it; without that, one transient
// failure would leave the run asleep with nothing to wake it. A resume that ends in a Pause
// (IsPause: the run slept again, or awaits approval, an interrupt, a signal, or a halt resolution)
// is not retried: the run is waiting on something else, and waking it every tick would only spin.
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
		if IsPause(err) {
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

// SignalPending is returned by Run when a tool called Await, AwaitFor or Receive and nothing
// has been delivered under Name yet (for Receive, Name is the channel). The run has paused
// durably at the await point. Deliver with Signal (or Enqueue for a channel), then re-invoke
// Run with RootRunID to continue. It is the externally-pushed dual of InterruptPending:
// Interrupt asks a human and resumes with their answer; Await waits for an event an outside
// system delivers.
type SignalPending struct {
	RunRef
	Name string
}

// Error names the run and the signal or channel it awaits.
func (e *SignalPending) Error() string {
	return fmt.Sprintf("run %s awaiting signal %q", e.RunID, e.Name)
}

func (*SignalPending) pause() {}

// Await blocks the current run until a single-shot signal named `name` is delivered, then
// returns its payload. Call it from inside a tool (the agent loop supplies the run context).
// On first encounter, with no signal recorded, it returns the zero T and a *SignalPending
// error that propagates out of Run, pausing the run durably. After Signal records a payload
// for the same name and Run is re-invoked, Await returns that payload and execution continues
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
	return zero, &SignalPending{RunRef: RunRef{RunID: runID, RootRunID: rootRunID(ctx, runID)}, Name: name}
}

// Signal delivers a single-shot signal to a run, journaled at-most-once by name: a
// redelivery (a retried webhook, an at-least-once queue) is a no-op and the first payload
// wins. This turns at-least-once transport into exactly-once application to the run. Safe to
// call from any process; the store's primary-key / ON CONFLICT is the cross-process dedup.
//
// Signal only records the payload. After delivering, re-invoke Run with the pause's RootRunID
// to resume the awaiting run: directly, or via a Waker scheduled at the current time.
func (j *Journal) Signal[T any](ctx context.Context, runID, name string, payload T) error {
	if runID == "" {
		return fmt.Errorf("Signal: empty runID: %w", ErrConfig)
	}
	b, err := marshalJournal(payload)
	if err != nil {
		return fmt.Errorf("agent: encode signal %q: %w (%w)", name, err, ErrConfig)
	}
	_, err = j.do(ctx, runID, signalStep(name), func(context.Context) (Record, error) {
		return Record{Kind: StepSignal, Result: b}, nil
	})
	return err
}

func signalStep(name string) string { return "signal:" + name }
