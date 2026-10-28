package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	escsubject "github.com/bide-ai/bide/internal/escalation"
)

// The escalation gate of a tainted call (docs/design/untrusted-content-and-amount-caps.md, sections 5.3
// and 5.6): PR 9b of the taint build. A call of a tool that can have a side effect, requested by a turn
// that had read untrusted content, in a run under taint, needs the policy's escalation gate in addition
// to the tool's own gate, as a second gate instance on the call. The instance is keyed by a derived
// subject, the call's ID with the suffix of package escalation: the signature of a decision binds the subject's tool-use
// ID, so a decision for one instance can never count for the other, and every record, tally, deadline and
// pause keyed by a call ID works for it unchanged. A real tool-use ID never carries the suffix (a model
// turn with a '#' in an ID is refused, ErrToolUseIDInvalid).

// escalationID is the subject of the escalation gate instance of the call toolUseID (package escalation).
func escalationID(toolUseID string) string { return escsubject.ID(toolUseID) }

// baseCallID is the call a gate subject belongs to, and whether it is an escalation subject.
func baseCallID(subject string) (string, bool) { return escsubject.Base(subject) }

// gateEnv is the loop's view of the journal that a gate instance is decided from: the records loaded at
// the drive's start (and so the decisions recorded by then), indexed by call ID.
type gateEnv struct {
	recs      []Record
	decided   map[string]bool   // tool-use IDs with a recorded one-decision record
	approvals map[string]bool   // tool-use ID -> approve(true)/deny(false)
	singles   map[string]Record // tool-use ID -> that record
	values    map[string]Record // StepValue records by name (an m-of-n gate's terminal tally)
	live      bool              // this drive asked the model for the turn
	fire      func(RunEvent)
}

// evalGate decides one gate instance of a call: id is the subject (the call's ID, or its escalation
// subject), pol the policy now in force for the instance, nil for none. It is the gate of the loop's
// pre-pass, moved out unchanged: a recorded denial or expiry is final whatever the policy is now; a
// pending gate is a pause (non-nil pause: the caller returns it); otherwise denied says the call must
// not run, and expired why.
func (a *Agent) evalGate(ctx context.Context, runID string, env *gateEnv, tu ToolUse, id string, pol *ApprovalPolicy) (denied, expired bool, pause error, err error) {
	tu.ID = id
	// The pause note of a gate is written when the drive that asked the model for the turn first pauses at it.
	// The escalation instance is asked after the tool's own gate has passed, usually by a later drive that
	// replays the turn, so its note is written whenever it first pauses (the write is insert-once).
	_, escalationInstance := baseCallID(id)
	recs, decided, approvals, singles, values := env.recs, env.decided, env.approvals, env.singles, env.values
	// protocol:claims begin ApGate ApExpire
	// A recorded denial is final, whatever the tool's gate is now: a human's Approve(false)
	// or an m-of-n gate's terminal tally that did not pass. The gate may have been removed
	// or loosened since (a redeploy), and the call must still not run. A recorded approval
	// is not carried over the same way: a gate that is still configured decides by its
	// current policy, so a tightened policy applies to a call not yet run.
	//
	// A deadline (ApprovalPolicy.Timeout) is recorded once, when the gate first waits, and an
	// expiry is recorded as the gate's outcome: a one-decision gate's decision record, or the
	// tally. Either is final, like any recorded denial, and the model reads ExpiredResult.
	_, expAt, hasDeadline := RecordedDeadline(recs, tu.ID)
	expired = decided[tu.ID] && singleExpired(singles[tu.ID], expAt, hasDeadline)
	denied = expired || decided[tu.ID] && !approvals[tu.ID]
	if r, ok := RecordedTally(values, tu.ID); ok && !denied {
		tally, err := decodeTally(runID, r)
		if err != nil {
			return false, false, nil, err
		}
		denied, expired = !tally.Passed(), tally.Expired
	}
	if !denied && pol != nil {
		var approved bool
		expAt, hasDeadline, err := openDeadline(ctx, a.store, runID, tu.ID, pol, recs)
		if err != nil {
			return false, false, nil, err
		}
		if !pol.single() {
			// m-of-n: the decision is the tally over the journaled per-approver records.
			tally, final, err := a.quorumTally(ctx, runID, tu, pol)
			if err != nil {
				return false, false, nil, err
			}
			if !final {
				if env.live || escalationInstance {
					a.notePending(ctx, runID, tu, pol)
				}
				evTally := tally // the event gets its own copy; ApprovalPending keeps tally
				evTally.Pending = append([]string(nil), tally.Pending...)
				env.fire(ApprovalRequired{ToolUseID: tu.ID, Name: tu.Name, Args: tu.Args, Quorum: &evTally})
				pend := &ApprovalPending{RunRef: RunRef{RunID: runID, RootRunID: rootRunID(ctx, runID)}, ToolUseID: tu.ID, ToolName: tu.Name, Args: tu.Args, Quorum: &tally}
				pend.Reason = tally.reapproval
				noteReask(ctx, a.store, runID, tu.ID, tu.Name, pol, tally)
				return false, false, pend, nil
			}
			approved, expired = tally.Passed(), tally.Expired
		} else {
			if !decided[tu.ID] && hasDeadline && pastDeadline(clockFrom(ctx)(), expAt) {
				// The deadline passed with no decision: the expiry is the decision, insert-once,
				// so an Approve that landed first is the decision instead.
				r, err := a.store.do(ctx, runID, approvalStep(tu.ID), func(context.Context) (Record, error) {
					return Record{Kind: StepApproval, ToolUseID: tu.ID, Approved: false, Result: json.RawMessage(expiredDecision), ApproverSignature: recordedStamp(clockFrom(ctx))}, nil
				})
				if err != nil {
					return false, false, nil, err
				}
				decided[tu.ID], approvals[tu.ID], singles[tu.ID] = true, r.Approved, r
				expired = singleExpired(r, expAt, true)
			}
			if !decided[tu.ID] {
				if env.live || escalationInstance {
					a.notePending(ctx, runID, tu, pol)
				}
				env.fire(ApprovalRequired{ToolUseID: tu.ID, Name: tu.Name, Args: tu.Args})
				return false, false, &ApprovalPending{RunRef: RunRef{RunID: runID, RootRunID: rootRunID(ctx, runID)}, ToolUseID: tu.ID, ToolName: tu.Name, Args: tu.Args}, nil
			}
			approved = approvals[tu.ID] && !expired
		}
		denied = !approved
	}
	// protocol:claims end
	return denied, expired, nil, nil
}

