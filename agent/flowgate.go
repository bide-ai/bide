package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/bide-ai/bide/internal/strictjson"
)

// This file holds the approval gate of a plan flow node. A gated node pauses before it runs, as an
// agent's gated tool call does, and is decided by the same records and the same counting rule:
// Approve for a SingleApproval gate, SubmitDecision and TallyApprovals for an m-of-n gate, the
// terminal tally under ApprovalTallyStep, and ApprovalWait under ApprovalPendingStep. The call the
// approvers decide on is the node: its key ("node:<name>", or "node:iter:<n>:<name>" in a loop
// body) is the ToolUseID, its name the ToolName, and its input, as JSON, the Args. Package plan
// reaches it through internal/journalhook.Gate.

// ApprovalRequestStep is the journal name of the request a plan flow node's approval gate records
// when the run first reaches it (see ApprovalRequest). toolUseID is the node's key.
func ApprovalRequestStep(toolUseID string) string { return "approval-request:" + encodeID(toolUseID) }

// ApprovalRequest is what the record named ApprovalRequestStep holds: the gated plan flow node
// approvers decide on. It is the flow counterpart of the model turn that requests an agent's tool
// call, and it is what FindToolCall finds for the node's key: ToolUseID is the node's key, ToolName
// its name, and Args its input as JSON. It is recorded once, before the first pause, and a later
// drive whose input differs from it is refused (ErrConfig), so every decision is checked against
// the input the node then runs with.
type ApprovalRequest struct {
	ToolUseID string          `json:"tool_use_id"`
	ToolName  string          `json:"tool_name"`
	Args      json.RawMessage `json:"args"`
}

// decodeApprovalRequest reads r as the request of the gate on toolUseID: a StepValue named
// ApprovalRequestStep(toolUseID), on a plan node key, whose Result decodes strictly and names
// toolUseID.
func decodeApprovalRequest(r Record, toolUseID string) (ApprovalRequest, bool) {
	if r.Kind != StepValue || r.Name != ApprovalRequestStep(toolUseID) || !planNodeKey(toolUseID) {
		return ApprovalRequest{}, false
	}
	var q ApprovalRequest
	if err := strictjson.Unmarshal(r.Result, &q, nil); err != nil || q.ToolUseID != toolUseID {
		return ApprovalRequest{}, false
	}
	return q, true
}

// flowDecision checks a decision on toolUseID before it is recorded. It applies only to the gate of
// a plan flow node: toolUseID is a node key and the run is a flow's (its recorded start is a flow's,
// or it has none yet, as before a flow's first drive); an agent's tool call, whatever its ID looks
// like, is not checked, and flow is false. For a flow node it refuses (ErrConfig) a decision when
// the gate has recorded no request yet, since the node's input is not fixed until then, and
// returns the request's journal position for staleDecision.
func flowDecision(ctx context.Context, d *Journal, op, runID, toolUseID string) (reqSeq int64, flow bool, err error) {
	if !planNodeKey(toolUseID) {
		return 0, false, nil
	}
	start, ok, err := RecordedStart(ctx, d, runID)
	if err != nil {
		return 0, false, fmt.Errorf("%s: %w", op, err)
	}
	if ok && start.kind() != RunKindFlow {
		return 0, false, nil
	}
	q, ok, err := d.getEntry(ctx, runID, ApprovalRequestStep(toolUseID))
	if err != nil {
		return 0, false, fmt.Errorf("%s: read %s (run %s): %w (%w)", op, ApprovalRequestStep(toolUseID), runID, err, ErrStorage)
	}
	if !ok {
		return 0, false, fmt.Errorf("%s: run %s has not reached the approval gate of %s, so it records no request to decide on yet: %w", op, runID, toolUseID, ErrConfig)
	}
	return q.Seq, true, nil
}

// staleDecision reports (ErrConfig) a decision whose journal name was already taken by a record
// written before the gate's request, at reqSeq: that record cannot count, and this decision, having
// the same name, was not recorded. For a SingleApproval gate the name is the gate's one decision, so
// the gate cannot pass; for an m-of-n gate it is this approver's identical decision, so their seat
// is lost for this gate.
func staleDecision(ctx context.Context, d *Journal, op, runID, toolUseID, name string, reqSeq int64, single bool) error {
	e, ok, err := d.getEntry(ctx, runID, name)
	if err != nil {
		return fmt.Errorf("%s: read %s (run %s): %w (%w)", op, name, runID, err, ErrStorage)
	}
	if !ok || e.Seq > reqSeq {
		return nil
	}
	if single {
		return fmt.Errorf("%s: a decision on %s was recorded before the gate's request, so it cannot count, and it holds the gate's one decision: the gate cannot pass; cancel the run (agent.Cancel): %w", op, toolUseID, ErrConfig)
	}
	return fmt.Errorf("%s: this approver's identical decision on %s was recorded before the gate's request, so it cannot count and this one was not recorded: their seat is lost for this gate; it can still pass with the other approvers if Need is reachable, otherwise cancel the run (agent.Cancel): %w", op, toolUseID, ErrConfig)
}

// protocol:flows begin GRead GCount

