package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/bide-ai/bide/agent"
)

// This file implements RFC 6962 (Certificate Transparency) Merkle trees over the journal:
// a Merkle root commitment plus inclusion proofs. The point over the linear Head chain is
// SELECTIVE DISCLOSURE — prove a single record is part of a committed run (via an O(log n)
// audit path) without revealing any other record. Same anchoring caveat as Head: the root
// must be signed/published out-of-band to be tamper-evident against a DB-controlling
// attacker. Domain-separated leaf/node hashing follows RFC 6962 §2.1 exactly.
//
// What an inclusion proof discloses: the record, its index, the size of the tree, and the
// sibling hashes on its path. Each sibling hash covers other leaves, and path[0] is a single
// neighbour's leaf hash, so a leaf must not be a function of guessable content alone: a verifier
// could hash each candidate for a low-entropy neighbour (an approval, a {"fraud_flag":true}
// result) and compare. Every journal record therefore carries a random 32-byte salt
// (agent.Record.Salt), set when a store first journals it and committed in its leaf, and a
// proof discloses only its own record's salt. The other hashes on the path cannot be matched
// against a guess without the salts they cover.
//
// Every leaf's data starts with a versioned tag naming what kind of leaf it is, so a leaf of one
// kind or version never hashes like another's:
//
//	journal record: SHA-256(0x00 || "bide.audit.journal-leaf.v1\x00" || the record's stored bytes)
//	absence key:    SHA-256(0x00 || "bide.audit.key-leaf.v1\x00" || key)
//	event:          SHA-256(0x00 || "bide.audit.event-leaf.v3\x00" || {"kind":...,"event":...,"salt":...})
//	anchor entry:   SHA-256(0x00 || "bide.audit.anchor-leaf.v1\x00" || entry JSON)
//
// A journal record's leaf commits to the bytes the journal stores for it (agent.Record.Raw),
// verbatim: never a re-encoding of the decoded record. A record written by a later release, with
// fields this one does not know, therefore hashes to the same leaf here as there, and a proof
// carries those bytes (ProofBundle.RecordBytes) for the verifier to hash. A record a redaction
// replaced with a tombstone keeps its place in the tree: its leaf hash is the one the tombstone
// records (see agent.Record.Redacted and JournalLeafHash), so every other record's proof, and the
// root, are unchanged; the redacted record itself can no longer be proven.
//
// A journal record's stored bytes include its salt (the "salt" field, base64), and so does an
// event's leaf: every event carries a random 32-byte salt that only its own proof discloses (see
// EventInclusion; v1 event leaves were unsalted). Key and anchor leaves are not salted. An absence
// proof names its neighbouring keys in the clear anyway (see Absence). An anchor entry holds a
// signed tree head, whose root and Ed25519 signature a holder of a neighbour's proof cannot
// compute, so a guess about a neighbouring entry can be confirmed only by someone who already
// holds that exact signed head (see AnchorEntry).

const (
	rfc6962LeafPrefix = 0x00 // hash of a leaf   = SHA-256(0x00 || data)
	rfc6962NodePrefix = 0x01 // hash of a node   = SHA-256(0x01 || left || right)
)

// Leaf tags: the versioned prefix of each kind of leaf's data (see above).
const (
	journalLeafTag = "bide.audit.journal-leaf.v1\x00"
	keyLeafTag     = "bide.audit.key-leaf.v1\x00"
	eventLeafTag   = "bide.audit.event-leaf.v3\x00"
	anchorLeafTag  = "bide.audit.anchor-leaf.v1\x00"
)

// tagged returns tag followed by data, a leaf's data.
func tagged(tag string, data []byte) []byte {
	return append([]byte(tag), data...)
}

func leafHash(data []byte) []byte {
	h := sha256.New()
	h.Write([]byte{rfc6962LeafPrefix})
	h.Write(data)
	return h.Sum(nil)
}

func nodeHash(l, r []byte) []byte {
	h := sha256.New()
	h.Write([]byte{rfc6962NodePrefix})
	h.Write(l)
	h.Write(r)
	return h.Sum(nil)
}

// split returns k, the largest power of two strictly less than n (n >= 2) — the RFC 6962
// split point for MTH(D[n]) = HASH(0x01 || MTH(D[0:k]) || MTH(D[k:n])).
func split(n int) int {
	k := 1
	for k < n {
		k <<= 1
	}
	return k >> 1
}

