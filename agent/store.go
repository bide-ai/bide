package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Durable is the crash-safe substrate: named-step memoization. Do runs a step at
// most-once per (runID, name): a recorded step returns without re-running fn. This
// mirrors DBOS RunAsStep and ADK v2 RunNode; our SQLite default implements it directly,
// and store/postgres is the high-availability backend. The side-effect-safety layer
// (Safety / ResumeHalt) sits ABOVE this and is substrate-agnostic (see Agent.Run).
type Durable interface {
	// Do returns the recorded Record for (runID, name) without running fn if present;
	// otherwise runs fn, records the returned Record (with Name set and a fresh Salt: persist
	// the bytes JournalEntry returns), and returns it. A recorded Record is read back with
	// DecodeStoredRecord, which refuses a row whose record names another step.
	// If fn errors, nothing is recorded — the step re-runs on the next attempt. Once fn has
	// returned a record, Do records it even if ctx was cancelled meanwhile: fn may have fired a
	// side effect, and its outcome must not be lost (see durabletest).
	Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error)
	// History returns all recorded steps for a run, in order, each read back with
	// DecodeStoredRecord under the name it is stored under.
	History(ctx context.Context, runID string) ([]Record, error)
}

// Lister is the optional capability a crash-recovery supervisor needs: it enumerates
// the runs a store knows about, so Recover can find in-flight runs to re-drive after a
// restart. The base Durable interface intentionally does NOT require it: memoization
// (Do) and replay (History) are the crash-safety core, and enumeration is a separate,
// backend-specific concern (a SQL store lists with a query; a sharded store may not
// enumerate cheaply at all). A store opts in by implementing Runs; Recover finds it with
// Capability, so a wrapper that implements Unwrap keeps its inner store's Lister.
type Lister interface {
	// Runs returns the IDs of every run the store holds, in no guaranteed order.
	Runs(ctx context.Context) ([]string, error)
}

// Capability returns store's implementation of the optional capability T (Lister, Leaser, or any
// other interface a store may implement beyond Durable), looking through wrappers.
//
// A store that wraps another Durable (an audit or tracing layer, say) exposes the store it wraps
// by implementing
//
//	Unwrap() Durable
//
// Capability checks store itself first, then follows Unwrap until a store implements T, and
// reports false once a store has no Unwrap method or Unwrap returns nil. A wrapper therefore
// exposes exactly the capabilities of the store it wraps, with no forwarding methods to keep in
// step with new capabilities. A wrapper that must change what a capability means (one that
// renames runs, for example, and so must rename what Runs returns) implements that capability
// itself, which takes precedence over the wrapped store's, as with errors.As. Lease, Recover and
// RecoverLoop find Leaser and Lister this way; code that needs a capability of a store it did not
// build should too, since a plain type assertion does not see through a wrapper.
func Capability[T any](store Durable) (T, bool) {
	for { // a nil store (or a nil Unwrap) implements nothing, so the assertions below end the walk
		if c, ok := store.(T); ok {
			return c, true
		}
		u, ok := store.(interface{ Unwrap() Durable })
		if !ok {
			break
		}
		store = u.Unwrap()
	}
	var zero T
	return zero, false
}

// runCompleteStep is the journal name of the terminal completion marker. The agent loop
// appends one StepValue Record under this name when a run returns its final answer, so a
// crash-recovery supervisor can tell a finished run from an in-flight one (see IsComplete
// and Recover) without inspecting the model output.
const runCompleteStep = "run:complete"

// runAbortedStep is the journal name of the terminal marker a saga records once its rollback
// has finished, so a recovery supervisor treats the aborted run as over.
const runAbortedStep = "run:aborted"

// runStartStep is the journal name of the record a run's first drive writes: how the run was
// started (see RunStart).
const runStartStep = "run:start"

