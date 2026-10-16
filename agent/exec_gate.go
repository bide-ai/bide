package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// maxExecRounds bounds the loop of preExecute: each round either returns, or writes an exec hold that the
// next round counts past. A gate whose every generation is held in turn stops here with an error.
const maxExecRounds = 8

// PendingIdentityStatus is ApprovalPending.Reason for a certificate-bound gate whose counted approvals
// cannot be shown to hold right now (the status of a certificate is missing or stale, or a decision no
// signed head covers): the call did not run and nothing authoritative was written. The same approvals
// run it once a fresh check passes; no approver needs to decide again.
const PendingIdentityStatus = "approval paused before execution: the revocation status of a counted approval's certificate is missing or stale, or the approval cannot be proven now; the call runs when a fresh check passes, with the same approvals"

// preExecute is the one function every path that can run a call behind a gate with certificate-bound
// approvers goes through, immediately before the call runs: the first execution, crash recovery, a
// RecoverLoop resume, a retry of a retry-safe tool, and a plan flow node (docs/design/approver-identity.md,
// "Status is re-checked immediately before the call executes"). It returns nil when every counted
// certificate-bound decision of the gate's outcome still holds, with status no older than the gate's
// execution ages; the caller then runs the call. Otherwise the call does not run:
//   - a decision that is revoked (E <= A(d)) or otherwise no longer verifies for good: an exec hold
//     (ApprovalExecHoldStep) is written, the next round counts the next generation, and the gate asks again
//     (*ApprovalPending with PendingExecHoldReapproval) unless that count passes and holds in turn;
//   - status that is missing or stale, or a decision no head covers now: nothing authoritative is written
//     (a phase note says exec-hold), and *ApprovalPending carries PendingIdentityStatus.
//
// root is the run tree's root for the pending value, since is a flow gate's request step ("" for a tool
// call), and a gate without a certificate-bound approver returns nil at once. Every error is a refusal:
// the call does not run.
func preExecute(ctx context.Context, j *Journal, runID, root, id string, pol *ApprovalPolicy, vf ApproverVerifierFor, since string) error {
	if pol == nil || pol.single() || vf == nil || !isBound(pol, vf) {
		return nil
	}
	for range maxExecRounds {
		recs, err := j.History(ctx, runID)
		if err != nil {
			return fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
		}
		_, call, ok := FindToolCall(recs, id)
		if !ok {
			return fmt.Errorf("run %s: the call %q about to run has no recorded request: %w", runID, id, ErrConfig)
		}
		s := ApprovalSubject{RunID: runID, ToolUseID: id, ToolName: call.Name, Args: call.Args}
		st := identChain(GateTallyValues(recs, id), id)
		if !st.present || !st.decided {
			// The gate holds, or has no outcome: count (which records the next generation when it passes).
			tally, final, err := tallyGate(ctx, j, runID, s, pol, vf, since)
			if err != nil {
				return err
			}
			if !final {
				return execPending(ctx, j, runID, root, s, pol, tally)
			}
			continue // a tally was recorded: read the chain again
		}
		tally, err := decodeTally(runID, st.outcome)
		if err != nil {
			return err
		}
		if !tally.Passed() {
			return fmt.Errorf("run %s: the approval gate on %q recorded %s as its outcome, which does not approve the call: %w", runID, id, st.outcome.Name, ErrConfig)
		}
		ident, ok := identityRecordFor(recs, id, st.gen, st.outcome)
		if !ok {
			return fmt.Errorf("run %s: the approval gate on %q has no identity record for its tally %s, so its counted approvals cannot be re-checked before the call runs: %w", runID, id, st.outcome.Name, ErrConfig)
		}
		counted := countedDecisions(recs, s, ident)
		verdict := CheckExec(counted, vf)
		switch verdict.Outcome {
		case ExecProceed:
			return nil
		case ExecWait:
			detail := execDetail(verdict, "waiting for revocation status that is current")
			writePauseNote(ctx, j, runID, s, pol, vf, recs, PauseExecHold, detail)
			return execPending(ctx, j, runID, root, s, pol, tally, PendingIdentityStatus)
		}
		if err := writeExecHold(ctx, j, runID, s, pol, vf, st.gen, verdict); err != nil {
			return err
		}
		writePauseNote(ctx, j, runID, s, pol, vf, recs, PauseExecHold, execDetail(verdict, "held before execution, asking the approvers again"))
	}
	return fmt.Errorf("run %s: the approval gate on %q was held again and again before its call could run (%d rounds): %w", runID, id, maxExecRounds, ErrConfig)
}