// escalation is what a call needs beyond its tool's own gate because of taint.
type escalation struct {
	gate   *ApprovalPolicy // the escalation gate now in force, nil when the call needs none
	refuse bool            // the policy refuses the call instead of asking
}

// gateDigest is the SHA-256 in hex of a gate policy, for the gate-reason record to say which gate the
// call was held for.
func gateDigest(p *ApprovalPolicy) string {
	if p == nil {
		return ""
	}
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// callMarks are the marks the model turn that requested call had been shown, and whether any were
// found: the pure fold over the run's journal (foldTaint), whose declarations are the registered tools'
// sources now. It reads the journal once for the call's turn; the caller shares the result among the
// turn's calls.
func (a *Agent) turnMarks(ctx context.Context, runID, firstCall string) ([]taintMark, error) {
	recs, err := a.store.History(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
	}
	declared := func(tool string) string {
		if s := a.specs[tool]; s != nil {
			return s.untrustedSource()
		}
		return ""
	}
	marks, _ := taintOfCall(recs, firstCall, startInherited(recs), declared)
	return marks, nil
}

// escalate decides what taint requires of the call tu of the tool with spec in the run runID, for a run
// under taint driven by an agent with a policy. marks are the marks the call's turn was shown. It
// recomputes the requirement from the marks and the registered spec, and writes the gate-reason record,
// insert-once, the first time it finds the call gated or exempt. A record already there (another
// driver's, an earlier drive's, or a writer's of the store) can only add: a taint reason in it is a
// requirement even when the marks recompute to none, and nothing in it (its reasons empty, or an
// exemption text) removes one; an exemption comes from the registered spec alone. A record naming
// another escalation gate than the one in force holds the call (ErrConfig), as does one that does not
// read.
func (a *Agent) escalate(ctx context.Context, runID string, tu ToolUse, spec *ToolSpec, marks []taintMark) (escalation, error) {
	if !a.taintApplies(ctx, spec) {
		return escalation{}, nil
	}
	rec, have, err := getGateReason(ctx, a.store, runID, tu.ID)
	if err != nil {
		return escalation{}, err
	}
	var recorded []gateReason
	if have {
		recorded = rec.Reasons
		if want := gateDigest(a.taint.Gate); rec.PolicyDigest != want {
			return escalation{}, fmt.Errorf("run %s: the call %q was held under an escalation gate with digest %.12s, and the taint policy now has %.12s; restore the policy it was held under, or cancel the run: %w", runID, tu.ID, rec.PolicyDigest, want, ErrConfig)
		}
	}
	var recomputed []gateReason
	for _, m := range marks {
		recomputed = append(recomputed, gateReason{Kind: gateReasonTaint, Source: m.Source, Ref: m.Ref, Pos: m.Pos})
	}
	reasons := unionReasons(recomputed, recorded)
	taintReasons := 0
	for _, r := range reasons {
		if r.Kind == gateReasonTaint {
			taintReasons++
		}
	}
	if taintReasons == 0 {
		return escalation{}, nil // nothing the call had been shown is marked
	}
	exempt := spec.TaintExempt != "" // from the registered spec, never from a record
	if !have {
		rec = gateReasonRecord{Format: gateReasonFormat, ToolUseID: tu.ID, Reasons: reasons, PolicyDigest: gateDigest(a.taint.Gate), Gate: a.taint.Gate.Clone(), RecordedAt: noteStamp(clockFrom(ctx))}
		if exempt {
			rec.Exempt = spec.TaintExempt
		}
		if _, _, err := putGateReason(ctx, a.store, runID, rec); err != nil {
			return escalation{}, err
		}
	}
	if exempt {
		return escalation{}, nil
	}
	if a.taint.Refuse {
		return escalation{refuse: true}, nil
	}
	gate := a.taint.Gate
	if a.taint.SeparateApprovers && effectiveGates(spec, gateInputs{}).Own() != nil {
		gate = gate.separatedFrom(tu.ID) // the second look is by other people than the own gate counted
	}
	return escalation{gate: gate}, nil
}

// hasTally reports whether values holds the terminal tally of the gate on id.
func hasTally(values map[string]Record, id string) bool {
	_, ok := RecordedTally(values, id)
	return ok
}

// taintApplies reports whether a call of the tool with spec, made under ctx, can be gated by taint: the
// run is under taint, this agent has a policy, and the tool can have a side effect. A compensation (a
// saga's rollback) is never gated by taint.
func (a *Agent) taintApplies(ctx context.Context, spec *ToolSpec) bool {
	return a.taint != nil && underTaintFrom(ctx) != "" && tracked(spec.Safety) && !isCompensating(ctx)
}

// taintRefusedResult is what the model reads when the policy refuses a tainted turn's side effect.
const taintRefusedResult = `"tool call refused: this run has read untrusted content"`

// refuseTainted records the refusal of a tainted call, a known failure with the tool never called, and
// returns the message the model reads.
func (a *Agent) refuseTainted(ctx context.Context, runID string, tu ToolUse, spec *ToolSpec) (*Message, error) {
	rec, err := a.store.put(ctx, runID, ToolResultStep(tu.ID), Record{Kind: StepToolResult, ToolUseID: tu.ID, IsError: true, Result: json.RawMessage(taintRefusedResult), Safety: recordedSafety(*spec), Approval: effectiveGates(spec, gateInputs{}).Snapshot()})
	if err != nil {
		return nil, err
	}
	return &Message{Role: RoleTool, Parts: []Part{ToolResult{ToolUseID: tu.ID, Result: rec.Result, IsError: rec.IsError}}}, nil
}

// preExecuteEscalation is preExecute for the escalation instance of a call, immediately before the call
// runs: the gate-reason record the loop's pre-pass wrote says the call is held under taint (a taint
// reason in it, and no exemption in the registered spec), and the instance's certificate-bound approvals
// are re-checked as the tool's own gate's are. A call that is not held under taint costs one read.
func (a *Agent) preExecuteEscalation(ctx context.Context, runID, toolUseID string, spec *ToolSpec) error {
	if !a.taintApplies(ctx, spec) || spec.TaintExempt != "" || a.taint.Gate == nil {
		return nil
	}
	rec, have, err := getGateReason(ctx, a.store, runID, toolUseID)
	if err != nil || !have {
		return err
	}
	for _, r := range rec.Reasons {
		if r.Kind == gateReasonTaint {
			return preExecute(ctx, a.store, runID, rootRunID(ctx, runID), escalationID(toolUseID), a.taint.Gate, a.approverVerifiers, "")
		}
	}
	return nil
}
