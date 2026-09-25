package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// StepKind records what a journal entry represents.
type StepKind string

const (
	StepModel      StepKind = "model"       // an assistant message from the model
	StepToolResult StepKind = "tool_result" // a completed tool call + its result
	StepValue      StepKind = "value"       // a user-authored durable step (see Step[T])
	StepApproval   StepKind = "approval"    // a durable human approve/deny decision (HITL)
	StepAttempt    StepKind = "attempt"     // "about to run a non-retriable side effect" marker
	StepSagaFail   StepKind = "saga_fail"   // a saga step failed → durable abort trigger
)

// Record is one durably-recorded, named event. The ordered sequence of Records for a
// run is its full, replayable history; resume rebuilds state by replaying them. Name
// is the stable identity used for memoization (see Durable.Do).
type Record struct {
	Name      string          `json:"name"`
	Kind      StepKind        `json:"kind"`
	Message   *Message        `json:"message,omitempty"`     // StepModel
	ToolUseID string          `json:"tool_use_id,omitempty"` // StepToolResult
	Result    json.RawMessage `json:"result,omitempty"`      // StepToolResult / StepValue
	IsError   bool            `json:"is_error,omitempty"`    // StepToolResult
	Approved  bool            `json:"approved,omitempty"`    // StepApproval
}

// Durable is the crash-safe substrate: named-step memoization. Do runs a step at
// most-once per (runID, name): a recorded step returns without re-running fn. This
// mirrors DBOS RunAsStep and ADK v2 RunNode; our SQLite default implements it directly,
// and store/postgres is the high-availability backend. The side-effect-safety layer
// (Safety / ResumeHalt) sits ABOVE this and is substrate-agnostic (see Agent.Run).
type Durable interface {
	// Do returns the recorded Record for (runID, name) without running fn if present;
	// otherwise runs fn, records the returned Record (with Name set), and returns it.
	// If fn errors, nothing is recorded — the step re-runs on the next attempt.
	Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error)
	// History returns all recorded steps for a run, in order.
	History(ctx context.Context, runID string) ([]Record, error)
}

// Lister is the optional capability a crash-recovery supervisor needs: it enumerates
// the runs a store knows about, so Recover can find in-flight runs to re-drive after a
// restart. The base Durable interface intentionally does NOT require it: memoization
// (Do) and replay (History) are the crash-safety core, and enumeration is a separate,
// backend-specific concern (a SQL store lists with a query; a sharded store may not
// enumerate cheaply at all). A store opts in by implementing Runs; Recover type-asserts
// for it.
type Lister interface {
	// Runs returns the IDs of every run the store holds, in no guaranteed order.
	Runs(ctx context.Context) ([]string, error)
}

// runCompleteStep is the journal name of the terminal completion marker. The agent loop
// appends one StepValue Record under this name when a run returns its final answer, so a
// crash-recovery supervisor can tell a finished run from an in-flight one (see IsComplete
// and Recover) without inspecting the model output.
const runCompleteStep = "run:complete"

// Step runs fn as a named durable step and returns its typed result. On resume, a
// completed step returns its recorded result without re-running fn. This is the
// Option-B authoring primitive: write plain Go control flow, and name the operations
// that must survive a crash.
//
//	inv, err := agent.Step(ctx, dur, runID, "fetch-invoice", func(ctx context.Context) (Invoice, error) { ... })
func Step[T any](ctx context.Context, d Durable, runID, name string, fn func(context.Context) (T, error)) (T, error) {
	var out T
	rec, err := d.Do(ctx, runID, name, func(ctx context.Context) (Record, error) {
		v, err := fn(ctx)
		if err != nil {
			return Record{}, err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return Record{}, err
		}
		return Record{Kind: StepValue, Result: b}, nil
	})
	if err != nil {
		return out, err
	}
	if len(rec.Result) == 0 {
		return out, nil
	}
	err = json.Unmarshal(rec.Result, &out)
	return out, err
}

// Approve durably records a human approve/deny decision for a tool call (HITL). It is
// idempotent: the first decision for a (runID, toolUseID) wins. After approving, re-run
// the agent with the same runID and it resumes past the pending-approval pause. The
// decision survives a crash because it's a journaled step like any other.
func Approve(ctx context.Context, d Durable, runID, toolUseID string, approved bool) error {
	if runID == "" {
		return fmt.Errorf("Approve: empty runID: %w", ErrConfig)
	}
	_, err := d.Do(ctx, runID, "approval:"+toolUseID, func(context.Context) (Record, error) {
		return Record{Kind: StepApproval, ToolUseID: toolUseID, Approved: approved}, nil
	})
	return err
}

// PendingApproval is returned by Agent.Run when a tool requiring human approval has no
// recorded decision yet. The run has paused durably; call Approve then re-run to resume.
type PendingApproval struct {
	RunID     string
	ToolUseID string
	ToolName  string
	Args      json.RawMessage
}

func (e *PendingApproval) Error() string {
	return fmt.Sprintf("run %s awaiting human approval for tool %q (call %s)", e.RunID, e.ToolName, e.ToolUseID)
}

// ResumeHalt is returned when resume can't safely proceed: a non-retriable tool was
// invoked but no result was recorded, so its outcome is unknown. The run stops for
// confirmation rather than risk a double side effect (e.g. a double charge).
type ResumeHalt struct {
	RunID     string
	ToolUseID string
	ToolName  string
}

func (e *ResumeHalt) Error() string {
	return fmt.Sprintf("resume halted: tool %q (call %s) has unknown outcome and is not retry-safe; confirm before continuing",
		e.ToolName, e.ToolUseID)
}

// MemStore is an in-memory Durable for tests and local dev. SQLite is the shipping
// default; store/postgres is the high-availability backend.
type MemStore struct {
	mu     sync.Mutex
	sf     singleflight.Group // collapses concurrent Do on the same (runID,name) — at-most-once fn
	runs   map[string]*runLog
	leases map[string]memLease // run leasing (see lease.go); in-process, for tests and the reference
	now    func() time.Time    // lease clock (settable in tests); defaults to time.Now
}

type runLog struct {
	order  []Record
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
	v, err, _ := m.sf.Do(runID+"\x00"+name, func() (any, error) {
		m.mu.Lock()
		rl := m.runs[runID]
		if rl == nil {
			rl = &runLog{byName: map[string]int{}}
			m.runs[runID] = rl
		}
		if i, ok := rl.byName[name]; ok {
			rec := rl.order[i]
			m.mu.Unlock()
			return rec, nil // memoized — do not re-run fn
		}
		m.mu.Unlock() // run fn without holding the lock (it may do model/tool I/O)

		rec, e := fn(ctx)
		if e != nil {
			return nil, e // not recorded — will re-run on the next attempt
		}
		rec.Name = name

		m.mu.Lock()
		defer m.mu.Unlock()
		if i, ok := rl.byName[name]; ok { // a prior write landed
			return rl.order[i], nil
		}
		rl.byName[name] = len(rl.order)
		rl.order = append(rl.order, rec)
		return rec, nil
	})
	if err != nil {
		return Record{}, err
	}
	return v.(Record), nil
}

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
	copy(out, rl.order)
	return out, nil
}
