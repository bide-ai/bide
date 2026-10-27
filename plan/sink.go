package plan

import (
	"context"
	"fmt"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/journalhook"
)

// protocol:flows begin NSink

// sendMarks sends the marks the run has learned up to the node that started it (journalhook.FlowTaintSink): the
// sources of node key once its result is recorded, and those its own children brought up, or every node's for
// key "". It runs after the node's result is recorded, whether the drive ran the node or replayed it, so a
// mark is sent on every drive. The run's parent is the node whose sink ctx carries: a run no node started has
// nowhere to send them, and nothing happens. It does nothing for a run that is not under taint.
func sendMarks(ctx context.Context, store *agent.Journal, runID string, c *builderCore, ts *taintState, node *node, key string) error {
	if ts != nil && ts.standing == 0 {
		return nil
	}
	if err := journalhook.FlowTaintSink(ctx, store, runID, key); err != nil {
		what := "the run"
		if node != nil {
			what = fmt.Sprintf("step %q", node.name)
		}
		return fmt.Errorf("plan: run %q: send the marks of %s up to the node that started it: %w", c.flowName, what, err)
	}
	return nil
}

// protocol:flows end
