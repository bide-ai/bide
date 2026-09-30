package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

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
// In a saga (RunSaga), a failure recorded this way is a failed step: the next RunSaga rolls the
// saga back, as it would have had the failure been recorded before the crash.
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
