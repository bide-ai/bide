package plan

import (
	"context"
	"errors"
	"fmt"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/escalation"
	"github.com/bide-ai/bide/internal/journalhook"
)

// ErrApprovalDenied is returned (wrapped, naming the node) by Flow.Run when a node's approval gate
// denied it: a SingleApproval gate's agent.Approve(false), or an m-of-n gate whose terminal tally
// did not pass. The node does not run. The denial is recorded, so every later drive of the run
// returns it again without running the node; the run stays unfinished until it is cancelled
// (agent.Cancel).
var ErrApprovalDenied = errors.New("approval denied")

// ErrApprovalExpired is returned (wrapped, naming the node) by Flow.Run when a node's approval gate
// expired: its deadline (agent.ApprovalPolicy.Timeout) passed with no approval. It wraps both
// agent.ErrApprovalExpired and ErrApprovalDenied: an expiry is a denial in every other respect, and
// every later drive returns it.
var ErrApprovalExpired = fmt.Errorf("%w (%w)", agent.ErrApprovalExpired, ErrApprovalDenied)

// protocol:flows begin GRead GPause TRefuse TGate TPause TPre PX

// taintState is a drive's standing against the taint declarations its run started under, and the flow's
// declarations (nil for a flow with no policy). A nil *taintState is a run that is not under taint.
type taintState struct {
	flow     *journalhook.FlowTaint
	standing int
}

// runGate holds node, run under its key key with the decoded input, at its gates. It returns nil when the node
// may run (it needs no gate, or its gates passed), *agent.ApprovalPending when a gate waits for decisions, an
// error wrapping ErrApprovalDenied when a gate denied it, and one wrapping ErrTaintRefused when the taint
// policy refuses it.
//
// The order is the design's (docs/design/flow-taint.md, A2): what taint requires of the node is computed from
// the journal and recorded; a refusal comes before any gate, so no approval is asked for a node that will not
// run; then the node's own gate, then the escalation gate on the node's instance (its subject is
// escalation.NodeInstance(key)), and last the pre-execution status checks of both, immediately before the
// node would run. Both gates are evaluated by the agent package (journalhook.Gate) with the records and the
// counting rule an agent's gated tool call uses; the node's input, encoded as the journal encodes it, is what
// the approvers of either gate sign (the subject's Args). All of this comes before the node's attempt claim.
func runGate(ctx context.Context, store *agent.Journal, runID string, c *builderCore, ts *taintState, node *node, key string, input any) (context.Context, error) {
	bodyCtx := ctx
	var escalationGate any
	if ts != nil && ts.standing != 0 {
		dec, err := journalhook.FlowTaintNodeHook(ctx, store, runID, key, node.safety, ts.standing)
		if err != nil {
			if errors.Is(err, journalhook.ErrTaintRefused) {
				return ctx, fmt.Errorf("plan: run %q: step %q (%s): %w: %w", c.flowName, node.name, key, ErrTaintRefused, err)
			}
			return ctx, fmt.Errorf("plan: run %q: the taint check of step %q: %w", c.flowName, node.name, err)
		}
		escalationGate = dec.Gate
		if dec.Ctx != nil {
			bodyCtx = dec.Ctx // the node's body runs under the run's taint status, its read set and its sink
		}
	}
	if node.approval == nil && escalationGate == nil {
		return bodyCtx, nil
	}
	args, err := journalhook.Marshal(input)
	if err != nil {
		return ctx, fmt.Errorf("plan: run %q: encode the input of gated step %q: %w", c.flowName, node.name, err)
	}
	var verifiers any
	if c.approverVerifiers != nil {
		verifiers = c.approverVerifiers
	}
	if node.approval != nil {
		if err := evalGate(ctx, store, runID, c, node, key, args, node.approval, verifiers, "approval gate"); err != nil {
			return ctx, err
		}
	}
	instance := escalation.NodeInstance(key)
	if escalationGate != nil {
		if err := evalGate(ctx, store, runID, c, node, instance, args, escalationGate, verifiers, "escalation gate"); err != nil {
			return ctx, err
		}
	}
	// The pre-execution status check of a gate with certificate-bound approvers, immediately before the
	// node would run (it does nothing for any other gate, and for a node whose result is recorded, which
	// replays and runs nothing). The escalation gate's is as the node's own.
	if node.approval != nil {
		if err := preExec(ctx, store, runID, c, node, key, node.approval, verifiers); err != nil {
			return ctx, err
		}
	}
	if escalationGate != nil {
		if err := preExec(ctx, store, runID, c, node, instance, escalationGate, verifiers); err != nil {
			return ctx, err
		}
	}
	return bodyCtx, nil
}

// evalGate evaluates the gate pol on subject, the node's key or its escalation instance, and returns nil
// when it passed, *agent.ApprovalPending when it waits, and an error wrapping ErrApprovalDenied when it
// denied the node.
func evalGate(ctx context.Context, store *agent.Journal, runID string, c *builderCore, node *node, subject string, args []byte, pol any, verifiers any, what string) error {
	approved, err := journalhook.Gate(ctx, store, runID, subject, node.name, args, pol, verifiers)
	if err != nil {
		var pend *agent.ApprovalPending
		if errors.As(err, &pend) {
			return err
		}
		if errors.Is(err, agent.ErrApprovalExpired) {
			return fmt.Errorf("plan: run %q: step %q (%s): %w: %w", c.flowName, node.name, subject, ErrApprovalExpired, err)
		}
		return fmt.Errorf("plan: run %q: %s of step %q: %w", c.flowName, what, node.name, err)
	}
	if !approved {
		return fmt.Errorf("plan: run %q: step %q (%s): %w", c.flowName, node.name, subject, ErrApprovalDenied)
	}
	return nil
}

// preExec is the pre-execution status check of the gate pol on subject.
func preExec(ctx context.Context, store *agent.Journal, runID string, c *builderCore, node *node, subject string, pol any, verifiers any) error {
	if err := journalhook.PreExec(ctx, store, runID, subject, pol, verifiers); err != nil {
		var pend *agent.ApprovalPending
		if errors.As(err, &pend) {
			return err
		}
		return fmt.Errorf("plan: run %q: the check before step %q runs: %w", c.flowName, node.name, err)
	}
	return nil
}

// protocol:flows end
