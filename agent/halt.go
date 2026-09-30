package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Approve durably records a human approve/deny decision for a tool call (HITL). It is
// idempotent: the first decision for a (runID, toolUseID) wins. After approving, re-run
// the agent with the pause's RootRunID and it resumes past the *ApprovalPending pause. The
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

// HaltCause says why a run halted on an operation whose outcome is unknown.
type HaltCause string

const (
	// HaltCrashed: the operation's attempt marker is recorded with no result, and the halting
	// driver knows of no live claimant. That is what the halting driver saw, not proof: a
	// driver of the same run may still be running the effect (a Step that lost its claim with no
	// call of it in flight in this process, or a drive that read the journal after the claim,
	// cannot tell). ResolveHaltRef therefore checks for a live driver itself, whatever the cause.
	HaltCrashed HaltCause = "crashed"
	// HaltContended: another driver of the same run won the claim on the operation while this
	// one was running: a tool call's claim lost after this drive read no marker for it, or a
	// Step's claim lost to a call of the step in flight in this process. That driver owns the
	// effect and may be running it now.
	HaltContended HaltCause = "contended"
)

// OpKind is the kind of operation a halt is on.
type OpKind string

const (
	// OpTool is a tool call; OpRef.ID is its tool-use ID.
	OpTool OpKind = "tool"
	// OpStep is a Step; OpRef.ID is the step name.
	OpStep OpKind = "step"
)

// OpRef names an operation in a run's journal: a tool call (ID is the tool-use ID and ToolName
// the tool's name) or a Step (ID is the step name, ToolName is empty).
type OpRef struct {
	Kind     OpKind
	ID       string
	ToolName string
}

// HaltRef is what ResolveHaltRef needs to clear a halt: the run whose journal holds the
// operation, the operation, and why it halted. Take it from the halt with OutcomeUnknown.Ref.
//
// Cause matters: a HaltContended halt may still be running in another driver, and
// ResolveHaltRef resolves it only once it is older than WithMinHaltAge. An operator building a
// HaltRef by hand from a journal listing must state the cause.
type HaltRef struct {
	RunID string
	Op    OpRef
	Cause HaltCause
}

// Outcome is the verified outcome of a halted operation, recorded by ResolveHaltRef as the
// operation's result. Result is the value an actual call would have returned (JSON-marshalled);
// IsError marks an outcome the model should read as a failure (in a saga, a failed step, which
// rolls the saga back). A non-nil Evidence marks the resolution reconciled from evidence and
// stores Evidence (JSON-marshalled) beside the result, as WithEvidence does.
type Outcome struct {
	Result   any
	IsError  bool
	Evidence any
}

