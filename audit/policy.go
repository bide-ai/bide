package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bide-ai/bide/agent"
)

// PolicyContent is the payload of a policy leaf: the serialized governed policy and its digest.
// Committing it as a journal leaf puts the policy itself under the same signed tree head and
// inclusion proofs as the actions taken under it, so an auditor can prove not merely that an
// action happened but that the exact policy admitting it was anchored in the same committed tree.
type PolicyContent struct {
	Digest string `json:"digest"` // the policy owner's stable identifier (treated as opaque here)
	Policy string `json:"policy"` // the serialized governed policy bytes
}

// policyLeafName is the reserved journal name for a policy leaf, keyed by digest so a run that
// changes policy over its lifetime records each epoch as its own leaf.
func policyLeafName(digest string) string { return "audit:policy:" + digest }

// PolicyLeafName is the journal name of the policy leaf RecordPolicy writes for digest. A
// verifier handed a bundle as a policy leaf checks that its record is a StepValue of this name:
// any other record, such as a tool result whose output has a policy leaf's shape, anchors no
// policy.
func PolicyLeafName(digest string) string { return policyLeafName(digest) }

// RecordPolicy commits the serialized policy as a dedicated journal leaf (idempotent per
// (runID, digest)), so the policy is covered by the same STH and inclusion proofs as the actions
// taken under it. The digest is the policy owner's stable identifier (e.g.
// gsm.Registry.PolicyDigest); audit treats it as opaque and does not recompute it from the bytes.
// A verifier recomputes the digest from the disclosed bytes and runs the external oracle
// (bide-audit verify-governance), so a leaf that lies about its digest is caught there.
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

// policyUsedKeyPrefix namespaces the absence key set to policy digests exercised by governed
// actions, so it does not collide with other key sets (e.g. ToolUseKeys) over the same run.
const policyUsedKeyPrefix = "policy_used:"

// PolicyUsedKey is the KeyFunc of PolicyUsedKeys (see ProveAbsent / ProveAbsentBundle) over governed-action leaves:
// a completed tool call whose result carries a policy_digest, as govern.AttestedEventTool
// records. It keys by that digest, so the absence machinery commits the set of policies actually
// exercised in a run. Proving a digest ABSENT under this KeyFunc shows that no governed action
// ran under that policy; recomputing AbsenceRoot lists exactly which policies were used, so an
// auditor can confirm every one is in the approved set.
//
// Scope: this is a policy-level negative ("no action ran under a disallowed policy"), which,
// combined with the approved policies being oracle-certified convergent and invariant-preserving,
// supports "no violation was admitted". It is not a per-action state-validity proof (a
// per-transition state digest is roadmap), and it does not close the runtime refinement gap: the
// runtime is differentially tested against the verified reference, not proven equal to it.
func PolicyUsedKey(r agent.Record) (string, bool) {
	if r.Kind != agent.StepToolResult || len(r.Result) == 0 {
		return "", false
	}
	digest, err := GovernedPolicyDigest(r.Result)
	if err != nil || digest == "" {
		return "", false
	}
	return policyUsedKeyPrefix + digest, true
}

// GovernedPolicyDigest returns the policy digest a governed-action payload (a tool result, as
// govern.AttestedEventTool journals it) carries. It is the one reading PolicyUsedKey and bide-audit
// share, so an action cannot be said to run under one policy by the used-policy set and under
// another by the CLI. The payload is open (it may hold other fields, such as the acting identity),
// so its names are not checked against a type, but it must decode strictly as an object (see
// UnmarshalStrict: no duplicate names, valid UTF-8, no lone surrogate escapes), and the digest is
// read from the exact name "policy_digest" only, never from a case variant, as a reader of the
// file would read it. A payload without that name, or whose value is not a string, is an error.
func GovernedPolicyDigest(result json.RawMessage) (string, error) {
	var fields map[string]json.RawMessage
	if err := UnmarshalStrict(result, &fields); err != nil {
		return "", err
	}
	raw, ok := fields["policy_digest"]
	if !ok {
		return "", errors.New(`audit: no "policy_digest"`)
	}
	var digest string
	if err := UnmarshalStrict(raw, &digest); err != nil {
		return "", fmt.Errorf("audit: policy_digest: %w", err)
	}
	return digest, nil
}

// PolicyUsedKeyFor is the absence key for a specific policy digest: pass it to ProveAbsent /
// ProveAbsentBundle with PolicyUsedKeys to prove no governed action ran under that policy.
func PolicyUsedKeyFor(digest string) string { return policyUsedKeyPrefix + digest }

// PoliciesUsed returns the sorted, distinct policy digests exercised by governed actions in the
// run. A redacted record's action cannot be read, so over a journal holding one the result may
// omit a policy the run used; CertifyRun refuses such a journal (ErrRedacted). An auditor compares this against the approved set; for any disallowed digest it then
// obtains an absence proof (ProveAbsentBundle with PolicyUsedKeys) showing no action ran under it.
func PoliciesUsed(records []agent.Record) []string {
	keys := absenceKeys(records, PolicyUsedKeys)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = strings.TrimPrefix(k, policyUsedKeyPrefix)
	}
	return out
}
