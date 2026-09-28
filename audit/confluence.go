package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bide-ai/bide/agent"
)

// ConvergenceContent is the payload of a convergence leaf: an opaque, serialized convergence
// certificate and the policy digest it certifies. Committing it as a journal leaf puts the
// convergence evidence under the same signed tree head and inclusion proofs as the policy and the
// actions taken under it, so an auditor can prove not merely that a policy was anchored but that
// the anchored policy was certified convergent in the same committed tree.
//
// audit treats the certificate as opaque bytes and the digest as an opaque identifier, exactly as
// it does for a policy leaf: it does not import gsm and does not recompute either. The producer of
// the certificate is govern.CertifyConvergence; a rigorous verifier re-derives the certificate
// from the disclosed policy bytes (by rebuilding the gsm machine) and compares, so a leaf that
// overstates convergence is caught outside audit, not trusted here.
type ConvergenceContent struct {
	Digest      string          `json:"digest"`      // the policy digest this certificate certifies (treated as opaque here)
	Certificate json.RawMessage `json:"certificate"` // the opaque, serialized convergence certificate bytes
}

// convergenceLeafName is the reserved journal name for a convergence leaf, keyed by digest so a
// run whose policy changes over its lifetime records each epoch's certificate as its own leaf.
func convergenceLeafName(digest string) string { return "audit:convergence:" + digest }

// RecordConvergence commits the serialized convergence certificate as a dedicated journal leaf
// (idempotent per (runID, digest)), so it is covered by the same STH and inclusion proofs as the
// policy leaf with the same digest and the actions taken under it. Pass the certificate bytes from
// govern.ConfluenceCertificate.Marshal and the same digest used for the policy leaf.
func RecordConvergence(ctx context.Context, store agent.Durable, runID string, cert []byte, digest string) (agent.Record, error) {
	content, err := json.Marshal(ConvergenceContent{Digest: digest, Certificate: json.RawMessage(cert)})
	if err != nil {
		return agent.Record{}, fmt.Errorf("audit: marshal convergence content: %w", err)
	}
	return store.Do(ctx, runID, convergenceLeafName(digest), func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: content}, nil
	})
}

// ProveConvergence builds a ProofBundle proving the convergence certificate for this digest was
// committed in the tree sth signs. Pair it with the policy leaf's ProofBundle for the same digest
// to show, in one committed tree, that the anchored policy was certified convergent.
func ProveConvergence(ctx context.Context, store agent.Durable, runID, digest string, sth SignedTreeHead) (ProofBundle, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return ProofBundle{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	name := convergenceLeafName(digest)
	idx := -1
	for i, r := range recs {
		if r.Kind == agent.StepValue && r.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ProofBundle{}, fmt.Errorf("audit: no convergence leaf for digest %q in run %s", digest, runID)
	}
	return ProveRecord(ctx, store, runID, idx, sth)
}