// ResolveHaltRef is the sanctioned escape from an *OutcomeUnknown halt. After a non-retriable
// tool call or Step halted with an unknown outcome, an operator (or a reconciler) who has
// verified the real side effect out of band records it as the operation's result, so a re-run
// proceeds past the halt instead of halting again:
//
//	if halt, ok := errors.AsType[*agent.OutcomeUnknown](err); ok {
//	    // confirm out of band that the charge did go through
//	    err = agent.ResolveHaltRef(ctx, store, halt.Ref(), agent.Outcome{Result: "charged (operator-confirmed)"},
//	        agent.WithMinHaltAge(time.Minute))
//	    msg, err = a.Run(ctx, halt.RootRunID, input) // resumes past the halt
//	}
//
// It is idempotent: the first result for an operation wins, so calling it twice or racing a
// concurrent driver records at most one. Resolving again with the same outcome returns nil; an
// operation that already has a different outcome (an earlier resolution, or the live driver's
// own result) is refused with *HaltAlreadyResolved, which reports the outcome that stands.
//
// It refuses to resolve an effect a driver may still be running, whatever the halt's Cause:
//   - with a store that leases runs (Leaser: MemStore, store/sqlite, store/postgres, found through a
//     Journal and through wrappers that implement Unwrap() Store, see Capability), it takes the
//     root run's lease for the resolution and returns *HaltInFlight while any driver holds it. Only
//     drivers that lease the run (Lease, Recover, RecoverLoop) are seen; a plain Run holds no lease,
//     which the claim below covers.
//   - with a store that cannot (a custom store with no Leaser, or a Durable that exposes none), it
//     requires WithMinHaltAge, so the halt is resolved only once no driver can still be running it.
//
// On either path, it then claims the attempt after the live one, under a claim of its own, before
// it records the outcome, and returns *HaltInFlight if a driver holds that claim already: a process
// that could not record that its claim never started may void the live attempt after the check
// and claim the next (a plain Run, which holds no lease, included), and its effect must not be
// overridden. The resolution's claim is journaled as an attempt marker of the operation. If
// recording the outcome then fails, the claim stays live (the outcome may have been recorded all
// the same), so the operation halts until it is resolved again; that resolution finds the
// resolution's attempt live and claims the one after it (with WithMinHaltAge, once that attempt
// is old enough).
//
// WithoutLiveDriverCheck skips all of these, for an operator who knows no driver is running.
//
// It refuses (ErrConfig) a ref with no valid Cause or Op.Kind, and an operation that only the
// other kind of operation attempted. WithMinHaltAge(d) refuses (*HaltTooYoung) a halt younger
// than d, measured from the live attempt's marker, so a reconciler cannot query and resolve
// before the provider's record has settled and thereby re-fire the effect. A HaltContended halt
// requires WithMinHaltAge: another driver may be running the effect, and resolving it while it
// is young could record an outcome that driver is about to contradict.
//
// Deciding the true outcome is a human (or reconciler) judgment the runtime cannot make for you.
func ResolveHaltRef(ctx context.Context, store Durable, ref HaltRef, out Outcome, opts ...ResolveOption) error {
	return resolveHalt(ctx, store, "ResolveHaltRef", ref, out, opts)
}

// ResolveHalt clears an *OutcomeUnknown halt on the tool call toolUseID of run runID, recording
// result (and isError) as the call's outcome. It is ResolveHaltRef for a tool call, with the
// cause taken as HaltCrashed, and the same live-driver check.
//
// Deprecated: transitional; replaced by ResolveHaltRef, which the 1.0 rewrite renames to
// ResolveHalt.
func ResolveHalt(ctx context.Context, store Durable, runID, toolUseID string, result any, isError bool, opts ...ResolveOption) error {
	if toolUseID == "" {
		return fmt.Errorf("ResolveHalt: empty toolUseID: %w", ErrConfig)
	}
	ref := HaltRef{RunID: runID, Op: OpRef{Kind: OpTool, ID: toolUseID}, Cause: HaltCrashed}
	return resolveHalt(ctx, store, "ResolveHalt", ref, Outcome{Result: result, IsError: isError}, opts)
}

// ResolveStepHalt clears an *OutcomeUnknown halt on the Step named name, as ResolveHalt does for
// a tool call, with the same live-driver check.
//
// Deprecated: transitional; use ResolveHaltRef with OpRef{Kind: OpStep}.
func ResolveStepHalt(ctx context.Context, store Durable, runID, name string, result any, isError bool, opts ...ResolveOption) error {
	ref := HaltRef{RunID: runID, Op: OpRef{Kind: OpStep, ID: name}, Cause: HaltCrashed}
	return resolveHalt(ctx, store, "ResolveStepHalt", ref, Outcome{Result: result, IsError: isError}, opts)
}

// haltKeys names what a resolution reads and writes: the halted operation's attempt marker, the
// marker a same-named operation of the other kind would have (to refuse the wrong kind), and the
// key and kind of the result it records.
type haltKeys struct {
	id, attempt, other, otherHint, result string
	kind                                  StepKind
}