// flowGate evaluates the approval gate pol of the plan flow node named node, under its key key, in
// run runID, for the node's input args (JSON). It reports approved=true when the node may run and
// false when the gate denied it; a gate still waiting returns *ApprovalPending.
//
// The order is the agent's: the policy and its seats are checked on every evaluation, then a
// recorded outcome decides (Approve's record for a SingleApproval gate, the terminal tally for an
// m-of-n gate, one point read each), so a node whose gate passed costs one read on replay. With no
// outcome recorded, the request is recorded (once; a recorded request whose node or input differs
// from this drive's is ErrConfig), an m-of-n gate counts the journal (tallyGate, which records a
// terminal tally before the node runs), and a gate with no outcome pauses. Only decisions recorded
// after the request count. A pause records ApprovalWait if it is missing; no part of the engine
// reads it.
func flowGate(ctx context.Context, d *Journal, runID, key, node string, args json.RawMessage, pol *ApprovalPolicy, verifierFor ApproverVerifierFor) (bool, error) {
	if !planNodeKey(key) {
		return false, fmt.Errorf("plan node %q: gate key %q is not a plan node key: %w", node, key, ErrConfig)
	}
	if pol == nil {
		return false, fmt.Errorf("plan node %q: no approval policy: %w", node, ErrConfig)
	}
	if err := pol.Validate(); err != nil {
		return false, fmt.Errorf("plan node %q: %w", node, err)
	}
	single := pol.single()
	if !single {
		if verifierFor == nil {
			return false, fmt.Errorf("plan node %q has an m-of-n approval gate but the flow has no approver verifiers: %w", node, ErrConfig)
		}
		if _, _, err := approverSeats(*pol, verifierFor); err != nil {
			return false, fmt.Errorf("plan node %q: %w", node, err)
		}
	}

	// GRead: a recorded outcome is final. A single decision counts only if it follows the gate's
	// request in the journal: one recorded before it (node keys are predictable) was not made on
	// the node's input.
	if single {
		e, ok, err := d.getEntry(ctx, runID, approvalStep(key))
		if err != nil {
			return false, fmt.Errorf("read %s (run %s): %w (%w)", approvalStep(key), runID, err, ErrStorage)
		}
		if ok {
			q, qok, err := d.getEntry(ctx, runID, ApprovalRequestStep(key))
			if err != nil {
				return false, fmt.Errorf("read %s (run %s): %w (%w)", ApprovalRequestStep(key), runID, err, ErrStorage)
			}
			r, derr := decodeStored(runID, approvalStep(key), e.Data)
			if derr != nil {
				return false, derr
			}
			if qok && q.Seq < e.Seq && r.Kind == StepApproval && r.Approver() == "" {
				return r.Approved, nil
			}
		}
	} else {
		r, ok, err := d.Get(ctx, runID, ApprovalTallyStep(key))
		if err != nil {
			return false, fmt.Errorf("read %s (run %s): %w (%w)", ApprovalTallyStep(key), runID, err, ErrStorage)
		}
		if ok && r.Kind == StepValue {
			t, err := decodeTally(runID, r)
			if err != nil {
				return false, err
			}
			return t.Passed(), nil
		}
	}
	b, err := marshalJournal(ApprovalRequest{ToolUseID: key, ToolName: node, Args: args})
	if err != nil {
		return false, fmt.Errorf("plan node %q: encode the approval request: %w (%w)", node, err, ErrConfig)
	}
	got, _, err := d.putNew(ctx, runID, ApprovalRequestStep(key), Record{Kind: StepValue, Result: b})
	if err != nil {
		return false, err
	}
	req, ok := decodeApprovalRequest(got, key)
	if !ok {
		return false, fmt.Errorf("plan node %q: the recorded %s of run %s does not read as its request: %w", node, ApprovalRequestStep(key), runID, ErrConfig)
	}
	if req.ToolName != node || !sameJSON(req.Args, args) {
		return false, fmt.Errorf("plan node %q: run %s recorded the gate's request for node %q with input %s, not this drive's %s; resume it with the flow and input it started with: %w", node, runID, req.ToolName, req.Args, args, ErrConfig)
	}

	// GCount: an m-of-n gate counts the journal and records a terminal tally.
	var quorum *ApprovalTally
	if !single {
		s := ApprovalSubject{RunID: runID, ToolUseID: key, ToolName: node, Args: req.Args}
		t, final, err := tallyGate(ctx, d, runID, s, pol, verifierFor, ApprovalRequestStep(key))
		if err != nil {
			return false, err
		}
		if final {
			return t.Passed(), nil
		}
		quorum = &t
	}
	// The pause note, if it is missing (one Insert that keeps a recorded one), so a crash between
	// the request and the note does not hide the wait from a viewer.
	w := ApprovalWait{ToolUseID: key, ToolName: node, Single: single}
	if !single {
		w.Need, w.Approvers = pol.Need, slices.Clone(pol.Approvers)
	}
	if b, err := json.Marshal(w); err == nil {
		_, _ = d.put(ctx, runID, ApprovalPendingStep(key), Record{Kind: StepValue, Result: b})
	}
	return false, &ApprovalPending{RunRef: RunRef{RunID: runID, RootRunID: runID}, ToolUseID: key, ToolName: node, Args: req.Args, Quorum: quorum}
}

// protocol:flows end