// execPending is the pause of a gate that does not run its call after the pre-execution check: the running
// count tally, and the reason (the tally's own re-ask reason unless one is given).
func execPending(ctx context.Context, j *Journal, runID, root string, s ApprovalSubject, pol *ApprovalPolicy, tally ApprovalTally, reason ...string) *ApprovalPending {
	pend := &ApprovalPending{RunRef: RunRef{RunID: runID, RootRunID: root}, ToolUseID: s.ToolUseID, ToolName: s.ToolName, Args: s.Args, Quorum: &tally}
	pend.Reason = tally.reapproval
	if len(reason) > 0 {
		pend.Reason = reason[0]
	}
	noteReask(ctx, j, runID, s.ToolUseID, s.ToolName, pol, tally)
	return pend
}

// identityRecordFor finds the ApprovalIdentity record that belongs to tally (the outcome of generation
// gen): the one named for the generation, or its digest-named variant, whose Tally names the tally and
// whose TallyDigest is the SHA-256 of the tally's bytes.
func identityRecordFor(recs []Record, toolUseID string, gen int, tally Record) (ApprovalIdentity, bool) {
	sum := sha256.Sum256(tally.Result)
	digest := hex.EncodeToString(sum[:])
	base := ApprovalIdentityStep(toolUseID, gen)
	for _, r := range recs {
		if r.Kind != StepValue || r.Name != base && !strings.HasPrefix(r.Name, base+":") {
			continue
		}
		var ir ApprovalIdentity
		if json.Unmarshal(r.Result, &ir) == nil && ir.Format == ApprovalIdentityFormat && ir.ToolUseID == toolUseID && ir.Generation == gen && ir.Tally == tally.Name && ir.TallyDigest == digest {
			return ir, true
		}
	}
	return ApprovalIdentity{}, false
}

// countedDecisions presents each counted certificate-bound decision of ident to its verifier: the decision
// record the identity record names, in its place in the journal. A record that is no longer there is
// presented empty, which no verifier accepts.
func countedDecisions(recs []Record, s ApprovalSubject, ident ApprovalIdentity) []CountedDecision {
	out := make([]CountedDecision, 0, len(ident.Decisions))
	for _, d := range ident.Decisions {
		cd := CertDecision{Subject: s, Journal: recs, Index: -1}
		for i, r := range recs {
			if r.Name == d.Step && IsApprovalDecision(r, s.ToolUseID) && r.Approver() == d.Approver {
				cd.Record, cd.Index = r, i
				break
			}
		}
		out = append(out, CountedDecision{Approver: d.Approver, Decision: cd})
	}
	return out
}

// execDetail is one sentence for a person, in the gate's words: how many counted approvals do not hold and
// why (the Reason constants are ours), never text taken from a certificate.
func execDetail(v ExecVerdict, tail string) string {
	reasons := map[string]bool{}
	var who []string
	for _, f := range v.Failed {
		who = append(who, f.Approver)
		reasons[f.Reason] = true
	}
	var rs []string
	for r := range reasons {
		rs = append(rs, r)
	}
	slices.Sort(rs)
	return fmt.Sprintf("%d of %d counted approvals do not hold before execution (%s): %s; %s", len(v.Failed), v.Checked, strings.Join(who, ", "), strings.Join(rs, "; "), tail)
}

// writeExecHold records, insert-once, the exec hold of generation gen. A hold already there (another drive
// wrote it) is the hold.
func writeExecHold(ctx context.Context, j *Journal, runID string, s ApprovalSubject, pol *ApprovalPolicy, vf ApproverVerifierFor, gen int, v ExecVerdict) error {
	h := ExecHold{Format: ApprovalExecHoldFormat, ToolUseID: s.ToolUseID, Generation: gen, ReasonCode: -1,
		Detail:    execDetail(v, "the approvers decide again"),
		CheckedAt: clockFrom(ctx)().UTC().Format(time.RFC3339Nano)}
	// The first failure that is a hold, a revocation first, names the reason.
	first := -1
	for i, f := range v.Failed {
		if execWaits(f.Reason) {
			continue
		}
		if first < 0 || f.Revocation != nil && v.Failed[first].Revocation == nil {
			first = i
		}
	}
	for _, f := range v.Failed {
		h.Approvers = append(h.Approvers, f.Approver)
	}
	if first >= 0 {
		f := v.Failed[first]
		h.Reason, h.StatusTime, h.AnchoredAt = f.Reason, f.StatusTime, f.Anchored
		if f.Revocation != nil {
			h.Serial, h.E, h.ReasonCode, h.CRLHash = f.Revocation.Serial, f.Revocation.E, f.Revocation.ReasonCode, f.Revocation.CRL
		}
	}
	if _, desc := identityApprovers(pol, vf); desc != nil {
		h.PolicyDigest = desc.PolicyDigest()
	}
	b, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("encode the exec hold of %q: %w (%w)", s.ToolUseID, err, ErrStorage)
	}
	if _, _, err := j.putNew(ctx, runID, ApprovalExecHoldStep(s.ToolUseID, gen), Record{Kind: StepValue, Result: b}); err != nil {
		return fmt.Errorf("record %s (run %s): %w (%w)", ApprovalExecHoldStep(s.ToolUseID, gen), runID, err, ErrStorage)
	}
	return nil
}