func resolveHalt(ctx context.Context, store Durable, op string, ref HaltRef, out Outcome, opts []ResolveOption) error {
	if ref.RunID == "" {
		return fmt.Errorf("%s: empty runID: %w", op, ErrConfig)
	}
	if ref.Cause != HaltCrashed && ref.Cause != HaltContended {
		return fmt.Errorf("%s: halt cause %q is not %q or %q: %w", op, ref.Cause, HaltCrashed, HaltContended, ErrConfig)
	}
	id := ref.Op.ID
	var h haltKeys
	switch ref.Op.Kind {
	case OpTool:
		if id == "" {
			return fmt.Errorf("%s: empty toolUseID: %w", op, ErrConfig)
		}
		h = haltKeys{id: id, attempt: toolAttemptStep(id), other: stepAttemptStep(id), otherHint: "a Step (OpStep)",
			result: ToolResultStep(id), kind: StepToolResult}
	case OpStep:
		if err := checkStepName(op, id); err != nil {
			return err
		}
		h = haltKeys{id: id, attempt: stepAttemptStep(id), other: toolAttemptStep(id), otherHint: "a tool call (OpTool)",
			result: id, kind: StepValue}
	default:
		return fmt.Errorf("%s: op kind %q is not %q or %q: %w", op, ref.Op.Kind, OpTool, OpStep, ErrConfig)
	}
	cfg := resolveConfig{now: time.Now}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.evErr != nil {
		return cfg.evErr
	}
	if out.Evidence != nil {
		if cfg.reconciled {
			return fmt.Errorf("%s: evidence given both in Outcome and by WithEvidence: %w", op, ErrConfig)
		}
		b, err := json.Marshal(out.Evidence)
		if err != nil {
			return fmt.Errorf("agent: encode resolve-halt evidence: %w (%w)", err, ErrConfig)
		}
		cfg.evidence, cfg.reconciled = b, true
	}
	if ref.Cause == HaltContended && cfg.minHaltAge <= 0 {
		return fmt.Errorf("%s: %q in run %s halted because another driver holds its claim and may be running it; pass WithMinHaltAge to resolve it only once that driver cannot still be running: %w", op, id, ref.RunID, ErrConfig)
	}
	release, _, err := checkNoLiveDriver(ctx, store, op, ref, cfg)
	if err != nil {
		return err
	}
	defer release()
	recs, err := store.History(ctx, ref.RunID)
	if err != nil {
		return fmt.Errorf("%s: read history for %s: %w (%w)", op, ref.RunID, err, ErrStorage)
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
		return fmt.Errorf("%s: %q in run %s was attempted by %s, not the operation named; resolve it as that kind: %w", op, id, ref.RunID, h.otherHint, ErrConfig)
	}
	if cfg.minHaltAge > 0 {
		var at time.Time
		if attempt != nil {
			at = markerTime(attempt.AttemptedAt)
		}
		if at.IsZero() {
			return fmt.Errorf("%s: cannot enforce min halt age for %q: no attempt marker carries a timestamp: %w", op, id, ErrConfig)
		}
		if age := cfg.now().Sub(at); age < cfg.minHaltAge {
			return &HaltTooYoung{RunID: ref.RunID, ToolUseID: id, Age: age, Min: cfg.minHaltAge}
		}
	}
	b, err := marshalJournal(out.Result) // not HTML-escaped: the model reads the injected result as written
	if err != nil {
		return fmt.Errorf("agent: encode resolve-halt result for %q: %w (%w)", id, err, ErrConfig)
	}
	// Neither the lease nor the age of the live attempt stops every driver from running the effect
	// after the check: a process that remembers the live attempt's claim (its not-started record
	// could not be written) writes that record, voiding the attempt, and claims the next one, and
	// it may be a plain Run that holds no lease. So the resolution claims the next attempt itself,
	// under a claim of its own: a driver that holds it already may be running the effect
	// (*HaltInFlight), and once the resolution holds it, no driver can claim past the live attempt
	// before the result below is recorded.
	var heldKey string
	if !cfg.noLiveCheck && attempt != nil {
		heldKey = nextAttemptStep(attempt.Name)
		won, _, err := ClaimAttempt(ctx, store, ref.RunID, heldKey, Record{Kind: StepAttempt, ToolUseID: id, AttemptedAt: cfg.now().UnixMilli()})
		if err != nil {
			return fmt.Errorf("%s: claim %s: %w", op, heldKey, err)
		}
		if !won {
			root, _, _ := strings.Cut(ref.RunID, subRunSep)
			return &HaltInFlight{RunID: ref.RunID, RootRunID: root, Op: ref.Op, Attempt: heldKey}
		}
	}
	rec := Record{Kind: h.kind, Result: b, IsError: out.IsError, Reconciled: cfg.reconciled, Evidence: cfg.evidence}
	if h.kind == StepToolResult {
		rec.ToolUseID = id
	}
	got, err := store.Do(ctx, ref.RunID, h.result, func(context.Context) (Record, error) { return rec, nil })
	if err != nil {
		// The verdict may have committed all the same, so the resolution's attempt stays live: a
		// driver that lost it to this resolution must not find it voided and run the effect under
		// the next attempt beside a recorded verdict. The operation then halts until the verdict
		// is read back or the halt is resolved again; a second resolution finds this attempt live
		// and claims the one after it.
		return err
	}
	if got.Kind != rec.Kind || got.IsError != rec.IsError || !sameJSON(got.Result, rec.Result) {
		// The operation already has an outcome (an earlier resolution, or the live driver's own
		// result), and it is not this one: the first stands, and the caller must know.
		return &HaltAlreadyResolved{RunID: ref.RunID, Op: ref.Op, Result: got.Result, IsError: got.IsError}
	}
	return nil
}

