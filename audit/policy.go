package audit

import (
	"context"
	"encoding/json"
	"fmt"

	agent "github.com/dayna/go-agents"
)

// PolicyContent is the payload of a policy leaf: the serialized governed policy and its digest.
// Committing it as a journal leaf puts the policy itself under the same signed tree head and
// inclusion proofs as the actions taken under it, so an auditor can prove not merely that an
// action happened but that the exact policy admitting it was anchored in the same committed tree.
type PolicyContent struct {
	Digest string `json:"digest"`
	Policy string `json:"policy"`
}

// policyLeafName is the reserved journal name for a policy leaf, keyed by digest so a run that
// changes policy over its lifetime records each epoch as its own leaf.
func policyLeafName(digest string) string { return "audit:policy:" + digest }

// RecordPolicy commits the serialized policy as a dedicated journal leaf (idempotent per
// (runID, digest)), so the policy is covered by the same STH and inclusion proofs as the actions
// taken under it. The digest is the policy owner's stable identifier (e.g.
// gsm.Registry.PolicyDigest); audit treats it as opaque and does not recompute it from the bytes.
// A verifier recomputes the digest from the disclosed bytes and runs the external oracle
// (goagents-audit verify-governance), so a leaf that lies about its digest is caught there.
func RecordPolicy(ctx context.Context, store agent.Durable, runID string, policy []byte, digest string) (agent.Record, error) {
	content, err := json.Marshal(PolicyContent{Digest: digest, Policy: string(policy)})
	if err != nil {
		return agent.Record{}, fmt.Errorf("audit: marshal policy content: %w", err)
	}
	return store.Do(ctx, runID, policyLeafName(digest), func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: content}, nil
	})
}

// ProvePolicy builds a ProofBundle proving the policy with this digest was committed in the tree
// sth signs. Pair it with an action's ProofBundle whose result embeds the same digest to show the
// action ran under an in-log, anchored policy; the verifier then recomputes the digest from the
// disclosed bytes and runs the external oracle to certify the policy converges.
func ProvePolicy(ctx context.Context, store agent.Durable, runID, digest string, sth SignedTreeHead) (ProofBundle, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return ProofBundle{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	name := policyLeafName(digest)
	idx := -1
	for i, r := range recs {
		if r.Kind == agent.StepValue && r.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ProofBundle{}, fmt.Errorf("audit: no policy leaf for digest %q in run %s", digest, runID)
	}
	return ProveRecord(ctx, store, runID, idx, sth)
}
