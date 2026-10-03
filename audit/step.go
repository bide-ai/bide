package audit

import (
	"context"
	"fmt"

	"github.com/bide-ai/bide/agent"
)

// ProveStep builds a ProofBundle proving that the durable step with this name (an agent.Step /
// Parallel task, recorded as a StepValue) is committed in the tree sth signs. It is the fan-in
// counterpart to ProveToolCall: after a Parallel run, each task is an independently provable
// record, so an auditor can prove "this specific compliance check ran and produced this result"
// without disclosing the other stages.
func ProveStep(ctx context.Context, store *agent.Journal, runID, name string, sth SignedTreeHead) (ProofBundle, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return ProofBundle{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	idx := -1
	for i, r := range recs {
		if r.Kind == agent.StepValue && r.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ProofBundle{}, fmt.Errorf("audit: no completed step %q in run %s", name, runID)
	}
	return ProveRecord(ctx, store, runID, idx, sth)
}
