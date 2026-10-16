package plan

import (
	"context"
	"errors"
	"fmt"

	"github.com/bide-ai/bide/agent"
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

// protocol:flows begin GRead GPause

// runGate holds node, run under its key key with the decoded input, at its approval gate. It
// returns nil when the node may run (it has no gate, or its gate passed), *agent.ApprovalPending
// when the gate waits for decisions, and an error wrapping ErrApprovalDenied when it denied the
// node. The gate is evaluated by the agent package (journalhook.Gate) with the records and the
// counting rule an agent's gated tool call uses; the node's input, encoded as the journal encodes
// it, is what the approvers sign (the subject's Args).
func runGate(ctx context.Context, store *agent.Journal, runID string, c *builderCore, node *node, key string, input any) error {
	if node.approval == nil {
		return nil
	}
	args, err := journalhook.Marshal(input)
	if err != nil {
		return fmt.Errorf("plan: run %q: encode the input of gated step %q: %w", c.flowName, node.name, err)
	}
	var verifiers any
	if c.approverVerifiers != nil {
		verifiers = c.approverVerifiers
	}
	approved, err := journalhook.Gate(ctx, store, runID, key, node.name, args, node.approval, verifiers)
	if err != nil {
		var pend *agent.ApprovalPending
		if errors.As(err, &pend) {
			return err
		}
		if errors.Is(err, agent.ErrApprovalExpired) {
			return fmt.Errorf("plan: run %q: step %q (%s): %w: %w", c.flowName, node.name, key, ErrApprovalExpired, err)
		}
		return fmt.Errorf("plan: run %q: approval gate of step %q: %w", c.flowName, node.name, err)
	}
	if !approved {
		return fmt.Errorf("plan: run %q: step %q (%s): %w", c.flowName, node.name, key, ErrApprovalDenied)
	}
	// The pre-execution status check of a gate with certificate-bound approvers, immediately before the
	// node would run (it does nothing for any other gate, and for a node whose result is recorded, which
	// replays and runs nothing).
	if err := journalhook.PreExec(ctx, store, runID, key, node.approval, verifiers); err != nil {
		var pend *agent.ApprovalPending
		if errors.As(err, &pend) {
			return err
		}
		return fmt.Errorf("plan: run %q: the check before step %q runs: %w", c.flowName, node.name, err)
	}
	return nil
}

// protocol:flows end