// leafHashes returns the leaf hash of each leaf's data.
func leafHashes(leaves [][]byte) [][]byte {
	out := make([][]byte, len(leaves))
	for i, l := range leaves {
		out[i] = leafHash(l)
	}
	return out
}

// merkleRoot is the RFC 6962 Merkle Tree Hash (MTH) over the leaf data slices.
func merkleRoot(leaves [][]byte) []byte { return hashRoot(leafHashes(leaves)) }

// hashRoot is the RFC 6962 Merkle Tree Hash over leaves given by their leaf hashes.
func hashRoot(hashes [][]byte) []byte {
	switch len(hashes) {
	case 0:
		s := sha256.Sum256(nil) // MTH({}) = SHA-256()
		return s[:]
	case 1:
		return hashes[0]
	}
	k := split(len(hashes))
	return nodeHash(hashRoot(hashes[:k]), hashRoot(hashes[k:]))
}

// auditPath is RFC 6962 PATH(m, D): the sibling hashes proving leaf m's inclusion.
func auditPath(m int, leaves [][]byte) [][]byte { return hashPath(m, leafHashes(leaves)) }

// hashPath is RFC 6962 PATH(m, D) over leaves given by their leaf hashes.
func hashPath(m int, hashes [][]byte) [][]byte {
	if len(hashes) <= 1 {
		return nil
	}
	k := split(len(hashes))
	if m < k {
		return append(hashPath(m, hashes[:k]), hashRoot(hashes[k:]))
	}
	return append(hashPath(m-k, hashes[k:]), hashRoot(hashes[:k]))
}

// verifyPath runs the RFC 6962 §2.1.1 inclusion-proof verification algorithm. A negative index is
// rejected (the bit arithmetic below would otherwise accept index -1 as the last leaf), and with it
// a negative size, which index >= size then covers.
func verifyPath(root, leaf []byte, index, size int, path [][]byte) bool {
	if index < 0 || index >= size {
		return false
	}
	fn, sn := index, size-1
	r := leafHash(leaf)
	for _, p := range path {
		if sn == 0 {
			return false
		}
		if fn&1 == 1 || fn == sn {
			r = nodeHash(p, r)
			for fn != 0 && fn&1 == 0 {
				fn >>= 1
				sn >>= 1
			}
		} else {
			r = nodeHash(r, p)
		}
		fn >>= 1
		sn >>= 1
	}
	return sn == 0 && bytes.Equal(r, root)
}

// canonicalRecord is the leaf data of one journal record: the journal leaf tag followed by the
// bytes the journal stores for it (agent.Record.Raw), verbatim, so a proof commits to what the
// journal holds and a record with fields this version does not know hashes as it was written. A
// record built in memory (no stored bytes) is refused: only a record read back from a journal has
// a leaf. A record without an agent.SaltSize salt is refused too: its leaf would be a function of its
// content alone, which a proof for its neighbour lets anyone confirm by guessing (see the top of
// this file). Every store sets the salt (agent.JournalEntry). A redacted record has no leaf data;
// journalLeafHashes reads its leaf hash from its tombstone.
func canonicalRecord(r agent.Record) ([]byte, error) {
	if r.Redacted {
		return nil, fmt.Errorf("record %q is redacted: its stored bytes are a tombstone, and only its leaf hash remains", r.Name)
	}
	raw := r.Raw()
	if raw == nil {
		return nil, fmt.Errorf("record %q has no stored bytes: only a record read back from a journal (History, Records, Get) has a leaf", r.Name)
	}
	if len(r.Salt()) != agent.SaltSize {
		return nil, fmt.Errorf("record %q has a %d-byte salt, want %d (a store sets it when it journals the record; see agent.JournalEntry)", r.Name, len(r.Salt()), agent.SaltSize)
	}
	return tagged(journalLeafTag, raw), nil
}

// JournalLeafHash returns the RFC 6962 leaf hash of a journal record from the bytes the journal
// stores for it: SHA-256(0x00 || "bide.audit.journal-leaf.v1\x00" || recordBytes). A redaction
// records it, hex-encoded, in the tombstone that replaces the bytes (see agent.Record.Redacted), so
// the record keeps its place in every tree over the journal.
func JournalLeafHash(recordBytes []byte) []byte { return leafHash(tagged(journalLeafTag, recordBytes)) }

