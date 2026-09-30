package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
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
	// StepNotStarted records that the attempt whose marker it names never called its side
	// effect, written by the driver that claimed it (see attempt.go). That attempt then no
	// longer halts a resume, and the effect is re-attempted under a new marker.
	StepNotStarted StepKind = "not_started"
)

// Record is one durably-recorded, named event. The ordered sequence of Records for a
// run is its full, replayable history; resume rebuilds state by replaying them. Name
// is the stable identity used for memoization (see Durable.Do).
type Record struct {
	Name    string   `json:"name"`
	Kind    StepKind `json:"kind"`
	Message *Message `json:"message,omitempty"` // StepModel
	// Usage is token usage. On a StepModel record, the call's. On a StepToolResult or StepSagaFail
	// record, the Usage of the runs the tool call started (sub-agents): their recorded responses.
	Usage *Usage `json:"usage,omitempty"`
	// DiscardedUsage is billed usage no recorded response carries. On a StepModel record, the
	// turn's other model requests: failed attempts a middleware retried, losing hedge targets. On
	// a StepValue record named "@spend/<n>", a model call that failed for good. On a
	// StepToolResult or StepSagaFail record, the rest of the Spend of the runs the tool call
	// started. Nil when there was none. WithTokenBudget counts it.
	DiscardedUsage *Usage          `json:"discarded_usage,omitempty"`
	ToolUseID      string          `json:"tool_use_id,omitempty"` // StepToolResult
	Result         json.RawMessage `json:"result,omitempty"`      // StepToolResult / StepValue
	IsError        bool            `json:"is_error,omitempty"`    // StepToolResult
	Approved       bool            `json:"approved,omitempty"`    // StepApproval
	Approver       string          `json:"approver,omitempty"`    // StepApproval written by ApproveAs
	Signature      []byte          `json:"signature,omitempty"`   // StepApproval written by ApproveAs
	// AttemptedAt is the Unix-millis wall-clock time an attempt marker (StepAttempt) was
	// written, i.e. just before a non-retriable side effect fired. It is set once and read
	// back verbatim on replay, so it stays deterministic. Zero (and omitted) on every
	// other kind; a reconciler uses it to honor a grace period before resolving a halt.
	AttemptedAt int64 `json:"attempted_at,omitempty"`
	// Reconciled marks a StepToolResult that ResolveHalt injected from verified evidence
	// rather than one the tool produced by running. It lets a later reader (and bide-audit)
	// tell a reconciled outcome from a clean one at a glance.
	Reconciled bool `json:"reconciled,omitempty"`
	// ReadOnly marks a StepToolResult for a call whose tool was declared ReadOnly when the call
	// ran. A saga rollback skips such a call (it changed nothing) and treats any other completed
	// call as a write, whatever the tool is declared as by the time the rollback runs. A result
	// that ResolveHalt injected is never ReadOnly: only a call that was not retry-safe halts.
	ReadOnly bool `json:"read_only,omitempty"`
	// Evidence is what a reconciler read to decide the outcome (a queried provider record,
	// a message id, a log line). It is carried on the reconciled result and signed with it,
	// so the verdict and its basis live in the journal beside the outcome.
	Evidence json.RawMessage `json:"evidence,omitempty"`
	// Claim is the random id of the driver that wrote an attempt marker (see ClaimAttempt).
	// A driver runs the side effect only if the marker it gets back carries its own claim, so
	// two drivers of the same run can never both run it, whatever their leases say.
	Claim string `json:"claim,omitempty"`
	// Salt is SaltSize random bytes a store sets when it first journals the record (see
	// JournalEntry), replacing any salt the step returned. It is persisted with the record and
	// read back verbatim, so it is stable across replay and across stores. It has no meaning to
	// the run; the audit trail needs it. An audit leaf commits to the record's journal encoding,
	// salt included, and an inclusion proof for one record carries its neighbours' leaf hashes, so
	// without the salt anyone holding a proof could confirm a guessed neighbour (an approval, a
	// small tool result) by hashing it. The salt is disclosed only with its own record.
	Salt []byte `json:"salt,omitempty"`
}

