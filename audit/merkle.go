package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
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

const (
	rfc6962LeafPrefix = 0x00 // hash of a leaf   = SHA-256(0x00 || data)
	rfc6962NodePrefix = 0x01 // hash of a node   = SHA-256(0x01 || left || right)
)

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

// merkleRoot is the RFC 6962 Merkle Tree Hash (MTH) over the leaf data slices.
func merkleRoot(leaves [][]byte) []byte {
	switch len(leaves) {
	case 0:
		s := sha256.Sum256(nil) // MTH({}) = SHA-256()
		return s[:]
	case 1:
		return leafHash(leaves[0])
	}
	k := split(len(leaves))
	return nodeHash(merkleRoot(leaves[:k]), merkleRoot(leaves[k:]))
}

// auditPath is RFC 6962 PATH(m, D): the sibling hashes proving leaf m's inclusion.
func auditPath(m int, leaves [][]byte) [][]byte {
	if len(leaves) <= 1 {
		return nil
	}
	k := split(len(leaves))
	if m < k {
		return append(auditPath(m, leaves[:k]), merkleRoot(leaves[k:]))
	}
	return append(auditPath(m-k, leaves[k:]), merkleRoot(leaves[:k]))
}

// verifyPath runs the RFC 6962 §2.1.1 inclusion-proof verification algorithm.
func verifyPath(root, leaf []byte, index, size int, path [][]byte) bool {
	if index >= size {
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

func canonicalLeaves(recs []agent.Record) ([][]byte, error) {
	leaves := make([][]byte, len(recs))
	for i, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			return nil, fmt.Errorf("audit: canonicalize record %d: %w", i, err)
		}
		leaves[i] = b
	}
	return leaves, nil
}

// Root returns the RFC 6962 Merkle root committing to runID's journal (records in
// persisted order, each leaf = the canonical record bytes). Anchor it like Head; unlike
// Head it supports per-record inclusion proofs (Prove / VerifyInclusion).
func Root(ctx context.Context, store agent.Durable, runID string) ([]byte, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	leaves, err := canonicalLeaves(recs)
	if err != nil {
		return nil, err
	}
	return merkleRoot(leaves), nil
}

// Inclusion is an RFC 6962 audit path proving one record's membership in a committed run.
type Inclusion struct {
	Index int      // the record's position in the journal
	Size  int      // the journal length the root committed to
	Path  [][]byte // sibling hashes, leaf-to-root
}

// Prove returns an inclusion proof for the record at index in runID's journal — enough to
// verify that record against Root WITHOUT revealing any other record (selective disclosure).
func Prove(ctx context.Context, store agent.Durable, runID string, index int) (Inclusion, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return Inclusion{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	if index < 0 || index >= len(recs) {
		return Inclusion{}, fmt.Errorf("audit: record index %d out of range [0,%d)", index, len(recs))
	}
	leaves, err := canonicalLeaves(recs)
	if err != nil {
		return Inclusion{}, err
	}
	return Inclusion{Index: index, Size: len(recs), Path: auditPath(index, leaves)}, nil
}

// VerifyInclusion reports whether record is the leaf at proof.Index in a run of proof.Size
// records committed by root — checked from record + proof alone, no other records needed.
func VerifyInclusion(root []byte, record agent.Record, proof Inclusion) (bool, error) {
	leaf, err := json.Marshal(record)
	if err != nil {
		return false, fmt.Errorf("audit: canonicalize record: %w", err)
	}
	return verifyPath(root, leaf, proof.Index, proof.Size, proof.Path), nil
}
