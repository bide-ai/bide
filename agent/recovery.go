// recovery.go groups the read side of the durable journal: Recover re-drives in-flight runs
// after a restart, run leasing (Leaser) coordinates a single driver per run under HA, and
// replay deterministically re-emits a journaled run. Given a store of recorded runs, these
// bring survivors back to life exactly once.

package agent

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"time"
)

// ===========================================================================
// Recovery: re-drive in-flight runs after a restart
// ===========================================================================

// RecoverOption configures Recover. Lease options apply only when the store implements Leaser.
type RecoverOption func(*recoverConfig)

type recoverConfig struct {
	holder string
	ttl    time.Duration
}

// WithLeaseHolder sets the identity this recoverer claims leases under. Defaults to a
// host/pid/random string. Give each process a stable, distinct holder if you want lease ownership
// to survive a restart of the same logical worker.
func WithLeaseHolder(id string) RecoverOption { return func(c *recoverConfig) { c.holder = id } }

// WithLeaseTTL sets how long an acquired lease is valid. Recover renews it while a run is driving,
// so a crash lets the lease expire after roughly this long and another process takes over. Defaults
// to 30s. Set it comfortably above the store's clock skew.
func WithLeaseTTL(d time.Duration) RecoverOption { return func(c *recoverConfig) { c.ttl = d } }

func defaultHolder() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s-%d-%d", host, os.Getpid(), rand.Uint64())
}

// IsComplete reports whether runID has reached its terminal answer, by checking the
// journal for the durable completion marker the agent loop records at the end of a run
// (see runCompleteStep). A crash-recovery supervisor uses it to skip finished runs.
func IsComplete(ctx context.Context, store Durable, runID string) (bool, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return false, fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
	}
	for _, r := range recs {
		if r.Kind == StepValue && r.Name == runCompleteStep {
			return true, nil
		}
	}
	return false, nil
}

