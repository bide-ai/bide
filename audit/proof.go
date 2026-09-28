package audit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"

	"github.com/blackwell-systems/bide/agent"
)

// A ProofBundle is a single, portable, self-describing artifact that proves ONE action
// happened inside a signed, committed run, disclosing nothing else. It is what "produce a
// proof" returns: hand it (plus the signer's public key, obtained out-of-band) to an auditor
// and they verify it offline. This turns the audit package's separate pieces (a record, its
// inclusion path, a signed tree head) into one thing you can marshal to JSON, store, email, or
// publish.
//
// The trust model, stated precisely: the public key must come from OUT OF BAND (the anchor /
// transparency log), NOT from the bundle. Given a trusted key, Verify checks (1) the STH's
// signature, so the {Size, Root, Timestamp} commitment is authentic; (2) that the inclusion
// proof is against that same signed size; and (3) that the disclosed record is the leaf at its
// index under the signed root. "Proofs you verify, not logs you trust": nothing here asks the
// verifier to trust the producer, its database, or its logs. For full tamper-evidence the
// auditor also confirms the STH itself appears in the anchor log (MemAnchorLog.Prove).
type ProofBundle struct {
	RunID     string         `json:"run_id"`    // the run whose journal the record and STH belong to
	Record    agent.Record   `json:"record"`    // the single disclosed action
	Inclusion Inclusion      `json:"inclusion"` // its RFC 6962 audit path
	STH       SignedTreeHead `json:"sth"`       // the signed commitment it is proven against
}

// Verify reports whether the bundle is internally consistent and authentic under pub (a key
// obtained out-of-band, e.g. from the anchor log operator). It returns false, not an error,
// for a well-formed-but-invalid proof; an error indicates the record could not be canonicalized.
func (b ProofBundle) Verify(pub ed25519.PublicKey) (bool, error) {
	if !b.STH.Verify(pub) {
		return false, nil // the signed commitment is not authentic under this key
	}
	if b.Inclusion.Size != b.STH.Size {
		return false, nil // the proof is not bound to the tree the STH signed
	}
	return VerifyInclusion(b.STH.Root, b.Record, b.Inclusion)
}

// ProveRecord builds a bundle proving the record at index is in the tree that sth commits to.
// The inclusion proof is built against the first sth.Size records of runID's journal (the tree
// the STH signed), so the bundle binds to an anchored STH rather than a freshly minted one.
// It fails if index is outside that tree, or if sth.Root does not match runID's journal at
// that size (wrong STH, or the history diverged from what was signed).
func ProveRecord(ctx context.Context, store agent.Durable, runID string, index int, sth SignedTreeHead) (ProofBundle, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return ProofBundle{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	if sth.Size < 0 || sth.Size > len(recs) {
		return ProofBundle{}, fmt.Errorf("audit: STH size %d out of range for run %s (%d records)", sth.Size, runID, len(recs))
	}
	if index < 0 || index >= sth.Size {
		return ProofBundle{}, fmt.Errorf("audit: record %d is not in the tree the STH commits to (size %d)", index, sth.Size)
	}
	leaves, err := canonicalLeaves(recs[:sth.Size])
	if err != nil {
		return ProofBundle{}, err
	}
	if !bytes.Equal(sth.Root, merkleRoot(leaves)) {
		return ProofBundle{}, fmt.Errorf("audit: STH root does not match run %s at size %d (wrong STH or history diverged)", runID, sth.Size)
	}
	return ProofBundle{
		RunID:     runID,
		Record:    recs[index],
		Inclusion: Inclusion{Index: index, Size: sth.Size, Path: auditPath(index, leaves)},
		STH:       sth,
	}, nil
}

// ProveToolCall is the semantic form of ProveRecord: prove that a specific tool call happened,
// by its ToolUseID, without the caller needing to know its journal index. It resolves the
// completed tool-result record for toolUseID and proves it against sth. This is the ergonomic
// entry point ("prove the charge on run X"), not "prove record index 7".
func ProveToolCall(ctx context.Context, store agent.Durable, runID, toolUseID string, sth SignedTreeHead) (ProofBundle, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return ProofBundle{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	idx := -1
	for i, r := range recs {
		if r.Kind == agent.StepToolResult && r.ToolUseID == toolUseID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ProofBundle{}, fmt.Errorf("audit: no completed tool call %q in run %s", toolUseID, runID)
	}
	return ProveRecord(ctx, store, runID, idx, sth)
}
