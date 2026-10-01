// recovery.go groups the read side of the durable journal: Recover re-drives in-flight runs
// after a restart, run leasing (Leaser, in lease.go) coordinates a single driver per run under
// HA, and replay (Replay, in replay.go) deterministically re-emits a journaled run. Given a
// store of recorded runs, these bring survivors back to life exactly once.

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"sync"
	"time"
)

// ===========================================================================
// Recovery: re-drive in-flight runs after a restart
// ===========================================================================

// leaseControl implements LeaseControl.
type leaseControl func(*recoverConfig)

func (f leaseControl) applyLease(c *recoverConfig) error       { f(c); return nil }
func (f leaseControl) applyRecover(c *recoverConfig) error     { f(c); return nil }
func (f leaseControl) applyRecoverLoop(c *recoverConfig) error { f(c); return nil }

// recoverLoopOption implements RecoverLoopOption: a setting only RecoverLoop has.
type recoverLoopOption func(*recoverConfig)

func (f recoverLoopOption) applyRecoverLoop(c *recoverConfig) error { f(c); return nil }

// recoverConfig is what LeaseOption, RecoverOption and RecoverLoopOption values write. Lease
// reads the lease fields, Recover those too, and RecoverLoop every field.
type recoverConfig struct {
	holder      string
	ttl         time.Duration
	interval    time.Duration // RecoverLoop only; 0 means ttl/2
	concurrency int           // RecoverLoop only
	lapsedConc  int           // RecoverLoop only: the lapsed loop's concurrency
	onError     func(error)   // RecoverLoop only
	intervalSet bool
}

// WithLeaseHolder names the worker that claims leases, as the leases table and logs show it.
// Defaults to a host/pid/random string. The name identifies, it does not grant: each Lease call
// (and each run Recover drives) claims under a token of its own derived from it, so two drivers
// sharing a name still exclude each other, and a restarted worker waits out its predecessor's
// lease (the TTL) like any other process rather than taking it over.
func WithLeaseHolder(id string) LeaseControl {
	return leaseControl(func(c *recoverConfig) { c.holder = id })
}

// WithLeaseTTL sets how long an acquired lease is valid. Lease renews it while a run is driving,
// so a crash lets the lease expire after roughly this long and another process (RecoverLoop)
// takes over. Defaults to 30s. Set it well above the store's round-trip time and the longest pause
// you expect a process to take: renewal starts at half the TTL, and a drive that cannot renew
// within three quarters of it is cancelled with ErrLeaseLost (see Lease). It must be positive:
// Lease, Recover and RecoverLoop return an ErrConfig error otherwise.
func WithLeaseTTL(d time.Duration) LeaseControl {
	return leaseControl(func(c *recoverConfig) { c.ttl = d })
}

// WithRecoverInterval sets how often RecoverLoop starts a recovery pass. Defaults to half the
// lease TTL, so a dead holder's run is taken over within about 1.5 TTLs of its last renewal. It
// must be positive.
func WithRecoverInterval(d time.Duration) RecoverLoopOption {
	return recoverLoopOption(func(c *recoverConfig) { c.interval, c.intervalSet = d, true })
}

// WithRecoverConcurrency caps how many runs RecoverLoop's full pass drives at once. Defaults to
// 16. It must be at least 1. The lapsed loop has slots of its own (see
// WithRecoverLapsedConcurrency).
func WithRecoverConcurrency(n int) RecoverLoopOption {
	return recoverLoopOption(func(c *recoverConfig) { c.concurrency = n })
}

// WithRecoverLapsedConcurrency caps how many runs RecoverLoop's lapsed loop drives at once: the
// runs whose lease lapsed, taken over from a holder that died or stalled (see RecoverLoop).
// Defaults to 16. It must be at least 1. It applies only over a store that
// implements Leaser. These slots are separate from WithRecoverConcurrency's, so a worker drives
// up to the sum of the two at once.
func WithRecoverLapsedConcurrency(n int) RecoverLoopOption {
	return recoverLoopOption(func(c *recoverConfig) { c.lapsedConc = n })
}

