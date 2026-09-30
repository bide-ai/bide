package audit

import (
	"context"
	"fmt"

	"github.com/bide-ai/bide/agent"
)

// A ProofBundle is a single, portable, self-describing artifact that proves ONE action
// happened inside a signed, committed run. It discloses the record (with its salt) as the bytes
// the journal stores for it, its index, the run's size and signed head, and the sibling hashes on
// its path, and no other record: each record's leaf commits to its own random salt, so the sibling
// hashes cannot be tested against a guessed neighbour (see merkle.go). It is what "produce a proof"
// returns: hand it (plus the signer's public key, obtained out-of-band) to an auditor and they
// verify it offline.
//
// The record is carried as RecordBytes, the stored bytes verbatim, and the verifier hashes exactly
// those bytes: it never re-encodes a decoded record. So a record a later release wrote, with
// fields this release does not know, verifies here, and what a proof commits to cannot depend on
// how this release encodes a record. Record decodes the bytes, leniently (unknown fields are
// ignored), for display and for the checks that read what the record is (its kind, its name).
//
// The trust model, stated precisely: the public key must come from OUT OF BAND (the anchor /
// transparency log), NOT from the bundle. Given a trusted key, Verify checks (1) the STH's
// signature, so the {Kind, RunID, Size, Root, TimestampNanos} commitment is authentic; (2) that
// the STH commits to a journal and to the bundle's RunID, so the run a verifier reports is the run
// the signer committed to; (3) that the inclusion proof is against that same signed size; and (4)
// that the record bytes are the leaf at their index under the signed root. "Proofs you verify, not
// logs you trust": nothing here asks the verifier to trust the producer, its database, or its
// logs. For full tamper-evidence the auditor also confirms the STH itself appears in the anchor
// log (MemAnchorLog.Prove).
type ProofBundle struct {
	Format      string         `json:"format"`       // ProofFormat
	RunID       string         `json:"run_id"`       // the run whose journal the record and STH belong to; must equal STH.RunID
	RecordBytes []byte         `json:"record_bytes"` // the single disclosed action: the bytes the journal stores for it
	Inclusion   Inclusion      `json:"inclusion"`    // its RFC 6962 audit path
	STH         SignedTreeHead `json:"sth"`          // the signed commitment it is proven against
}

// Record decodes the proven record from RecordBytes, for display and role checks. The decoding is
// lenient, as the journal's own: a field this version does not know is ignored. It errors (wrapping
// ErrMalformed) only if the bytes do not decode as a journal record.
func (b ProofBundle) Record() (agent.Record, error) { return decodeRecordBytes(b.RecordBytes) }

// Verify returns nil if the bundle is internally consistent and authentic under v (a key obtained
// out-of-band, e.g. from the anchor log operator): the STH verifies under v, it is a journal head of
// RunID, the inclusion proof is for the signed size, and RecordBytes are the leaf at their index
// under the signed root. It then requires RecordBytes to decode as a record (ErrMalformed if not).
// A bundle that does not hold is an error wrapping ErrNotVerified; one of another format, ErrFormat.
// It does not check the head's timestamp; apply CheckTimestamp to b.STH for that.
func (b ProofBundle) Verify(v Verifier) error {
	if err := formatOf(b, b.Format); err != nil {
		return err
	}
	if err := b.STH.Verify(v); err != nil {
		return err
	}
	if b.STH.Kind != TreeJournal || b.STH.RunID != b.RunID {
		return notVerified("audit: the proof's head is a %q head of run %q, not the journal of run %q", b.STH.Kind, b.STH.RunID, b.RunID)
	}
	if b.Inclusion.Size != b.STH.Size {
		return notVerified("audit: the inclusion proof is for a tree of %d records, not the signed %d", b.Inclusion.Size, b.STH.Size)
	}
	if err := VerifyInclusion(b.STH.Root, b.RecordBytes, b.Inclusion); err != nil {
		return err
	}
	_, err := b.Record()
	return err
}

// ProveRecord builds a bundle proving the record at index is in the tree that sth commits to.
// The inclusion proof is built against the first sth.Size records of runID's journal (the tree
// the STH signed), so the bundle binds to an anchored STH rather than a freshly minted one.
// It fails if index is outside that tree, if the record is redacted, or if sth.Root does not
// match runID's journal at that size (wrong STH, or the history diverged from what was signed).
func ProveRecord(ctx context.Context, store agent.Durable, runID string, index int, sth SignedTreeHead) (ProofBundle, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return ProofBundle{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	return proveIn(runID, recs, index, sth)
}

// proveIn is ProveRecord over recs, runID's journal as read.
func proveIn(runID string, recs []agent.Record, index int, sth SignedTreeHead) (ProofBundle, error) {
	recs, err := journalPrefix(runID, recs, sth.TreeHead)
	if err != nil {
		return ProofBundle{}, err
	}
	if index < 0 || index >= sth.Size {
		return ProofBundle{}, fmt.Errorf("audit: record %d is not in the tree the STH commits to (size %d)", index, sth.Size)
	}
	if _, err := canonicalRecord(recs[index]); err != nil {
		return ProofBundle{}, fmt.Errorf("audit: record %d: %w", index, err)
	}
	if _, err := decodeRecordBytes(recs[index].Raw()); err != nil {
		return ProofBundle{}, fmt.Errorf("audit: record %d: %w", index, err)
	}
	leaves, err := journalLeafHashes(recs)
	if err != nil {
		return ProofBundle{}, err
	}
	return ProofBundle{
		Format:      ProofFormat,
		RunID:       runID,
		RecordBytes: recs[index].Raw(),
		Inclusion:   Inclusion{Index: index, Size: sth.Size, Path: hashPath(index, leaves)},
		STH:         sth,
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