// Recover re-drives the runs that were in flight when the process died. It enumerates
// every run the store holds (via Lister), skips the ones already marked complete, and
// calls resume for each remaining run to push it forward. It returns how many runs it
// re-drove and the joined genuine failures (nil if none).
//
// The store must implement Lister; a store that cannot enumerate its runs (the base
// Durable contract does not require it) yields an ErrConfig-wrapped error.
//
// If the store also implements Leaser, Recover coordinates across processes: it claims an
// exclusive, renewed lease per run before driving it and skips a run another holder currently
// leases, so competing recoverers do not both re-drive the same run. A crash lets the lease expire
// (see WithLeaseTTL) and another process takes over. Without Leaser, Recover drives every
// enumerated run, which is safe under at-most-once memoization but redundant across processes.
//
// A pause is a SUCCESS, not a failure. A re-driven run that is still waiting returns one
// of the durable pause signals (*PendingApproval, *Interrupted, *Sleeping, *Awaiting, *ResumeHalt);
// Recover detects those with errors.As and does NOT record them as errors: they mean
// "recovered, still waiting", and the run resumes later when its condition is met (a
// human approves, an interrupt is answered, a timer fires). Only a genuine error (a model
// or storage fault, a bad tool) is joined into the returned error.
//
// resume is deployment POLICY, not a mechanism the SDK can supply: it alone knows a run's
// original input and any Waker or clock to bind onto the context (a Waker-bound resume
// rebuilds the timer set for sleeping runs, since Sleep re-registers its wake on replay).
// A typical resume is:
//
//	func(ctx context.Context, runID string) error {
//	    _, err := agent.Run(agent.WithWaker(ctx, w), runID, inputFor(runID))
//	    return err
//	}
//
// resume should no-op a runID it does not own: a sub-agent run is driven by its parent, so
// re-driving one directly is redundant (harmless under at-most-once memoization, but the
// parent already replays it). Recover re-drives every incomplete run it enumerates; let
// resume decide which ones it is responsible for.
func Recover(ctx context.Context, store Durable, resume func(ctx context.Context, runID string) error, opts ...RecoverOption) (int, error) {
	lister, ok := store.(Lister)
	if !ok {
		return 0, fmt.Errorf("Recover needs a store that implements Lister to enumerate runs: %w", ErrConfig)
	}
	cfg := recoverConfig{ttl: 30 * time.Second}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.holder == "" {
		cfg.holder = defaultHolder()
	}

	runIDs, err := lister.Runs(ctx)
	if err != nil {
		return 0, fmt.Errorf("list runs for recovery: %w (%w)", err, ErrStorage)
	}

	var recovered int
	var errs []error
	for _, runID := range runIDs {
		complete, err := IsComplete(ctx, store, runID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if complete {
			continue // finished before the crash: nothing to re-drive
		}

		// Drive under the run's lease (when the store supports one). A run another holder currently
		// leases is skipped; competing recoverers and live primary drivers coordinate through Lease.
		driven, err := Lease(ctx, store, runID, func(ctx context.Context) error { return resume(ctx, runID) },
			WithLeaseHolder(cfg.holder), WithLeaseTTL(cfg.ttl))
		if !driven {
			if err != nil {
				errs = append(errs, fmt.Errorf("recover run %s: %w", runID, err))
			}
			continue // leased by another holder (err nil) or acquisition failed
		}
		recovered++
		if err != nil && !isPause(err) {
			errs = append(errs, fmt.Errorf("recover run %s: %w", runID, err))
		}
	}
	return recovered, errors.Join(errs...)
}

// Lease runs drive under an exclusive, auto-renewed lease on runID, so a primary driver and a
// recoverer (or two workers) do not drive the same run at once. If the store implements Leaser and
// another holder currently leases the run, drive is NOT called and Lease returns (false, nil). If
// the store does not implement Leaser, drive runs unconditionally. The bool reports whether drive
// ran; the error is the acquisition error (when false) or drive's own error (when true).
//
// A primary driver wraps Agent.Run so a live run and a recoverer never both drive it, using the
// same lease the recoverer respects:
//
//	driven, err := agent.Lease(ctx, store, runID, func(ctx context.Context) error {
//	    _, err := ag.Run(ctx, runID, input)
//	    return err
//	}, agent.WithLeaseHolder("worker-1"))
func Lease(ctx context.Context, store Durable, runID string, drive func(context.Context) error, opts ...RecoverOption) (bool, error) {
	cfg := recoverConfig{ttl: 30 * time.Second}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.holder == "" {
		cfg.holder = defaultHolder()
	}
	leaser, ok := store.(Leaser)
	if !ok {
		return true, drive(ctx) // no leasing available: drive unconditionally
	}
	got, err := leaser.AcquireLease(ctx, runID, cfg.holder, cfg.ttl)
	if err != nil {
		return false, fmt.Errorf("acquire lease %s: %w (%w)", runID, err, ErrStorage)
	}
	if !got {
		return false, nil // another holder is driving it
	}
	// Release on return, including if drive panics.
	defer func() { _ = leaser.ReleaseLease(ctx, runID, cfg.holder) }()
	return true, driveWithRenew(ctx, leaser, runID, cfg, drive)
}

// driveWithRenew runs the drive while renewing the lease every ttl/2, so a drive that outlasts the
// TTL keeps its lease. The drive is given a derived context that is cancelled if the lease is lost
// (renew fails or returns not-held) or the parent context is done, so a node that loses its lease
// stops driving rather than continuing un-leased. `defer close(done)` guarantees the renewer
// goroutine exits even if the drive panics.
func driveWithRenew(ctx context.Context, leaser Leaser, runID string, cfg recoverConfig, run func(context.Context) error) error {
	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(cfg.ttl / 2)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if ok, err := leaser.RenewLease(ctx, runID, cfg.holder, cfg.ttl); err != nil || !ok {
					cancel() // lost the lease: stop the drive rather than run un-leased
					return
				}
			}
		}
	}()
	return run(dctx)
}

// isPause reports whether err is a durable pause signal (the run recovered and is still
// waiting) rather than a genuine failure.
func isPause(err error) bool {
	var (
		pa  *PendingApproval
		itr *Interrupted
		slp *Sleeping
		awt *Awaiting
		rh  *ResumeHalt
	)
	return errors.As(err, &pa) || errors.As(err, &itr) || errors.As(err, &slp) || errors.As(err, &awt) || errors.As(err, &rh)
}

// ===========================================================================
// Leasing: single-driver coordination under HA
// ===========================================================================