// WithRecoverErrors sets the function RecoverLoop hands each genuine failure to: a store error
// while enumerating runs or acquiring a lease, or a drive that failed. Pauses and lost leases are
// not failures and are not reported. Without it, RecoverLoop drops failures (the next pass
// retries the run). It may be called from several goroutines, one call at a time.
func WithRecoverErrors(fn func(error)) RecoverLoopOption {
	return recoverLoopOption(func(c *recoverConfig) { c.onError = fn })
}

// leaseConfig applies opts, through apply, over the defaults and validates the result.
func leaseConfig[O comparable](what string, opts []O, apply func(O, *recoverConfig) error) (recoverConfig, error) {
	cfg := recoverConfig{ttl: 30 * time.Second, concurrency: 16, lapsedConc: 16}
	if err := applyOptions(what, &cfg, opts, apply); err != nil {
		return cfg, err
	}
	if cfg.ttl <= 0 {
		return cfg, fmt.Errorf("lease TTL must be positive, got %v: %w", cfg.ttl, ErrConfig)
	}
	if cfg.holder == "" {
		cfg.holder = defaultHolder()
	}
	return cfg, nil
}

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

// protocol:lifecycle begin DOpen

// completedAnswer reports whether recs hold the completion marker and, if so, the run's
// recorded final answer: the last model turn journaled before the marker. Records after the
// marker (written by an older version that re-drove finished runs) are ignored, so the first
// completion's answer stands.
func completedAnswer(recs []Record) (Message, bool) {
	for i, r := range recs {
		if r.Kind != StepValue || r.Name != runCompleteStep {
			continue
		}
		for j := i - 1; j >= 0; j-- {
			if recs[j].Kind == StepModel && recs[j].Message != nil {
				return *recs[j].Message, true
			}
		}
		return Message{}, true
	}
	return Message{}, false
}

// protocol:lifecycle end

// protocol:lifecycle begin PList PNext PSlot PWait DCheck DResume