// journalLeafHashes returns the leaf hash of each record: the hash of its leaf data, or for a
// redacted record the leaf hash its tombstone records.
func journalLeafHashes(recs []agent.Record) ([][]byte, error) {
	out := make([][]byte, len(recs))
	for i, r := range recs {
		if r.Redacted {
			h, err := tombstoneLeafHash(r.Raw())
			if err != nil {
				return nil, fmt.Errorf("audit: record %d (%q): %w", i, r.Name, err)
			}
			out[i] = h
			continue
		}
		b, err := canonicalRecord(r)
		if err != nil {
			return nil, fmt.Errorf("audit: canonicalize record %d: %w", i, err)
		}
		out[i] = leafHash(b)
	}
	return out, nil
}

// tombstoneLeafHash reads the leaf hash a redaction tombstone records:
// {"redacted":{"leaf_hash":"<hex>",...}}, 32 bytes.
func tombstoneLeafHash(tombstone []byte) ([]byte, error) {
	var t struct {
		Redacted *struct {
			LeafHash string `json:"leaf_hash"`
		} `json:"redacted"`
	}
	if err := json.Unmarshal(tombstone, &t); err != nil || t.Redacted == nil {
		return nil, fmt.Errorf("a redacted record's tombstone does not read: %w", ErrMalformed)
	}
	h, err := hex.DecodeString(t.Redacted.LeafHash)
	if err != nil || len(h) != sha256.Size {
		return nil, fmt.Errorf("a redacted record's tombstone leaf hash %q is not 32 bytes of hex: %w", t.Redacted.LeafHash, ErrMalformed)
	}
	return h, nil
}

// decodeRecordBytes decodes a proven record from its stored bytes for display and role checks
// only, leniently: a field this version does not know is ignored, never an error, as the journal
// itself reads it. The leaf is the bytes, never this decoding. Bytes that do not decode as a record
// are ErrMalformed.
func decodeRecordBytes(b []byte) (agent.Record, error) {
	if len(b) == 0 {
		return agent.Record{}, fmt.Errorf("audit: the proof carries no record bytes: %w", ErrMalformed)
	}
	r, err := agent.DecodeRecord(b)
	if err != nil {
		return agent.Record{}, fmt.Errorf("audit: the record bytes do not decode as a journal record (%v): %w", err, ErrMalformed)
	}
	return r, nil
}

// Root returns the RFC 6962 Merkle root committing to runID's journal (records in
// persisted order, each leaf = the canonical record bytes). Anchor it like Head; unlike
// Head it supports per-record inclusion proofs (Prove / VerifyInclusion).
func Root(ctx context.Context, store agent.Durable, runID string) ([]byte, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	leaves, err := journalLeafHashes(recs)
	if err != nil {
		return nil, err
	}
	return hashRoot(leaves), nil
}

// Inclusion is an RFC 6962 audit path proving one record's membership in a committed run.
type Inclusion struct {
	Index int      `json:"index"` // the record's position in the journal
	Size  int      `json:"size"`  // the journal length the root committed to
	Path  [][]byte `json:"path"`  // sibling hashes, leaf-to-root
}

// Prove returns an inclusion proof for the record at index in runID's journal: enough to verify
// that record's stored bytes (agent.Record.Raw) against Root WITHOUT revealing any other record
// (selective disclosure). A redacted record cannot be proven.
func Prove(ctx context.Context, store agent.Durable, runID string, index int) (Inclusion, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return Inclusion{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	if index < 0 || index >= len(recs) {
		return Inclusion{}, fmt.Errorf("audit: record index %d out of range [0,%d)", index, len(recs))
	}
	if _, err := canonicalRecord(recs[index]); err != nil {
		return Inclusion{}, fmt.Errorf("audit: record %d: %w", index, err)
	}
	leaves, err := journalLeafHashes(recs)
	if err != nil {
		return Inclusion{}, err
	}
	return Inclusion{Index: index, Size: len(recs), Path: hashPath(index, leaves)}, nil
}

// VerifyInclusion returns nil if recordBytes, the bytes a journal stores for a record, are the
// leaf at proof.Index in a run of proof.Size records committed by root: checked from the bytes and
// the proof alone, no other records needed, and without decoding the bytes. Otherwise the error
// wraps ErrNotVerified.
func VerifyInclusion(root, recordBytes []byte, proof Inclusion) error {
	if !verifyPath(root, tagged(journalLeafTag, recordBytes), proof.Index, proof.Size, proof.Path) {
		return notVerified("audit: the record is not leaf %d of the %d-record tree under this root", proof.Index, proof.Size)
	}
	return nil
}