// Leaser is the optional capability that coordinates driving a run across processes. Without it,
// several processes recovering against a shared store all re-drive the same in-flight runs: safe
// under at-most-once memoization but redundant, and a real hazard when the store's Do is not
// cross-process atomic (two live drivers could each run a non-memoized step before either records
// it). A store that implements Leaser lets a driver claim an exclusive, time-bounded lease on a run
// so only the holder drives it; a dead holder's lease expires and another process takes over, which
// is the high-availability property. Recover uses it automatically when the store provides it.
//
// The base Durable contract does not require leasing, and MemStore's implementation is in-process
// (for tests and as the reference); the cross-process payoff is a shared backend (store/postgres)
// implementing this with an atomic upsert over a leases table.
type Leaser interface {
	// AcquireLease claims runID for holder until now+ttl. It returns true if granted (the run is
	// unleased or the live lease is already holder's, which renews it), false if another holder
	// currently holds a live lease. An expired lease is available to any holder.
	AcquireLease(ctx context.Context, runID, holder string, ttl time.Duration) (bool, error)
	// RenewLease extends holder's lease on runID, returning false if holder no longer holds it (it
	// expired or was taken). A driver whose work outlasts ttl renews to keep the lease.
	RenewLease(ctx context.Context, runID, holder string, ttl time.Duration) (bool, error)
	// ReleaseLease relinquishes runID if held by holder (a no-op otherwise) so another process can
	// take it immediately rather than waiting for expiry.
	ReleaseLease(ctx context.Context, runID, holder string) error
}

// memLease is one in-memory lease: the current holder and when it expires.
type memLease struct {
	holder string
	expiry time.Time
}

// held reports whether the lease for runID is currently held by someone other than holder.
func (m *MemStore) heldByOther(runID, holder string, now time.Time) bool {
	cur, ok := m.leases[runID]
	return ok && cur.holder != holder && now.Before(cur.expiry)
}

// AcquireLease implements Leaser.
func (m *MemStore) AcquireLease(_ context.Context, runID, holder string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if m.heldByOther(runID, holder, now) {
		return false, nil
	}
	m.leases[runID] = memLease{holder: holder, expiry: now.Add(ttl)}
	return true, nil
}

// RenewLease implements Leaser.
func (m *MemStore) RenewLease(_ context.Context, runID, holder string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	cur, ok := m.leases[runID]
	if !ok || cur.holder != holder || !now.Before(cur.expiry) {
		return false, nil // no longer ours (expired or taken)
	}
	m.leases[runID] = memLease{holder: holder, expiry: now.Add(ttl)}
	return true, nil
}

// ReleaseLease implements Leaser.
func (m *MemStore) ReleaseLease(_ context.Context, runID, holder string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.leases[runID]; ok && cur.holder == holder {
		delete(m.leases, runID)
	}
	return nil
}

// ===========================================================================
// Replay: deterministic re-emission of a journaled run
// ===========================================================================

// Replay returns a Model that re-emits the model outputs recorded in `source` for runID,
// in order, instead of calling a live LLM. Run an agent with it against a FRESH store to
// deterministically re-execute a past run:
//
//   - time-travel debugging: step through exactly what happened, offline and free;
//   - regression tests: capture a production run, replay it in CI (pairs with
//     testing/synctest), assert behavior didn't drift;
//   - evals over real traffic: the journal IS a golden dataset.
//
// Because the journal captures every model output across the whole (possibly nested)
// tree, the replay is exact. This is only possible because the durable substrate records
// a complete, replayable history in the first place.
func Replay(ctx context.Context, source Durable, runID string) (Model, error) {
	recs, err := source.History(ctx, runID)
	if err != nil {
		return nil, err
	}
	var msgs []Message
	for _, r := range recs {
		if r.Kind == StepModel && r.Message != nil {
			msgs = append(msgs, *r.Message)
		}
	}
	return &replayModel{msgs: msgs}, nil
}

type replayModel struct {
	msgs []Message
	i    int
}

func (m *replayModel) Stream(_ context.Context, _ Request) (*Stream, error) {
	if m.i >= len(m.msgs) {
		return nil, fmt.Errorf("replay: %w", ErrNoRecordedOutput)
	}
	msg := m.msgs[m.i]
	m.i++

	evs := emitsFor(msg)
	ch := make(chan Emit, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return NewStream(ch), nil
}

// emitsFor converts an assistant Message back into the stream events that would have
// produced it (the inverse of msgBuilder).
func emitsFor(msg Message) []Emit {
	var out []Emit
	idx := 0
	for _, p := range msg.Parts {
		switch v := p.(type) {
		case Reasoning:
			if v.Text != "" {
				out = append(out, Emit{Event: ReasoningDelta{Text: v.Text}})
			}
			if v.Signature != "" {
				out = append(out, Emit{Event: ReasoningDelta{Signature: v.Signature}})
			}
		case Text:
			out = append(out, Emit{Event: TextDelta{Text: v.Text}})
		case ToolUse:
			out = append(out, Emit{Event: ToolCallDelta{Index: idx, ID: v.ID, Name: v.Name, ArgsFragment: v.Args}})
			idx++
		}
	}
	return append(out, Emit{Event: Finish{Reason: "stop"}})
}
