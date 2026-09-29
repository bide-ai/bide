package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	StepSignal     StepKind = "signal"      // an external event delivered into a run (see Signal/Await)
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
	Usage     *Usage          `json:"usage,omitempty"`       // StepModel: the call's token usage
	ToolUseID string          `json:"tool_use_id,omitempty"` // StepToolResult
	Result    json.RawMessage `json:"result,omitempty"`      // StepToolResult / StepValue
	IsError   bool            `json:"is_error,omitempty"`    // StepToolResult
	Approved  bool            `json:"approved,omitempty"`    // StepApproval
	Approver  string          `json:"approver,omitempty"`    // StepApproval written by ApproveAs
	Signature []byte          `json:"signature,omitempty"`   // StepApproval written by ApproveAs
	// AttemptedAt is the Unix-millis wall-clock time an attempt marker (StepAttempt) was
	// written, i.e. just before a non-retriable side effect fired. It is set once and read
	// back verbatim on replay, so it stays deterministic. Zero (and omitted) on every
	// other kind; a reconciler uses it to honor a grace period before resolving a halt.
	AttemptedAt int64 `json:"attempted_at,omitempty"`
	// Reconciled marks a StepToolResult that ResolveHalt injected from verified evidence
	// rather than one the tool produced by running. It lets a later reader (and bide-audit)
	// tell a reconciled outcome from a clean one at a glance.
	Reconciled bool `json:"reconciled,omitempty"`
	// Evidence is what a reconciler read to decide the outcome (a queried provider record,
	// a message id, a log line). It is carried on the reconciled result and signed with it,
	// so the verdict and its basis live in the journal beside the outcome.
	Evidence json.RawMessage `json:"evidence,omitempty"`
	// Claim is the random id of the driver that wrote an attempt marker (see ClaimAttempt).
	// A driver runs the side effect only if the marker it gets back carries its own claim, so
	// two drivers of the same run can never both run it, whatever their leases say.
	Claim string `json:"claim,omitempty"`
}

// ClaimAttempt writes the attempt marker named name as an exclusive claim and reports whether
// the caller won it. It stamps rec with a fresh random claim id and records it with Do; because
// Do records a name at most once, exactly one driver's marker is stored, and a driver won only
// if the marker Do returns carries its own claim. A driver that loses must not run the side
// effect the marker guards: another driver owns it and may be running it right now.
//
// This makes at-most-once independent of leasing. A lease cannot guarantee mutual exclusion (a
// holder stalled past its TTL wakes still believing it holds the lease), but two drivers that
// overlap still cannot both win the claim, since the store's atomic insert decides the winner.
// With a single driver the claim is always won, so a run with no contention behaves exactly as
// before.
func ClaimAttempt(ctx context.Context, d Durable, runID, name string, rec Record) (won bool, got Record, err error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return false, Record{}, fmt.Errorf("claim %s: %w (%w)", name, err, ErrStorage)
	}
	claim := hex.EncodeToString(b[:])
	rec.Claim = claim
	got, err = d.Do(ctx, runID, name, func(context.Context) (Record, error) { return rec, nil })
	if err != nil {
		return false, Record{}, err
	}
	return got.Claim == claim, got, nil
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

// runAbortedStep is the journal name of the terminal marker a saga records once its rollback
// has finished, so a recovery supervisor treats the aborted run as over.
const runAbortedStep = "run:aborted"

