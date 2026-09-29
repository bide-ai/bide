package audit

import (
	"bytes"
	"context"
	"fmt"
	"math/bits"

	"github.com/bide-ai/bide/agent"
)

// RFC 6962 §2.1.2 Merkle consistency proofs: prove that an earlier tree (the first m
// records) is an append-only PREFIX of a later tree (n records) — i.e. history was only
// appended, never rewritten or reordered. This is the transparency-log guarantee. A
// third party with just (rootM, rootN, proof) can check it without either full journal.

// subProof is RFC 6962 SUBPROOF(m, leaves, b).
func subProof(m int, leaves [][]byte, b bool) [][]byte {
	n := len(leaves)
	if m == n {
		if b {
			return nil
		}
		return [][]byte{merkleRoot(leaves)} // MTH(D[0:m])
	}
	k := split(n)
	if m <= k {
		return append(subProof(m, leaves[:k], b), merkleRoot(leaves[k:]))
	}
	return append(subProof(m-k, leaves[k:], false), merkleRoot(leaves[:k]))
}

// consistencyProof is RFC 6962 PROOF(m, leaves) = SUBPROOF(m, leaves, true).
func consistencyProof(m int, leaves [][]byte) [][]byte {
	if m <= 0 || m >= len(leaves) {
		return nil // m==0 or m==n → empty proof (handled by verify)
	}
	return subProof(m, leaves, true)
}

// --- verification (canonical RFC 6962 / CT reference algorithm) ---

func decompInclProof(index, size uint64) (inner, border uint) {
	inner = uint(bits.Len64(index ^ (size - 1)))
	border = uint(bits.OnesCount64(index >> inner))
	return inner, border
}

func chainInner(seed []byte, proof [][]byte, index uint64) []byte {
	for i, h := range proof {
		if (index>>uint(i))&1 == 0 {
			seed = nodeHash(seed, h)
		} else {
			seed = nodeHash(h, seed)
		}
	}
	return seed
}

func chainInnerRight(seed []byte, proof [][]byte, index uint64) []byte {
	for i, h := range proof {
		if (index>>uint(i))&1 == 1 {
			seed = nodeHash(h, seed)
		}
	}
	return seed
}

func chainBorderRight(seed []byte, proof [][]byte) []byte {
	for _, h := range proof {
		seed = nodeHash(h, seed)
	}
	return seed
}

// verifyConsistency checks an RFC 6962 consistency proof between tree sizes m and n with
// roots root1 and root2.
func verifyConsistency(m, n int, proof [][]byte, root1, root2 []byte) bool {
	switch {
	case m < 0 || m > n: // a negative m is not a tree size; m > n then covers a negative n
		return false
	case m == n:
		return len(proof) == 0 && bytes.Equal(root1, root2)
	case m == 0:
		return len(proof) == 0 // the empty tree is a prefix of any tree
	case len(proof) == 0:
		return false
	}

	inner, border := decompInclProof(uint64(m-1), uint64(n))
	shift := uint(bits.TrailingZeros64(uint64(m)))
	inner -= shift

	seed, start := proof[0], 1
	if m == 1<<shift { // m is a power of two → the seed is root1, not carried in the proof
		seed, start = root1, 0
	}
	if len(proof) != int(start)+int(inner)+int(border) {
		return false
	}
	p := proof[start:]
	mask := (uint64(m) - 1) >> shift

	hash1 := chainBorderRight(chainInnerRight(seed, p[:inner], mask), p[inner:])
	hash2 := chainBorderRight(chainInner(seed, p[:inner], mask), p[inner:])
	return bytes.Equal(hash1, root1) && bytes.Equal(hash2, root2)
}

// Consistency is an RFC 6962 consistency proof: the first First records are an append-only
// prefix of the Size-record tree.
type Consistency struct {
	First int      // m — the earlier tree size
	Size  int      // n — the current tree size
	Path  [][]byte // proof hashes
}

// ProveConsistency proves that runID's first `first` records are an append-only prefix of
// its current journal — i.e. nothing before `first` was changed or reordered, only appended.
func ProveConsistency(ctx context.Context, store agent.Durable, runID string, first int) (Consistency, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return Consistency{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	if first < 0 || first > len(recs) {
		return Consistency{}, fmt.Errorf("audit: first %d out of range [0,%d]", first, len(recs))
	}
	leaves, err := canonicalLeaves(recs)
	if err != nil {
		return Consistency{}, err
	}
	return Consistency{First: first, Size: len(recs), Path: consistencyProof(first, leaves)}, nil
}

// VerifyConsistency reports whether firstRoot (an earlier Root over First records) is an
// append-only prefix of laterRoot (a Root over Size records), given the proof — checked
// from the two roots + proof alone. A rewrite or reorder of any early record fails.
func VerifyConsistency(firstRoot, laterRoot []byte, proof Consistency) bool {
	return verifyConsistency(proof.First, proof.Size, proof.Path, firstRoot, laterRoot)
}