// markerTime is the time an attempt marker's AttemptedAt records, or the zero time when it records
// none. Zero means a marker written before the field existed. A negative value is not a time the
// engine writes (it stamps time.Now().UnixMilli()), so it is a hand-written or tampered row; read as
// a time it would place the attempt in 1969 and make any grace period look long elapsed, so it
// counts as no timestamp too. A marker in the future (clock skew between nodes, or a tampered row)
// is kept: it is younger than any grace period, so WithMinHaltAge waits on it.
func markerTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// SaltSize is the length of Record.Salt: 32 bytes (256 bits) from crypto/rand.
const SaltSize = 32

// JournalEntry returns the bytes a store persists when it records rec as the step named name:
// rec with Name set to name and a fresh random Salt (see Record.Salt), in its journal encoding
// (EncodeRecord). Every Durable implementation must record a new step through it, so every
// record carries a salt; the audit package refuses to commit a record without one. It errors
// only if the system's random source fails or rec cannot be encoded.
func JournalEntry(name string, rec Record) ([]byte, error) {
	salt := make([]byte, SaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("salt step %q: %w (%w)", name, err, ErrStorage)
	}
	rec.Name = name
	rec.Salt = salt
	return EncodeRecord(rec)
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

// claimAttempt is ClaimAttempt for a marker key a probe of this process may be reading at the
// same moment (see doShared): it claims again rather than fail with the probe's outcome.
func claimAttempt(ctx context.Context, d Durable, runID, name string, rec Record) (bool, Record, error) {
	for {
		won, got, err := ClaimAttempt(ctx, d, runID, name, rec)
		if !errors.Is(err, errNoRecord) {
			return won, got, err
		}
		if err := ctx.Err(); err != nil {
			return false, Record{}, err
		}
	}
}

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
// existed. A recovery callback uses it to re-drive a run with its own input and entry point:
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
// again. Clear it with ResolveStepHalt (the halt's ToolUseID is the step name; its ToolName is
// empty) once the true outcome is known. A step that is safe to re-run declares it with StepSafety (ReadOnly,
// Idempotent, or an IdempotencyKey); it then skips the marker and simply re-runs after a crash.
//
// If fn returns an error, nothing is recorded but the marker: a side-effecting step whose fn
// failed halts on the next attempt too, since a failed call may still have taken effect.
//
// T must be JSON-serializable: the result is marshaled into the journal, so a struct with
// unexported fields round-trips those fields to their zero values (encoding/json skips them)
// with no error reported. Return exported fields, a map, or a pointer whose fields are exported.
//
// name must not start with a prefix the engine reserves for its own journal keys ("@", "run:",
// "tool:", "attempt:", "approval:", "signal:", and the rest; see IsReservedStepName): such a
// name is ErrConfig.
func Step[T any](ctx context.Context, d Durable, runID, name string, fn func(context.Context) (T, error), opts ...StepOption) (T, error) {
	if err := checkStepName("Step", name); err != nil {
		var zero T
		return zero, err
	}
	return step(ctx, d, runID, name, fn, opts...)
}

// step is Step without the check on name, for the engine's own steps.
func step[T any](ctx context.Context, d Durable, runID, name string, fn func(context.Context) (T, error), opts ...StepOption) (T, error) {
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
	// An earlier attempt recorded as not started does not count (see attempt.go): the claim is
	// then for the next attempt.
	base := stepAttemptStep(name)
	claimed, won := true, false
	var marker Record
	var markerKey string
	var attemptedAt time.Time
	if !cfg.safety.RetrySafe() {
		w, got, key, err := claimNextAttempt(ctx, d, runID, base,
			Record{Kind: StepAttempt, ToolUseID: name, AttemptedAt: time.Now().UnixMilli()})
		if err != nil {
			return out, err
		}
		claimed, won, marker, markerKey = w, w, got, key
		attemptedAt = markerTime(got.AttemptedAt)
	}
	var started atomic.Bool // fn was called: from here on its effect may have fired
	rec, err := d.Do(ctx, runID, name, func(ctx context.Context) (Record, error) {
		if claimed && cfg.safety.RetrySafe() {
			// A retry-safe step writes no marker, but an earlier attempt of it may have, if it
			// was declared a side effect then. The marker is the attempt's recorded safety, so
			// the step halts as it would have, rather than run a side effect a second time,
			// unless that attempt is recorded as never started.
			m, ok, err := liveAttempt(ctx, d, runID, base)
			if err != nil {
				return Record{}, err
			}
			if ok {
				claimed = false
				attemptedAt = markerTime(m.AttemptedAt)
			}
		}
		if !claimed {
			// Attempted before, with no recorded result: the outcome is unknown.
			return Record{}, &ResumeHalt{RunID: runID, RootRunID: runID, ToolUseID: name, AttemptedAt: attemptedAt}
		}
		if err := ctx.Err(); won && err != nil {
			return Record{}, err // cancelled after the claim: fn is not called, and that is recorded below
		}
		started.Store(true)
		v, err := fn(stepOnceScope(ctx, runID, name)) // the step numbers its own NextOnceKey keys
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
		if won && !started.Load() {
			// This driver claimed the attempt and never called fn (it was cancelled, or the store
			// failed, first): record that, so the next attempt runs fn instead of halting.
			if nerr := recordNotStarted(ctx, d, runID, markerKey, marker); nerr != nil {
				err = fmt.Errorf("%w (%w)", err, nerr)
			}
		}
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
	_, err := d.Do(ctx, runID, approvalStep(toolUseID), func(context.Context) (Record, error) {
		return Record{Kind: StepApproval, ToolUseID: toolUseID, Approved: approved}, nil
	})
	return err
}

// ResolveHalt is the sanctioned escape from a ResumeHalt on a tool call. After a non-retriable
// tool halted with an unknown outcome (see ResumeHalt), an operator who has verified the real
// side effect out of band injects the missing tool result directly, keyed by the halted
// tool-use ID (under ToolResultStep, the key the agent loop uses), so a re-run proceeds past the
// halt instead of halting again. Pass the result value (JSON-marshalled here) an actual call
// would have returned, and isError if the verified outcome was a failure the model should
// react to. A halted Step (its ResumeHalt has no ToolName) is cleared with ResolveStepHalt:
// ResolveHalt refuses an ID that only a Step has attempted.
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
	if toolUseID == "" {
		return fmt.Errorf("ResolveHalt: empty toolUseID: %w", ErrConfig)
	}
	h := haltKeys{op: "ResolveHalt", id: toolUseID, attempt: toolAttemptStep(toolUseID), other: stepAttemptStep(toolUseID), otherOp: "ResolveStepHalt",
		result: ToolResultStep(toolUseID), kind: StepToolResult}
	return resolve(ctx, store, runID, h, result, isError, opts)
}

// ResolveStepHalt clears a ResumeHalt on the Step named name, as ResolveHalt does for a tool
// call: it records the verified outcome as the step's result, so the resumed Step returns it
// (or, with isError, fails with it) instead of halting again. It takes the same options.
func ResolveStepHalt(ctx context.Context, store Durable, runID, name string, result any, isError bool, opts ...ResolveOption) error {
	if err := checkStepName("ResolveStepHalt", name); err != nil {
		return err
	}
	h := haltKeys{op: "ResolveStepHalt", id: name, attempt: stepAttemptStep(name), other: toolAttemptStep(name), otherOp: "ResolveHalt",
		result: name, kind: StepValue}
	return resolve(ctx, store, runID, h, result, isError, opts)
}

// haltKeys names what a resolution reads and writes: the halted operation's attempt marker, the
// marker a same-named operation of the other kind would have (to refuse the wrong function),
// and the key and kind of the result it records.
type haltKeys struct {
	op, id, attempt, other, otherOp, result string
	kind                                    StepKind
}

func resolve(ctx context.Context, store Durable, runID string, h haltKeys, result any, isError bool, opts []ResolveOption) error {
	if runID == "" {
		return fmt.Errorf("%s: empty runID: %w", h.op, ErrConfig)
	}
	cfg := resolveConfig{now: time.Now}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.evErr != nil {
		return cfg.evErr
	}
	recs, err := store.History(ctx, runID)
	if err != nil {
		return fmt.Errorf("%s: read history for %s: %w (%w)", h.op, runID, err, ErrStorage)
	}
	// The live attempt: one recorded as never started fired nothing, so its age says nothing.
	live := liveAttempts(recs)
	var attempt, other *Record
	if r, ok := live[h.attempt]; ok {
		attempt = &r
	}
	for i := range recs {
		if recs[i].Name == h.other { // a re-attempt exists only after a first attempt
			other = &recs[i]
		}
	}
	if attempt == nil && other != nil {
		return fmt.Errorf("%s: %q in run %s was attempted by the other kind of operation; resolve it with %s: %w", h.op, h.id, runID, h.otherOp, ErrConfig)
	}
	if cfg.minHaltAge > 0 {
		var at time.Time
		if attempt != nil {
			at = markerTime(attempt.AttemptedAt)
		}
		if at.IsZero() {
			return fmt.Errorf("%s: cannot enforce min halt age for %q: no attempt marker carries a timestamp: %w", h.op, h.id, ErrConfig)
		}
		if age := cfg.now().Sub(at); age < cfg.minHaltAge {
			return &HaltTooYoung{RunID: runID, ToolUseID: h.id, Age: age, Min: cfg.minHaltAge}
		}
	}
	b, err := marshalJournal(result) // not HTML-escaped: the model reads the injected result as written
	if err != nil {
		return fmt.Errorf("agent: encode resolve-halt result for %q: %w (%w)", h.id, err, ErrConfig)
	}
	rec := Record{Kind: h.kind, Result: b, IsError: isError, Reconciled: cfg.reconciled, Evidence: cfg.evidence}
	if h.kind == StepToolResult {
		rec.ToolUseID = h.id
	}
	_, err = store.Do(ctx, runID, h.result, func(context.Context) (Record, error) { return rec, nil })
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
// ResolveHalt errors rather than resolve blind; a marker whose AttemptedAt is zero or negative
// carries none (a negative value is not one the engine writes). Returns *HaltTooYoung when the
// halt has not aged enough, including a marker stamped in the future, so the caller waits and
// retries later.
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
	// if unknown (including a marker stamped zero or negative, see markerTime). A reconciler uses it to honor a grace period before resolving (see
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

// EncodeRecord returns the journal encoding of r: the bytes a store persists for it and the
// bytes an audit leaf commits to. Every store writes exactly these bytes, so a record's journal
// form does not depend on the backend, and every store hands back DecodeRecord of them, on the
// live path as on replay, so a resumed run rebuilds exactly the conversation the live run had.
//
// The encoding is JSON with two properties that make it canonical:
//
//   - It is a fixed point: decoding it with DecodeRecord and encoding the result again yields
//     the same bytes. So a store holds exactly EncodeRecord(rec) for the record rec it hands
//     back, and an audit leaf computed from a record read back from any store is the bytes that
//     store persisted.
//   - It keeps the content of every json.RawMessage (a tool's arguments and result, a step's
//     value, a reconciler's evidence) byte for byte, except whitespace between JSON tokens,
//     which is removed. Nothing is HTML-escaped: a tool result that says "<b>" is journaled as
//     "<b>", not with the escapes json.Marshal writes for < and >, so an adapter that forwards
//     the JSON text to a model sends what the tool returned. Key order, number spelling ("1.50",
//     "1e20"), escapes already in the text, and even invalid UTF-8 inside a JSON string are kept
//     as they are.
//
// Insignificant whitespace is dropped because encoding/json compacts every raw value it embeds,
// and one spelling per JSON value is what an audit leaf should commit to. For the same reason
// the line and paragraph separators U+2028 and U+2029 are always written as their JSON escapes,
// which encoding/json emits in some builds but not others. In a Go string field
// (message text, a reasoning signature), invalid UTF-8 becomes U+FFFD, as encoding/json writes
// it; since the stores hand out only the decoded form, the live and replayed conversations agree.
func EncodeRecord(r Record) ([]byte, error) {
	b, err := marshalJournal(r)
	if err != nil {
		return nil, err
	}
	// One decode and re-encode reaches the fixed point. The first pass writes invalid UTF-8 in a
	// Go string field as the JSON escape for U+FFFD, which decodes to a valid U+FFFD that a later pass
	// writes verbatim; every other part of the encoding is already stable.
	back, err := DecodeRecord(b)
	if err != nil {
		return nil, err
	}
	return marshalJournal(back)
}

// DecodeRecord decodes a record from its journal encoding (see EncodeRecord) into an independent
// copy that shares no memory with b.
func DecodeRecord(b []byte) (Record, error) {
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return Record{}, fmt.Errorf("decode stored record: %w (%w)", err, ErrStorage)
	}
	return r, nil
}

// DecodeStoredRecord decodes the record a store holds for the step name of run runID (see
// DecodeRecord) and checks that the record carries that name. Every record is journaled with the
// name it is stored under (JournalEntry), and the engine finds records by the name inside them
// (IsComplete, the approval tally, a signal, an attempt marker), so a row whose record names
// another step (a row edited or copied in the backing store) would be read as that step: a row
// under "x" holding a record named run:complete would mark an unfinished run complete. Such a row
// is ErrStorage, naming the run and the key, like any other stored record that does not decode:
// the store's contents are wrong, whatever wrote them. Fields this version does not know still
// decode, so a journal a newer version wrote stays readable.
//
// Every Durable implementation must read a record back through it, in Do and in History.
func DecodeStoredRecord(runID, name string, b []byte) (Record, error) {
	r, err := DecodeRecord(b)
	if err != nil {
		return Record{}, fmt.Errorf("run %s, step %q: %w", runID, name, err)
	}
	if r.Name != name {
		return Record{}, fmt.Errorf("run %s: the row stored as step %q holds a record named %q: %w", runID, name, r.Name, ErrStorage)
	}
	return r, nil
}

// marshalJournal is json.Marshal without HTML escaping and without the newline an Encoder
// appends.
//
// U+2028 and U+2029 are always written as their six-character JSON escapes. encoding/json escapes
// them inside a raw value when built on its v2 implementation (the Go 1.27 default) but not when
// built with GOEXPERIMENT=nojsonv2; escaping them here gives the journal one encoding in every
// build. Both byte sequences can occur only inside a JSON string (everything outside one is
// ASCII), where the escape denotes the same character.
func marshalJournal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	b = bytes.ReplaceAll(b, []byte(lineSep), []byte(jsonEscape+"2028"))
	return bytes.ReplaceAll(b, []byte(paraSep), []byte(jsonEscape+"2029")), nil
}

const (
	lineSep    = "\xe2\x80\xa8" // U+2028 LINE SEPARATOR, UTF-8 encoded
	paraSep    = "\xe2\x80\xa9" // U+2029 PARAGRAPH SEPARATOR, UTF-8 encoded
	jsonEscape = "\x5cu"        // a backslash and u: the prefix of a JSON \uXXXX escape
)

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
