package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// Anchoring is what upgrades INTEGRITY to TAMPER-EVIDENCE. A hash chain / Merkle root stored
// in the same database an attacker controls can be rewritten and rehashed; the guarantee only
// bites once you commit the signed head to a trust domain the app tier does NOT fully control.
// This file is the bring-your-own port for that (Anchor), plus a reference external
// transparency log (MemAnchorLog) that is itself append-only and independently verifiable.

// Anchor is the bring-your-own port for out-of-band anchoring: publish a SignedTreeHead to a
// trust domain separate from the app (a Certificate-Transparency-style log, a notary /
// timestamping service, another account's WORM store, a public ledger). Bide ships
// MemAnchorLog as a reference; you implement Publish against the external log you trust.
//
// Order: one AuditedStore publishes a run's heads in increasing size. When two processes anchor the
// same run (around a lease handoff), a smaller head can arrive after a larger one. Both are valid
// commitments to prefixes of one journal, so a monitor should check consistency between a run's
// heads ordered by size, not by arrival.
type Anchor interface {
	Publish(ctx context.Context, runID string, sth SignedTreeHead) error
}

// AnchorEntry is one published commitment recorded by a transparency log: the run it commits
// to, its signed tree head, and the entry's position in the anchor log.
//
// Anchor leaves are not salted. An anchor-log proof's path hashes cover neighbouring entries, and a
// neighbour's sequence number and run ID may be guessable, but its signed head is not: the head's
// root commits to salted leaves and its Ed25519 signature needs the signing key, so a proof holder
// can confirm a neighbouring entry only by already holding that exact signed head, which tells
// them only that the head they hold was anchored there. A head published without a signature has
// no such entropy; do not anchor one.
type AnchorEntry struct {
	Seq   int            `json:"seq"`    // the entry's position in the anchor log
	RunID string         `json:"run_id"` // the run whose commitment was published
	STH   SignedTreeHead `json:"sth"`    // the published signed tree head
}

// MemAnchorLog is a reference external transparency log: an append-only, independently
// Merkle-committed record of published STHs. In a real deployment the anchor lives in a
// DIFFERENT trust domain than the journal (a separate service / account / public log); this
// in-memory version is for tests and local dev. Because it keeps its OWN RFC 6962 tree over
// the entries, a third party can verify that the anchor log itself only grew (ProveConsistency)
// and that a specific STH was anchored (Prove) — the properties that make anchoring meaningful.
type MemAnchorLog struct {
	mu      sync.Mutex
	entries []AnchorEntry
}

// NewMemAnchorLog returns an empty transparency log.
func NewMemAnchorLog() *MemAnchorLog { return &MemAnchorLog{} }

var _ Anchor = (*MemAnchorLog)(nil)

// Publish appends an STH for runID at the next sequence. Append-only: entries are never
// modified or removed. It refuses a head that commits to a different run than runID, so an
// entry's RunID is always the run its signed head names.
func (l *MemAnchorLog) Publish(_ context.Context, runID string, sth SignedTreeHead) error {
	if sth.RunID != runID {
		return fmt.Errorf("audit: anchor: tree head is for run %q, not %q", sth.RunID, runID)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, AnchorEntry{Seq: len(l.entries), RunID: runID, STH: sth})
	return nil
}

// Len is the number of anchored entries.
func (l *MemAnchorLog) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// Entries returns a copy of the anchored entries in order.
func (l *MemAnchorLog) Entries() []AnchorEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]AnchorEntry(nil), l.entries...)
}

func (l *MemAnchorLog) leaves() ([][]byte, error) {
	out := make([][]byte, len(l.entries))
	for i, e := range l.entries {
		b, err := canonicalAnchorEntry(e)
		if err != nil {
			return nil, err
		}
		out[i] = b
	}
	return out, nil
}

// Root is the RFC 6962 Merkle root over all anchored entries — the anchor log's own
// commitment. Publish/sign IT in turn (or expose it) so a monitor can check the log's growth.
func (l *MemAnchorLog) Root() ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	leaves, err := l.leaves()
	if err != nil {
		return nil, err
	}
	return merkleRoot(leaves), nil
}

// Prove returns an inclusion proof that the entry at seq is in the anchor log committed by
// Root — i.e. that a given STH was anchored, disclosing no other entry.
func (l *MemAnchorLog) Prove(seq int) (Inclusion, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if seq < 0 || seq >= len(l.entries) {
		return Inclusion{}, fmt.Errorf("audit: anchor entry %d out of range [0,%d)", seq, len(l.entries))
	}
	leaves, err := l.leaves()
	if err != nil {
		return Inclusion{}, err
	}
	return Inclusion{Index: seq, Size: len(l.entries), Path: auditPath(seq, leaves)}, nil
}

// ProveConsistency proves the first `first` anchored entries are an append-only prefix of the
// current log — the transparency-log guarantee that no earlier anchor was rewritten.
func (l *MemAnchorLog) ProveConsistency(first int) (Consistency, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if first < 0 || first > len(l.entries) {
		return Consistency{}, fmt.Errorf("audit: first %d out of range [0,%d]", first, len(l.entries))
	}
	leaves, err := l.leaves()
	if err != nil {
		return Consistency{}, err
	}
	return Consistency{First: first, Size: len(l.entries), Path: consistencyProof(first, leaves)}, nil
}

// VerifyAnchorInclusion reports whether entry is the leaf at proof.Index in an anchor log of
// proof.Size entries committed by root, from the entry and proof alone. The entry must state what
// the proof proves: its Seq is proof.Index, and its RunID is the run its signed head names (the
// only entries MemAnchorLog.Publish writes). An entry that misstates either is an error, since
// the anchor log is another party's and a monitor reads both fields.
func VerifyAnchorInclusion(root []byte, entry AnchorEntry, proof Inclusion) (bool, error) {
	if entry.Seq != proof.Index {
		return false, fmt.Errorf("audit: anchor entry says it is at %d, but the proof is for index %d", entry.Seq, proof.Index)
	}
	if entry.RunID != entry.STH.RunID {
		return false, fmt.Errorf("audit: anchor entry is for run %q, but its tree head is for run %q", entry.RunID, entry.STH.RunID)
	}
	leaf, err := canonicalAnchorEntry(entry)
	if err != nil {
		return false, err
	}
	return verifyPath(root, leaf, proof.Index, proof.Size, proof.Path), nil
}

func canonicalAnchorEntry(e AnchorEntry) ([]byte, error) {
	if err := checkUTF8(e); err != nil {
		return nil, fmt.Errorf("audit: canonicalize anchor entry: %w", err)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("audit: canonicalize anchor entry: %w", err)
	}
	return tagged(anchorLeafTag, b), nil
}