// RunStart is how a run was started, as its first drive records it: the input it answers (for a
// Session turn, the turn's message) and whether it runs as a saga (RunSaga, StreamSaga,
// RunSagaResult, or a sub-agent called inside a saga). A run's model turns and tool calls answer
// that input under that entry point's rules, so every later drive is held to it: resuming an
// unfinished run with another input, or through the other entry point (Run for a saga, RunSaga
// for a run), is ErrConfig. A finished run returns its recorded answer whatever it is passed, as
// before.
//
// A run whose earlier drives predate this record gets it on its first drive under this version,
// with the input and entry point that drive is given.
type RunStart struct {
	Input string `json:"input"`
	Saga  bool   `json:"saga,omitempty"`
}

// RecordedStart returns how runID was started (see RunStart), and ok=false for a run whose
// journal holds no such record: one never driven, or one not driven since before the record
// existed. A session's turn run (IsSessionRun) records its message, but only the session can
// drive it (it seeds the turn with the transcript before that message), so Recover never hands
// one to its callback. A recovery callback uses it to re-drive a run with its own input and
// entry point:
//
//	start, ok, err := agent.RecordedStart(ctx, store, runID)
//	if err != nil {
//	    return err
//	}
//	if !ok {
//	    start = startFor(runID) // the deployment's own record, for a run not driven under this version
//	}
//	if start.Saga {
//	    _, err = a.RunSaga(ctx, runID, start.Input)
//	} else {
//	    _, err = a.Run(ctx, runID, start.Input)
//	}
func RecordedStart(ctx context.Context, d Durable, runID string) (RunStart, bool, error) {
	recs, err := d.History(ctx, runID)
	if err != nil {
		return RunStart{}, false, fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
	}
	for _, r := range recs {
		if r.Kind == StepValue && r.Name == runStartStep {
			var s RunStart
			if err := json.Unmarshal(r.Result, &s); err != nil {
				return RunStart{}, false, fmt.Errorf("decode %s (run %s): %w (%w)", runStartStep, runID, err, ErrStorage)
			}
			return s, true, nil
		}
	}
	return RunStart{}, false, nil
}

// holdToStart records want as runID's start if the run has none, and otherwise checks want
// against the recorded one: a drive that differs is ErrConfig, since the run's journal answers
// the recorded input under the recorded entry point's rules.
func holdToStart(ctx context.Context, d Durable, runID string, want RunStart) error {
	b, err := marshalJournal(want)
	if err != nil {
		return fmt.Errorf("encode %s (run %s): %w (%w)", runStartStep, runID, err, ErrConfig)
	}
	rec, err := d.Do(ctx, runID, runStartStep, func(context.Context) (Record, error) {
		return Record{Kind: StepValue, Result: b}, nil
	})
	if err != nil {
		return fmt.Errorf("record %s (run %s): %w (%w)", runStartStep, runID, err, ErrStorage)
	}
	var got RunStart
	if err := json.Unmarshal(rec.Result, &got); err != nil {
		return fmt.Errorf("decode %s (run %s): %w (%w)", runStartStep, runID, err, ErrStorage)
	}
	switch {
	case got.Saga && !want.Saga:
		return fmt.Errorf("run %s was started as a saga; resume it with RunSaga (or StreamSaga): %w", runID, ErrConfig)
	case !got.Saga && want.Saga:
		return fmt.Errorf("run %s was not started as a saga; resume it with Run (or Stream): %w", runID, ErrConfig)
	case got.Input != want.Input:
		return fmt.Errorf("run %s was started with a different input (see RecordedStart); resume it with that input: %w", runID, ErrConfig)
	}
	return nil
}