// hasValueStep reports whether runID's journal holds a StepValue record named name.
func hasValueStep(ctx context.Context, store Durable, runID, name string) (bool, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return false, fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
	}
	for _, r := range recs {
		if r.Kind == StepValue && r.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// Step runs fn as a named durable step and returns its typed result. On resume, a
// completed step returns its recorded result without re-running fn. This is the
// Option-B authoring primitive: write plain Go control flow, and name the operations
// that must survive a crash.
//
//	inv, err := agent.Step(ctx, dur, runID, "fetch-invoice", fetchInvoice, agent.StepSafety(agent.Safety{ReadOnly: true}))
//	res, err := agent.Step(ctx, dur, runID, "reserve", reserve) // a side effect: at most once
//
// A step runs at most once, like a tool call. By default it is treated as a side effect: an
// attempt marker is journaled before fn runs, so if the process dies after fn's effect and
// before its result is recorded, the resumed step returns *ResumeHalt instead of running fn
// again. Clear it with ResolveHalt (the halt's ToolUseID is the step name) once the true
// outcome is known. A step that is safe to re-run declares it with StepSafety (ReadOnly,
// Idempotent, or an IdempotencyKey); it then skips the marker and simply re-runs after a crash.
//
// If fn returns an error, nothing is recorded but the marker: a side-effecting step whose fn
// failed halts on the next attempt too, since a failed call may still have taken effect.
//
// T must be JSON-serializable: the result is marshaled into the journal, so a struct with
// unexported fields round-trips those fields to their zero values (encoding/json skips them)
// with no error reported. Return exported fields, a map, or a pointer whose fields are exported.
func Step[T any](ctx context.Context, d Durable, runID, name string, fn func(context.Context) (T, error), opts ...StepOption) (T, error) {
	var out T
	if err := ctx.Err(); err != nil {
		return out, err // a cancelled caller starts no new step
	}
	var cfg stepConfig
	for _, o := range opts {
		o(&cfg)
	}
	// The attempt marker is an exclusive claim, as for a tool call: a driver that did not
	// write it (a resume after a crash, or a second driver of the same run) must not run fn.
	claimed := true
	var attemptedAt time.Time
	if !cfg.safety.RetrySafe() {
		won, got, err := ClaimAttempt(ctx, d, runID, "attempt:step:"+name,
			Record{Kind: StepAttempt, ToolUseID: name, AttemptedAt: time.Now().UnixMilli()})
		if err != nil {
			return out, err
		}
		claimed = won
		if got.AttemptedAt != 0 {
			attemptedAt = time.UnixMilli(got.AttemptedAt)
		}
	}
	rec, err := d.Do(ctx, runID, name, func(ctx context.Context) (Record, error) {
		if !claimed {
			// Attempted before, with no recorded result: the outcome is unknown.
			return Record{}, &ResumeHalt{RunID: runID, RootRunID: runID, ToolUseID: name, AttemptedAt: attemptedAt}
		}
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
	if rec.IsError { // resolved by ResolveHalt as failed
		return out, fmt.Errorf("step %q was resolved as failed: %s: %w", name, rec.Result, ErrTool)
	}
	if len(rec.Result) == 0 {
		return out, nil
	}
	err = json.Unmarshal(rec.Result, &out)
	return out, err
}

// StepOption configures Step.
type StepOption func(*stepConfig)

type stepConfig struct{ safety Safety }

// StepSafety declares how safe a step is to re-run, as Safety does for a tool. A step that is
// RetrySafe (ReadOnly, Idempotent, or keyed) re-runs after a crash; any other step halts.
func StepSafety(s Safety) StepOption { return func(c *stepConfig) { c.safety = s } }

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

// ResolveHalt is the sanctioned escape from a ResumeHalt. After a non-retriable tool
// halted with an unknown outcome (see ResumeHalt), an operator who has verified the real
// side effect out of band injects the missing tool result directly, keyed by the halted
// tool-use ID (the same key the agent loop uses), so a re-run proceeds past the halt
// instead of halting again. Pass the result value (JSON-marshalled here) an actual call
// would have returned, and isError if the verified outcome was a failure the model should
// react to.
//
// It is idempotent: the first result for a (runID, toolUseID) wins, so calling it twice or
// racing a concurrent driver injects the record at most once. runID and toolUseID come
// straight off the ResumeHalt. After resolving, re-run the agent with the halt's RootRunID,
// which is the same run unless the halt came from inside a sub-agent:
//
//	var halt *agent.ResumeHalt
//	if errors.As(err, &halt) {
//	    // operator confirms out of band that the charge did go through
//	    _ = agent.ResolveHalt(ctx, store, halt.RunID, halt.ToolUseID, "charged (operator-confirmed)", false)
//	    msg, err = a.Run(ctx, halt.RootRunID, input) // resumes past the halt
//	}
//
// Two options refine this. WithMinHaltAge(d) refuses to resolve a halt younger than d
// (measured from the attempt marker), so a reconciler cannot query and resolve before the
// provider's record has settled and thereby re-fire the effect. WithEvidence(v) records the
// resolution as reconciled and stores what was read to decide, signed beside the outcome,
// so a clean run stays distinguishable from a reconciled one.
//
// This is the only supported way to clear a ResumeHalt for a non-idempotent side effect;
// deciding the true outcome is a human (or reconciler) judgment the runtime cannot make for you.
func ResolveHalt(ctx context.Context, store Durable, runID, toolUseID string, result any, isError bool, opts ...ResolveOption) error {
	if runID == "" {
		return fmt.Errorf("ResolveHalt: empty runID: %w", ErrConfig)
	}
	if toolUseID == "" {
		return fmt.Errorf("ResolveHalt: empty toolUseID: %w", ErrConfig)
	}
	cfg := resolveConfig{now: time.Now}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.evErr != nil {
		return cfg.evErr
	}
	if cfg.minHaltAge > 0 {
		attemptedAt, ok, err := attemptTime(ctx, store, runID, toolUseID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("ResolveHalt: cannot enforce min halt age for %q: no attempt marker carries a timestamp: %w", toolUseID, ErrConfig)
		}
		if age := cfg.now().Sub(attemptedAt); age < cfg.minHaltAge {
			return &HaltTooYoung{RunID: runID, ToolUseID: toolUseID, Age: age, Min: cfg.minHaltAge}
		}
	}
	b, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("agent: encode resolve-halt result for %q: %w (%w)", toolUseID, err, ErrConfig)
	}
	_, err = store.Do(ctx, runID, toolUseID, func(context.Context) (Record, error) {
		return Record{Kind: StepToolResult, ToolUseID: toolUseID, Result: b, IsError: isError, Reconciled: cfg.reconciled, Evidence: cfg.evidence}, nil
	})
	return err
}

// ResolveOption configures ResolveHalt.
type ResolveOption func(*resolveConfig)

type resolveConfig struct {
	minHaltAge time.Duration
	now        func() time.Time
	evidence   json.RawMessage
	reconciled bool
	evErr      error
}

// WithMinHaltAge refuses to resolve a halt younger than d, measured from the attempt
// marker's AttemptedAt to now. A reconciler passes this so it cannot resolve before the
// provider's record has had time to settle (a sent-message id can appear seconds after the
// send): resolving too early reads "absent" and re-fires the very side effect the halt
// exists to prevent. d <= 0 skips the check. When d > 0 but no attempt timestamp is found,
// ResolveHalt errors rather than resolve blind. Returns *HaltTooYoung when the halt has not
// aged enough, so the caller waits and retries later.
func WithMinHaltAge(d time.Duration) ResolveOption {
	return func(c *resolveConfig) { c.minHaltAge = d }
}

// WithNow overrides the clock WithMinHaltAge measures against (default time.Now). For tests
// and callers that carry their own clock.
func WithNow(now func() time.Time) ResolveOption {
	return func(c *resolveConfig) {
		if now != nil {
			c.now = now
		}
	}
}

// WithEvidence records the resolution as reconciled from verified evidence rather than an
// operator's bare assertion: it marks the injected result Reconciled and stores v
// (JSON-marshalled) as its Evidence, signed alongside the outcome. Use it so a later reader
// can tell a reconciled step from a clean one and re-check the basis of the verdict.
func WithEvidence(v any) ResolveOption {
	return func(c *resolveConfig) {
		b, err := json.Marshal(v)
		if err != nil {
			c.evErr = fmt.Errorf("agent: encode resolve-halt evidence: %w (%w)", err, ErrConfig)
			return
		}
		c.evidence = b
		c.reconciled = true
	}
}

// HaltTooYoung is returned by ResolveHalt when WithMinHaltAge is set and the halt has not
// aged past the grace period yet. Wait and retry the resolution later.
type HaltTooYoung struct {
	RunID     string
	ToolUseID string
	Age       time.Duration // elapsed since the effect was attempted
	Min       time.Duration // the required minimum
}

func (e *HaltTooYoung) Error() string {
	return fmt.Sprintf("resolve-halt for call %s (run %s) too soon: attempted %s ago, need %s before resolving",
		e.ToolUseID, e.RunID, e.Age, e.Min)
}

// attemptTime returns the AttemptedAt of the StepAttempt marker for toolUseID in the run's
// history. ok is false when there is no attempt marker for it or the marker carries no
// timestamp (e.g. a journal written before AttemptedAt existed).
func attemptTime(ctx context.Context, store Durable, runID, toolUseID string) (time.Time, bool, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("ResolveHalt: read history for %s: %w", runID, err)
	}
	for _, r := range recs {
		if r.Kind == StepAttempt && r.ToolUseID == toolUseID && r.AttemptedAt != 0 {
			return time.UnixMilli(r.AttemptedAt), true, nil
		}
	}
	return time.Time{}, false, nil
}

// PendingApproval is returned by Agent.Run when a tool requiring human approval has no
// recorded decision yet. The run has paused durably; call Approve then re-run to resume.
type PendingApproval struct {
	RunID string
	// RootRunID is the run to re-invoke to continue: the top-level run. It differs from RunID
	// when the signal comes from inside a sub-agent, whose journal is RunID. Record the answer
	// against RunID (Resume, Approve, ResolveHalt, Signal), then run RootRunID with the root agent.
	RootRunID string
	ToolUseID string
	ToolName  string
	Args      json.RawMessage
	// Quorum is non-nil for an m-of-n gate: the running tally at the pause. Record
	// decisions with ApproveAs, then re-run.
	Quorum *ApprovalTally
}

func (e *PendingApproval) Error() string {
	return fmt.Sprintf("run %s awaiting human approval for tool %q (call %s)", e.RunID, e.ToolName, e.ToolUseID)
}

// ResumeHalt is returned when resume can't safely proceed: a non-retriable tool was
// invoked but no result was recorded, so its outcome is unknown. The run stops for
// confirmation rather than risk a double side effect (e.g. a double charge).
//
// It is also returned when another driver of the same run claimed the call first (see
// ClaimAttempt), for example a second node that took over after this node's lease lapsed. That
// driver owns the side effect; once it records the result, re-running proceeds normally.
type ResumeHalt struct {
	RunID string
	// RootRunID is the run to re-invoke to continue: the top-level run. It differs from RunID
	// when the signal comes from inside a sub-agent, whose journal is RunID. Record the answer
	// against RunID (Resume, Approve, ResolveHalt, Signal), then run RootRunID with the root agent.
	RootRunID string
	ToolUseID string
	ToolName  string
	// AttemptedAt is when the effect was attempted (the attempt marker's timestamp), zero
	// if unknown. A reconciler uses it to honor a grace period before resolving (see
	// ResolveHalt with WithMinHaltAge) so it does not query the provider before its record
	// has settled.
	AttemptedAt time.Time
}

func (e *ResumeHalt) Error() string {
	if e.ToolName == "" { // a Step: ToolUseID is the step name
		return fmt.Sprintf("resume halted: step %q has unknown outcome and is not retry-safe; confirm before continuing", e.ToolUseID)
	}
	return fmt.Sprintf("resume halted: tool %q (call %s) has unknown outcome and is not retry-safe; confirm before continuing",
		e.ToolName, e.ToolUseID)
}

// MemStore is an in-memory Durable for tests and local dev. SQLite is the shipping
// default; store/postgres is the high-availability backend.
//
// It keeps each record as its JSON encoding and decodes on every read, the same way the SQL
// stores do. So a record a caller gets back is always an independent copy (modifying it
// cannot change the journal), and it has the same byte-normalized form a production store
// returns, which keeps tests on MemStore faithful to SQLite and Postgres.
type MemStore struct {
	mu     sync.Mutex
	sf     singleflight.Group // collapses concurrent Do on the same (runID,name) — at-most-once fn
	runs   map[string]*runLog
	leases map[string]memLease // run leasing (see lease.go); in-process, for tests and the reference
	now    func() time.Time    // lease clock (settable in tests); defaults to time.Now
}

type runLog struct {
	order  [][]byte // each record's JSON encoding, in append order
	byName map[string]int
}

// decodeRecord decodes one stored record into an independent copy.
func decodeRecord(b []byte) (Record, error) {
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return Record{}, fmt.Errorf("decode stored record: %w (%w)", err, ErrStorage)
	}
	return r, nil
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
			b := rl.order[i]
			m.mu.Unlock()
			return decodeRecord(b) // memoized — do not re-run fn
		}
		m.mu.Unlock() // run fn without holding the lock (it may do model/tool I/O)

		rec, e := fn(ctx)
		if e != nil {
			return nil, e // not recorded — will re-run on the next attempt
		}
		rec.Name = name
		b, e := json.Marshal(rec)
		if e != nil {
			return nil, fmt.Errorf("marshal step %q: %w (%w)", name, e, ErrStorage)
		}

		m.mu.Lock()
		defer m.mu.Unlock()
		if i, ok := rl.byName[name]; ok { // a prior write landed
			return decodeRecord(rl.order[i])
		}
		rl.byName[name] = len(rl.order)
		rl.order = append(rl.order, b)
		return rec, nil // the caller's own record, as the SQL stores return it
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
	for i, b := range rl.order {
		r, err := decodeRecord(b)
		if err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, nil
}