// sameJSON reports whether a and b are the same JSON text, ignoring insignificant whitespace.
func sameJSON(a, b json.RawMessage) bool {
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

// resolveLeaseTTL is how long ResolveHaltRef holds the run's lease while it resolves: long enough
// for one read and one write, short enough that a resolver that dies leaves the run free soon.
const resolveLeaseTTL = 30 * time.Second

// checkNoLiveDriver refuses to resolve while a driver may be running the halted operation's effect.
// With a store that leases runs (Leaser), it takes the root run's lease for the resolution, which
// fails (*HaltInFlight) while any driver holds it, and keeps it until release is called, so no
// leased driver takes the run meanwhile. A store without Leaser cannot say whether a driver is
// live, so the resolution needs WithMinHaltAge. WithoutLiveDriverCheck skips both.
func checkNoLiveDriver(ctx context.Context, store Durable, op string, ref HaltRef, cfg resolveConfig) (func(), bool, error) {
	noop := func() {}
	if cfg.noLiveCheck {
		return noop, false, nil
	}
	root, _, _ := strings.Cut(ref.RunID, subRunSep)
	l, ok := capabilityOf[Leaser](store)
	if !ok {
		if cfg.minHaltAge <= 0 {
			return nil, false, fmt.Errorf("%s: the store cannot say whether a driver of run %s is still running %q (it does not implement Leaser); pass WithMinHaltAge so the halt is resolved only once no driver can still be running it, or WithoutLiveDriverCheck to take that risk: %w", op, root, ref.Op.ID, ErrConfig)
		}
		return noop, false, nil
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, false, fmt.Errorf("%s: lease holder id: %w (%w)", op, err, ErrStorage)
	}
	holder := "resolve-halt:" + hex.EncodeToString(b[:])
	won, err := l.AcquireLease(ctx, root, holder, resolveLeaseTTL)
	if err != nil {
		return nil, false, fmt.Errorf("%s: lease run %s: %w (%w)", op, root, err, ErrStorage)
	}
	if !won {
		return nil, false, &HaltInFlight{RunID: ref.RunID, RootRunID: root, Op: ref.Op}
	}
	return func() { _ = l.ReleaseLease(context.WithoutCancel(ctx), root, holder) }, true, nil
}

// HaltInFlight is returned by ResolveHaltRef when a driver may be running the operation's effect
// right now, and its own result must win over a resolution: a driver holds the lease on the halted
// run's root, or (see ResolveHaltRef) a driver holds the claim on the
// attempt after the live one, having found the live one recorded as never started. Retry once the
// driver has finished (its lease released or expired, or its result recorded); a retry reads the
// run again.
type HaltInFlight struct {
	RunID     string // the run whose journal holds the operation
	RootRunID string // the run whose lease is held (the halted run's root)
	Op        OpRef
	// Attempt is the attempt marker key a driver holds, when that is why the resolution was
	// refused; empty when the lease is held.
	Attempt string
}

// Error names the operation and what a driver holds.
func (e *HaltInFlight) Error() string {
	if e.Attempt != "" {
		return fmt.Sprintf("resolve-halt for %s %q (run %s): a driver holds the claim on the next attempt (%s) and may be running it; retry once it has finished",
			e.Op.Kind, e.Op.ID, e.RunID, e.Attempt)
	}
	return fmt.Sprintf("resolve-halt for %s %q (run %s): a driver holds the lease on run %s and may still be running it; retry once it has finished",
		e.Op.Kind, e.Op.ID, e.RunID, e.RootRunID)
}

// ErrAlreadyResolved is the condition of a resolution refused because the operation already has
// a different recorded outcome (see HaltAlreadyResolved). It wraps ErrConfig.
var ErrAlreadyResolved = fmt.Errorf("halt already has a different recorded outcome: %w", ErrConfig)

// HaltAlreadyResolved is returned by ResolveHaltRef when the operation already has a recorded
// outcome that differs from the one given: an earlier resolution, or the result the live driver
// recorded. The recorded outcome stands; Result and IsError report it. Resolving again with the
// same outcome is not an error. It wraps ErrAlreadyResolved.
type HaltAlreadyResolved struct {
	RunID   string
	Op      OpRef
	Result  json.RawMessage // the recorded result
	IsError bool            // whether the recorded outcome is a failure
}

// Error names the operation and the outcome that stands.
func (e *HaltAlreadyResolved) Error() string {
	return fmt.Sprintf("resolve-halt for %s %q (run %s): already recorded as %s (is_error=%v): %s",
		e.Op.Kind, e.Op.ID, e.RunID, e.Result, e.IsError, ErrAlreadyResolved)
}

// Unwrap returns ErrAlreadyResolved.
func (e *HaltAlreadyResolved) Unwrap() error { return ErrAlreadyResolved }

// ResolveOption configures ResolveHaltRef (and its wrappers ResolveHalt and ResolveStepHalt).
type ResolveOption func(*resolveConfig)

type resolveConfig struct {
	noLiveCheck bool
	minHaltAge  time.Duration
	now         func() time.Time
	evidence    json.RawMessage
	reconciled  bool
	evErr       error
}

// WithMinHaltAge refuses to resolve a halt younger than d, measured from the live attempt
// marker's AttemptedAt to now (an attempt recorded as never started does not count). A
// reconciler passes this so it cannot resolve before the provider's record has had time to
// settle (a sent-message id can appear seconds after the send): resolving too early reads
// "absent" and re-fires the very side effect the halt exists to prevent. A HaltContended halt
// requires it, and so does any halt on a store that cannot lease runs (no Leaser), unless
// WithoutLiveDriverCheck is given. d <= 0 skips the check. When d > 0 but no attempt timestamp is found,
// resolution errors rather than resolve blind; a marker whose AttemptedAt is zero or negative
// carries none (a negative value is not one the engine writes). Returns *HaltTooYoung when the
// halt has not aged enough, including a marker stamped in the future, so the caller waits and
// retries later.
func WithMinHaltAge(d time.Duration) ResolveOption {
	return func(c *resolveConfig) { c.minHaltAge = d }
}

// WithoutLiveDriverCheck skips ResolveHaltRef's check that no driver may still be running the
// halted operation: the lease probe on a store that leases runs, and the WithMinHaltAge
// requirement on one that does not. With it, a resolution can race a live driver's own result.
// Pass it only when you know no driver of the run is running, for example with every worker
// stopped.
func WithoutLiveDriverCheck() ResolveOption {
	return func(c *resolveConfig) { c.noLiveCheck = true }
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
// Outcome.Evidence does the same; giving both is ErrConfig.
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

// HaltTooYoung is returned by ResolveHaltRef when WithMinHaltAge is set and the halt has not
// aged past the grace period yet. Wait and retry the resolution later.
type HaltTooYoung struct {
	RunID     string
	ToolUseID string        // the operation's ID: a tool-use ID, or a step name
	Age       time.Duration // elapsed since the effect was attempted
	Min       time.Duration // the required minimum
}

func (e *HaltTooYoung) Error() string {
	return fmt.Sprintf("resolve-halt for call %s (run %s) too soon: attempted %s ago, need %s before resolving",
		e.ToolUseID, e.RunID, e.Age, e.Min)
}

// ApprovalPending is returned by Agent.Run when a tool requiring human approval has no
// recorded decision yet. The run has paused durably; record a decision (Approve, or
// SubmitDecision for an m-of-n gate) against RunID, then re-run RootRunID to resume.
type ApprovalPending struct {
	RunRef
	ToolUseID string
	ToolName  string
	Args      json.RawMessage
	// Quorum is non-nil for an m-of-n gate: the running tally at the pause. Record
	// decisions with SubmitDecision, then re-run.
	Quorum *ApprovalTally
}

// Error names the run and the call awaiting approval.
func (e *ApprovalPending) Error() string {
	return fmt.Sprintf("run %s awaiting human approval for tool %q (call %s)", e.RunID, e.ToolName, e.ToolUseID)
}

func (*ApprovalPending) pause() {}

// PendingApproval is the former name of ApprovalPending.
//
// Deprecated: transitional; renamed by the 1.0 rewrite. Use ApprovalPending.
type PendingApproval = ApprovalPending

// OutcomeUnknown is returned when a run cannot safely proceed past an operation whose outcome
// is unknown: a non-retriable tool call or Step was attempted (its marker is recorded) but no
// result was. The run stops for confirmation rather than risk a double side effect (a double
// charge). Cause says why: HaltCrashed when the marker was already there with no live claimant
// known (the driver that claimed it most likely died mid-effect), HaltContended when another
// driver of the same run won the claim while this one ran (for example one that took over after
// this node's lease lapsed); that driver owns the effect, and once it records the result,
// re-running proceeds normally.
//
// Clear it with ResolveHaltRef(ctx, store, halt.Ref(), outcome), then re-run RootRunID.
type OutcomeUnknown struct {
	RunRef
	Op OpRef
	// AttemptedAt is when the effect was attempted (the live attempt marker's timestamp), zero
	// if unknown (including a marker stamped zero or negative, see markerTime). A reconciler uses
	// it to honor a grace period before resolving (see WithMinHaltAge) so it does not query the
	// provider before its record has settled.
	AttemptedAt time.Time
	Cause       HaltCause
}

// Error names the operation whose outcome is unknown, and says when another driver holds its claim.
func (e *OutcomeUnknown) Error() string {
	var s string
	if e.Op.Kind == OpStep {
		s = fmt.Sprintf("resume halted: step %q has unknown outcome and is not retry-safe; confirm before continuing", e.Op.ID)
	} else {
		s = fmt.Sprintf("resume halted: tool %q (call %s) has unknown outcome and is not retry-safe; confirm before continuing",
			e.Op.ToolName, e.Op.ID)
	}
	if e.Cause == HaltContended {
		s += " (another driver holds its claim)"
	}
	return s
}

func (*OutcomeUnknown) pause() {}

// Ref returns the reference ResolveHaltRef takes to clear this halt.
func (e *OutcomeUnknown) Ref() HaltRef { return HaltRef{RunID: e.RunID, Op: e.Op, Cause: e.Cause} }

// ResumeHalt is the former name of OutcomeUnknown.
//
// Deprecated: transitional; renamed by the 1.0 rewrite. Use OutcomeUnknown.
type ResumeHalt = OutcomeUnknown

// toolHalt and stepHalt build the halts the engine returns.
func toolHalt(runID, root, toolUseID, toolName string, attemptedAt time.Time, cause HaltCause) *OutcomeUnknown {
	return &OutcomeUnknown{RunRef: RunRef{RunID: runID, RootRunID: root}, Op: OpRef{Kind: OpTool, ID: toolUseID, ToolName: toolName},
		AttemptedAt: attemptedAt, Cause: cause}
}

func stepHalt(runID, name string, attemptedAt time.Time, cause HaltCause) *OutcomeUnknown {
	return &OutcomeUnknown{RunRef: RunRef{RunID: runID, RootRunID: runID}, Op: OpRef{Kind: OpStep, ID: name},
		AttemptedAt: attemptedAt, Cause: cause}
}