// MemStore is an in-memory Durable for tests and local dev. SQLite is the shipping
// default; store/postgres is the high-availability backend.
//
// It keeps each record as its journal encoding (EncodeRecord) and decodes on every read, the
// same way the SQL stores do. So a record a caller gets back is always an independent copy
// (modifying it cannot change the journal), and Do hands back the record a replay reads, on the
// live path too, which keeps tests on MemStore faithful to SQLite and Postgres.
type MemStore struct {
	mu     sync.Mutex
	sf     singleflight.Group // collapses concurrent Do on the same (runID,name) — at-most-once fn
	runs   map[string]*runLog
	leases map[string]memLease // run leasing (see lease.go); in-process, for tests and the reference
	now    func() time.Time    // lease clock (settable in tests); defaults to time.Now
}

type runLog struct {
	order  [][]byte // each record's journal encoding, in append order
	names  []string // the step name each encoding in order is stored under
	byName map[string]int
}

func NewMemStore() *MemStore {
	return &MemStore{runs: map[string]*runLog{}, leases: map[string]memLease{}, now: time.Now}
}

// setNow replaces the lease clock under the mutex that guards its reads, so a test can install a
// controlled clock without racing the lease methods. Test-only; production uses time.Now.
func (m *MemStore) setNow(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

var _ Durable = (*MemStore)(nil) // port/adapter contract
var _ Lister = (*MemStore)(nil)  // MemStore can enumerate runs for crash recovery
var _ Leaser = (*MemStore)(nil)  // MemStore can lease runs to coordinate recovery

func (m *MemStore) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	// Single-flight per (runID,name): concurrent callers for the same step run fn ONCE and
	// share the result, so a side effect can't fire twice under concurrency (parallel tools,
	// retries). In-process only; cross-process dedup is the store's job (PK/ON CONFLICT).
	// What they share is the stored encoding; each caller decodes its own copy of it below.
	v, err, _ := m.sf.Do(stepKey(runID, name), func() (any, error) {
		m.mu.Lock()
		rl := m.runs[runID]
		if rl == nil {
			rl = &runLog{byName: map[string]int{}}
			m.runs[runID] = rl
		}
		if i, ok := rl.byName[name]; ok {
			b := rl.order[i]
			m.mu.Unlock()
			return b, nil // memoized: do not re-run fn
		}
		m.mu.Unlock() // run fn without holding the lock (it may do model/tool I/O)

		rec, e := fn(ctx)
		if e != nil {
			return nil, e // not recorded — will re-run on the next attempt
		}
		b, e := JournalEntry(name, rec)
		if e != nil {
			return nil, fmt.Errorf("marshal step %q: %w (%w)", name, e, ErrStorage)
		}

		m.mu.Lock()
		defer m.mu.Unlock()
		if i, ok := rl.byName[name]; ok { // a prior write landed
			return rl.order[i], nil
		}
		rl.byName[name] = len(rl.order)
		rl.order = append(rl.order, b)
		rl.names = append(rl.names, name)
		return b, nil
	})
	if err != nil {
		return Record{}, err
	}
	// The stored record, decoded: what History returns for this step, never the caller's own.
	return DecodeStoredRecord(runID, name, v.([]byte))
}

// stepKey is the in-process deduplication key of step name of runID: the run ID's length in
// bytes, ':', the run ID, then the name. The length makes the split exact whatever bytes the two
// hold, so two different steps never share a key (joining them with a separator would not:
// ("a\x00b", "c") and ("a", "b\x00c") both join to "a\x00b\x00c").
func stepKey(runID, name string) string { return strconv.Itoa(len(runID)) + ":" + runID + name }

// Runs returns the IDs of every run the store holds, satisfying Lister so a
// crash-recovery supervisor can enumerate in-flight runs (see Recover).
func (m *MemStore) Runs(_ context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.runs))
	for id := range m.runs {
		out = append(out, id)
	}
	return out, nil
}

func (m *MemStore) History(_ context.Context, runID string) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rl := m.runs[runID]
	if rl == nil {
		return nil, nil
	}
	out := make([]Record, len(rl.order))
	for i, b := range rl.order {
		r, err := DecodeStoredRecord(runID, rl.names[i], b)
		if err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, nil
}