// Recover re-drives the runs that were in flight when the process died, in one pass. It enumerates
// the runs the store holds that are not over (via Lister, which filters out every run holding a
// terminal marker: run:complete, run:aborted or run:cancelled), and calls resume for each to push
// it forward. A run another driver finished after the listing is not resumed: holding the run's
// lease, Recover checks the terminal markers again and reads the run's run:start before it calls
// resume (over a Journal, four point reads per run it drives). It returns how many runs it re-drove and the joined genuine
// failures (nil if none).
//
// The store must implement Lister; a store that cannot enumerate its runs (the base
// Durable contract does not require it) yields an ErrConfig-wrapped error.
//
// If the store also implements Leaser, Recover coordinates across processes: it claims an
// exclusive, renewed lease per run before driving it and skips a run another holder currently
// leases, so competing recoverers do not both re-drive the same run. A crash lets the lease expire
// (see WithLeaseTTL), and the next pass of another process takes the run over: Recover is one
// pass, so run RecoverLoop to keep passing. Without Leaser, Recover drives every
// enumerated run, which is safe under at-most-once memoization but redundant across processes.
//
// A pause is a SUCCESS, not a failure. A re-driven run that is still waiting returns a Pause
// (*ApprovalPending, *InterruptPending, *TimerPending, *SignalPending, *OutcomeUnknown);
// Recover detects it with IsPause and does NOT record it as an error: it means "recovered,
// still waiting", and the run resumes later when its condition is met (a human approves, an
// interrupt is answered, a timer fires). Only a genuine error (a model
// or storage fault, a bad tool) is joined into the returned error. A run whose lease was lost
// mid-drive (ErrLeaseLost) is not joined either: another process holds it now and carries it on.
//
// resume is deployment POLICY, not a mechanism the SDK can supply: it knows which agent drives a
// run and any Waker or clock it binds. Under the run's lease, after the terminal markers, Recover
// reads the run's run:start (one more point read) and hands it to resume, so resume needs no table
// of inputs: the run's input, saga flag and per-run options are journaled, and the run runs under
// them (see RunStart). ResumeAgent(a) is the Resumer of a's runs, ResumeTyped[T](a) that of its
// typed runs answering a T, and ResumeAny combines several (a deployment with two agents, say):
//
//	resume := agent.ResumeAny(agent.ResumeAgent(a, agent.WithWaker(w)), agent.ResumeTyped[Quote](b))
//
// A deployment's own Resumer is any func(ctx, runID, start) error; it returns an error wrapping
// ErrNotResumable for a run it does not drive.
//
// A run with no run:start (one never driven, such as a Signal sent to a mistyped run ID, or one
// whose first drive has not written it yet) is skipped, and reported once per process as
// ErrNotStarted. A run no Resumer drives (ErrNotResumable) is reported once per process too. What
// the process remembers is the report, not the skip: each pass reads run:start again, so a run that
// starts after a pass skipped it is recovered by a later pass. Neither counts as re-driven.
//
// Recover skips a sub-agent's run (IsSubRun): its root run drives it, and re-running the root
// resumes it. It skips a session's journal and turn runs (IsSessionRun) too: a turn is seeded with
// the transcript before its message, which only the session holds, and only the session records
// its answer, so an unfinished turn resumes when its message is sent again (the same message
// through Send or SendMessage, or the redelivered SendOnce or SendMessageOnce). Recover re-drives
// every other incomplete run it enumerates. A nil resume is ErrConfig.
func Recover(ctx context.Context, store Durable, resume Resumer, opts ...RecoverOption) (int, error) {
	if resume == nil {
		return 0, fmt.Errorf("Recover: nil Resumer: %w", ErrConfig)
	}
	lister, ok := capabilityOf[Lister](store)
	if !ok {
		return 0, fmt.Errorf("Recover needs a store that implements Lister (itself or through Unwrap) to enumerate runs: %w", ErrConfig)
	}
	cfg, err := leaseConfig("Recover", opts, RecoverOption.applyRecover)
	if err != nil {
		return 0, err
	}

	var recovered int
	var errs []error
	for runID, err := range lister.Runs(ctx, recoverFilter) {
		if err != nil {
			errs = append(errs, fmt.Errorf("list runs for recovery: %w (%w)", err, ErrStorage))
			break
		}
		if !recoverable(runID) {
			continue
		}
		driven, err := recoverRun(ctx, store, runID, resume, cfg)
		if driven {
			recovered++
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return recovered, errors.Join(errs...)
}

// Resumer drives one run a recovery pass found unfinished: runID, started as start (its run:start,
// read under the run's lease). It returns an error wrapping ErrNotResumable for a run it does not
// drive, so ResumeAny can try the next, and Recover reports the run once per process rather than
// as a failure on every pass. A Resumer passes no per-run option: the run runs under the options
// its run:start journaled (see RunStart). ResumeAgent and ResumeTyped return the Resumers of an
// agent's runs; a deployment's own Resumer is any func with this signature (one that drives a plan
// flow's runs reads start.Flow and start.Input and calls the flow's Run).
type Resumer func(ctx context.Context, runID string, start RunStart) error

// ResumeAny returns the Resumer that hands a run to each of rs in turn, in order, until one
// returns anything that does not wrap ErrNotResumable (nil included), and returns that. If every
// Resumer returns ErrNotResumable (or rs is empty), it returns an error wrapping ErrNotResumable.
// A nil Resumer it reaches is ErrConfig.
func ResumeAny(rs ...Resumer) Resumer {
	rs = slices.Clone(rs)
	return func(ctx context.Context, runID string, start RunStart) error {
		for i, r := range rs {
			if r == nil {
				return fmt.Errorf("ResumeAny: Resumer %d is nil: %w", i, ErrConfig)
			}
			if err := r(ctx, runID, start); err == nil || !errors.Is(err, ErrNotResumable) {
				return err
			}
		}
		return fmt.Errorf("run %s (kind %q): no Resumer drives it: %w", runID, start.kind(), ErrNotResumable)
	}
}

// recoverReports holds the reports a recovery pass makes once per process (ErrNotStarted,
// ErrNotResumable), by the run's store and ID. What is remembered is the report, never the skip:
// run:start is read again on every pass, so a run that starts after a pass found it unstarted is
// recovered (model 10, rule 15). An entry lives for the life of the process; there is one per run a
// pass found unstarted or not resumable.
var recoverReports = struct {
	mu sync.Mutex
	m  map[recoverReportKey]bool
}{m: map[recoverReportKey]bool{}}

// recoverReportKey names one report: the store's identity (durableIdentity), the run, and the
// report's sentinel.
type recoverReportKey struct {
	store any
	runID string
	kind  error
}

// reportOnce reports whether this process has not yet made the report kind for store's run runID,
// and marks it made. A store with no identity to key it by (see durableIdentity) is reported on
// every pass.
func reportOnce(store Durable, runID string, kind error) bool {
	id, ok := durableIdentity(store)
	if !ok {
		return true
	}
	k := recoverReportKey{store: id, runID: runID, kind: kind}
	recoverReports.mu.Lock()
	defer recoverReports.mu.Unlock()
	if recoverReports.m[k] {
		return false
	}
	recoverReports.m[k] = true
	return true
}

// endOfRunMarkers are the journal names of the terminal markers: a run that completed, a saga
// that aborted and finished its rollback, and a cancelled run are over.
var endOfRunMarkers = []string{runCompleteStep, runAbortedStep, runCancelledStep}

// recoverFilter is the runs a recovery pass enumerates: those holding no terminal marker. A SQL
// store evaluates the filter in its query, so a pass reads none of the finished runs.
var recoverFilter = RunFilter{ExcludeHolding: endOfRunMarkers}

// lapsedFilter is the runs RecoverLoop's lapsed loop enumerates: those recoverFilter admits whose
// lease has lapsed.
var lapsedFilter = RunFilter{ExcludeHolding: endOfRunMarkers, LeaseLapsed: true}

// runEnded reports whether runID holds an entry named by a terminal marker (see endOfRunMarkers):
// recoverFilter's test, applied to one run. Like the filter, it asks only whether the entry exists,
// so it reads no header and decodes nothing: a run in a format this version cannot read that is
// not over still reaches resume, which refuses it. Over a Journal it costs one point read (Store.Get)
// per marker, stopping at the first it finds; over another Durable, one History.
func runEnded(ctx context.Context, store Durable, runID string) (bool, error) {
	if j := journalOf(store); j != nil {
		for _, name := range endOfRunMarkers {
			if _, ok, err := j.store.Get(ctx, runID, name); err != nil || ok {
				if err != nil {
					return false, storageErr(fmt.Sprintf("read step %q of run %s", name, runID), err)
				}
				return true, nil
			}
		}
		return false, nil
	}
	recs, err := store.History(ctx, runID)
	if err != nil {
		return false, storageErr("load history "+runID, err) // a format refusal stays one
	}
	for _, r := range recs {
		if slices.Contains(endOfRunMarkers, r.Name) {
			return true, nil
		}
	}
	return false, nil
}

// startUnderLease reads runID's run:start for a recovery pass (see RecordedStart). Over a Journal,
// for a run that holds it, it is one point read (Store.Get) that, like runEnded, does not check the
// run's header: a run in a format this version cannot read still reaches resume, which refuses it.
// A run in another format whose run:start is missing or does not decode is a *JournalVersionError.
// Over another Durable it is RecordedStart.
func startUnderLease(ctx context.Context, store Durable, runID string) (RunStart, bool, error) {
	j := journalOf(store)
	if j == nil {
		return RecordedStart(ctx, store, runID)
	}
	e, ok, err := j.store.Get(ctx, runID, runStartStep)
	if err != nil {
		return RunStart{}, false, storageErr(fmt.Sprintf("read step %q of run %s", runStartStep, runID), err)
	}
	if !ok {
		// A run with no run:start: through the Journal's read, which refuses a run in another format
		// (a *JournalVersionError, reported as such) rather than report it unstarted. This read
		// costs one Load of the run's first entry, for an unstarted run only.
		if !j.good.has(runID) {
			first, any, err := j.firstEntry(ctx, runID)
			if err != nil {
				return RunStart{}, false, err
			}
			if any {
				if err := j.checkFirst(runID, first); err != nil {
					return RunStart{}, false, err
				}
			}
		}
		return RunStart{}, false, nil
	}
	if st, ok := decodeStartEntry(e.Data); ok {
		return st, true, nil
	}
	// Anything else (a tombstone, a record of another kind, a row that does not decode) takes the
	// full decoding, which reports it as such.
	r, err := decodeStored(runID, runStartStep, e.Data)
	if err != nil {
		if herr := j.readable(ctx, runID); herr != nil {
			return RunStart{}, false, herr // a run in another format: refused as such
		}
		return RunStart{}, false, err
	}
	if r.Kind != StepValue {
		return RunStart{}, false, nil
	}
	var st RunStart
	if err := json.Unmarshal(r.Result, &st); err != nil {
		return RunStart{}, false, fmt.Errorf("decode %s (run %s): %w (%w)", runStartStep, runID, err, ErrStorage)
	}
	return st, true, nil
}

// decodeStartEntry decodes a stored run:start record in one pass, reading only what a recovery
// pass needs: the record's name and kind, and the start its result holds. It reports false for
// anything but a value record named run:start whose result decodes as a start; the caller then
// takes the full decoding (decodeStored), which refuses or reports it. The record's other members
// are not read here: the drive the Resumer starts reads the whole journal, run:start included,
// through the full decoding, and refuses a record that does not decode.
func decodeStartEntry(b []byte) (RunStart, bool) {
	if h := decodeHook.Load(); h != nil {
		(*h)(b)
	}
	var w struct {
		Name   string        `json:"name"`
		Kind   StepKind      `json:"kind"`
		Result *runStartWire `json:"result"`
	}
	if err := json.Unmarshal(b, &w); err != nil || w.Name != runStartStep || w.Kind != StepValue || w.Result == nil {
		return RunStart{}, false
	}
	st, err := w.Result.start()
	if err != nil {
		return RunStart{}, false
	}
	return st, true
}

// recoverable reports whether a run the recovery filter admits is one a recovery pass drives: not a
// sub-agent's run (its root's re-run resumes it) or a session's (the session resumes it).
func recoverable(runID string) bool { return !IsSubRun(runID) && !IsSessionRun(runID) }

// recoverRun drives runID under its lease (when the store supports one) and reports whether it
// called resume for it and the genuine failure, if any. A run another holder currently leases is
// skipped; competing recoverers and live primary drivers coordinate through Lease. A pause and a
// lost lease are not failures.
//
// The pass listed runID before it held the lease, and another driver may have finished the run
// since (while the pass waited for a slot, or drove the runs listed before it). So under the lease,
// before resume, recoverRun checks the terminal markers again and leaves a run that is over
// undriven; a run it could not check is not driven either, and the failure is reported. The check
// cannot miss a finish by a driver that holds the run's lease (Lease, Recover, RecoverLoop): such a
// driver records its marker before it releases the lease, and this drive holds the lease from
// before the check until after resume returns. It can miss a finish by a driver that holds no
// lease (a plain Agent.Run), and one by a lease holder that stalled past its TTL: a drive that
// stalls past the TTL between the check and resume loses the lease, and another driver may finish
// the run in that window. Either way at-most-once still holds: resume is handed a finished run,
// which a resume that calls Run or RunSaga replays without firing anything again.
func recoverRun(ctx context.Context, store Durable, runID string, resume Resumer, cfg recoverConfig) (bool, error) {
	var (
		resumed      bool
		notStarted   bool
		notResumable error // the resumer's ErrNotResumable
	)
	driven, err := leaseRun(ctx, store, runID, func(ctx context.Context) error {
		if over, err := runEnded(ctx, store, runID); err != nil || over {
			return err
		}
		// The run's start, read under the lease on every pass: a run with none is skipped, and
		// read again next pass (rule 15), so one whose first drive writes it later is recovered.
		start, ok, err := startUnderLease(ctx, store, runID)
		if err != nil {
			return err
		}
		if !ok {
			notStarted = true
			return nil
		}
		err = resume(ctx, runID, start)
		if err != nil && errors.Is(err, ErrNotResumable) {
			notResumable = err
			return nil
		}
		resumed = true
		return err
	}, cfg)
	switch {
	case err != nil && (!driven || !IsPause(err) && !errors.Is(err, ErrLeaseLost)):
		return resumed, fmt.Errorf("recover run %s: %w", runID, err)
	case notStarted && reportOnce(store, runID, ErrNotStarted):
		return false, fmt.Errorf("recover run %s: skipped: %w", runID, ErrNotStarted)
	case notResumable != nil && reportOnce(store, runID, ErrNotResumable):
		return false, fmt.Errorf("recover run %s: skipped: %w", runID, notResumable)
	}
	return resumed, nil
}

// RecoverLoop re-drives in-flight runs until ctx is done, so a run whose holder dies is taken over
// automatically: Recover is a single pass, and a run another holder leases at that moment is left
// for a later one. It runs two loops, each starting a pass every WithRecoverInterval (half the
// lease TTL by default):
//
//   - The full pass enumerates the store's unfinished runs as Recover does and drives each one it
//     can lease: halted runs (waiting on an approval, an interrupt, a timer or a signal, or with an
//     outcome unknown), runs nobody leases (a plain Agent.Run whose process died) and runs whose
//     holder died. It costs about six store round trips for each unfinished run it lists, halted
//     runs included, and the next full pass does not start before this one has started all of its
//     drives, so with many unfinished runs or a slow store a full pass can outlast the interval.
//   - The lapsed loop, over a store that implements Leaser, enumerates only the unfinished runs
//     whose lease has lapsed (RunFilter.LeaseLapsed) and drives them with slots of its own
//     (WithRecoverLapsedConcurrency). A Leaser deletes a lease on release, so a lapsed lease is
//     one its holder neither renewed nor released: the holder died or stalled. A halted run holds
//     no lease between visits, so the lapsed loop neither visits the halted runs nor waits behind
//     them. Each lapsed pass first deletes the lapsed leases no pass would take over, those of
//     finished runs and of runs the store does not hold (Leaser.ReapLeases), so a holder that died
//     between its run's last write and its release does not leave a lease every later pass reads.
//
// So a dead holder's run is picked up within about one interval of its lease expiring (the TTL
// after the holder's last renewal), however many halted runs the store holds, while the lapsed
// loop has a free slot and its listing is short: it waits for a slot only while it is already
// driving WithRecoverLapsedConcurrency lapsed runs. A run that holds no lease when its driver dies
// (one driven by a plain Agent.Run, not under Lease) is left to the full pass, and its pickup can
// take up to a full pass's length longer.
//
// Runs are driven concurrently, up to WithRecoverConcurrency at once in the full pass (16 by
// default) and up to WithRecoverLapsedConcurrency in the lapsed loop (16 by default), so one long
// drive does not hold up the others; a run either loop is already driving is not started again by
// either. A pass that finds every slot of its loop busy waits for one, so each pass reaches every
// run it listed; the loop's next pass starts when this one has started all of its drives and the
// interval has elapsed. Genuine failures go to the WithRecoverErrors handler, and the run is
// retried on a later pass; pauses and lost leases are not failures (see Recover).
// A run that failed because its Waker could not schedule a wake (an error wrapping ErrStorage)
// recorded nothing for the sleeping call, so the next pass reaches the Sleep again and schedules
// again.
//
// A run with no run:start, and a run no Resumer drives, are skipped and reported once per process,
// as Recover reports them (to the WithRecoverErrors handler); every pass reads run:start again.
//
// Run it once per process, for the life of the process, with the same resume Recover takes:
//
//	go func() {
//	    err := agent.RecoverLoop(ctx, store, resume,
//	        agent.WithLeaseHolder("worker-1"),
//	        agent.WithRecoverErrors(func(err error) { log.Print(err) }))
//	    // err is ctx's error once ctx is done
//	}()
//
// A configuration error (a store that does not implement Lister, a non-positive TTL, interval or
// concurrency) is returned at once. Otherwise RecoverLoop returns ctx's error when ctx is done,
// after both loops have stopped and the drives they started (whose contexts derive from ctx) have
// returned.
func RecoverLoop(ctx context.Context, store Durable, resume Resumer, opts ...RecoverLoopOption) error {
	if resume == nil {
		return fmt.Errorf("RecoverLoop: nil Resumer: %w", ErrConfig)
	}
	lister, ok := capabilityOf[Lister](store)
	if !ok {
		return fmt.Errorf("RecoverLoop needs a store that implements Lister (itself or through Unwrap) to enumerate runs: %w", ErrConfig)
	}
	cfg, err := leaseConfig("RecoverLoop", opts, RecoverLoopOption.applyRecoverLoop)
	if err != nil {
		return err
	}
	if !cfg.intervalSet {
		cfg.interval = max(cfg.ttl/2, 1)
	}
	if cfg.interval <= 0 {
		return fmt.Errorf("recover interval must be positive, got %v: %w", cfg.interval, ErrConfig)
	}
	if cfg.concurrency < 1 {
		return fmt.Errorf("recover concurrency must be at least 1, got %d: %w", cfg.concurrency, ErrConfig)
	}
	if cfg.lapsedConc < 1 {
		return fmt.Errorf("recover lapsed concurrency must be at least 1, got %d: %w", cfg.lapsedConc, ErrConfig)
	}

	var reportMu sync.Mutex
	report := func(err error) {
		if cfg.onError == nil || ctx.Err() != nil {
			return // no handler, or a failure caused by the shutdown itself
		}
		reportMu.Lock()
		defer reportMu.Unlock()
		cfg.onError(err)
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		inFlight = map[string]bool{} // the runs either loop is driving
	)
	defer wg.Wait()
	// pass lists the runs f admits and drives each one this process is not driving yet, taking one
	// of slots for each drive.
	pass := func(f RunFilter, slots chan struct{}) {
		for runID, err := range lister.Runs(ctx, f) {
			if err != nil {
				report(fmt.Errorf("list runs for recovery: %w (%w)", err, ErrStorage))
				return
			}
			if ctx.Err() != nil {
				return
			}
			if !recoverable(runID) {
				continue
			}
			mu.Lock()
			busy := inFlight[runID]
			mu.Unlock()
			if busy {
				continue // this process is driving it already
			}
			// Wait for a free slot rather than leave the rest of the list to the next pass: the next
			// pass starts from the top again, so runs that stay incomplete on every pass (halted
			// ones) would take the slots each time and starve the runs listed after them.
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			// The other loop may have started the run while this one waited for a slot: check again
			// and mark it under one lock, so only one drive owns the entry it deletes.
			mu.Lock()
			busy = inFlight[runID]
			if !busy {
				inFlight[runID] = true
			}
			mu.Unlock()
			if busy {
				<-slots
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					mu.Lock()
					delete(inFlight, runID)
					mu.Unlock()
					<-slots
				}()
				if _, err := recoverRun(ctx, store, runID, resume, cfg); err != nil {
					report(err)
				}
			}()
		}
	}
	// every runs before (when not nil) and pass(f, slots) now and then once per interval, until ctx
	// is done.
	every := func(before func(), f RunFilter, slots chan struct{}) {
		t := time.NewTicker(cfg.interval)
		defer t.Stop()
		for {
			if before != nil {
				before()
			}
			pass(f, slots)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}

	if leaser, ok := capabilityOf[Leaser](store); ok {
		// Each lapsed pass first deletes the lapsed leases no pass would take over (a finished run's,
		// or one on a run the store does not hold), so they do not accumulate in the listing.
		reap := func() {
			if _, err := leaser.ReapLeases(ctx, endOfRunMarkers); err != nil {
				report(fmt.Errorf("reap lapsed leases: %w (%w)", err, ErrStorage))
			}
		}
		wg.Go(func() { every(reap, lapsedFilter, make(chan struct{}, cfg.lapsedConc)) })
	}
	every(nil, recoverFilter, make(chan struct{}, cfg.concurrency))
	return ctx.Err()
}

// protocol:lifecycle end
