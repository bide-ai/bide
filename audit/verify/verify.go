// Package verify is a dependency-light, standalone verifier for go-agents audit proofs. It
// depends on nothing but the Go standard library (crypto/sha256, crypto/ed25519,
// encoding/binary, bytes, math/bits), deliberately NOT the agent core or gsm, so a third
// party (an auditor, a regulator) can verify a proof without importing the SDK, or reimplement
// this file from RFC 6962 and check us against it. That is what "verifiable without trusting
// the vendor" means in practice.
//
// It operates on canonical LEAF BYTES, not typed records, precisely so it needs no domain
// types. The leaf for a journal record is the record's canonical JSON (what the audit package
// hashes); the caller supplies those bytes. The algorithms mirror the audit package exactly
// and are cross-checked against it in the tests, so the intentional duplication cannot drift.
//
// This mirror is verification-only, by design: it can check a proof, never mint one.
package verify

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"math/bits"
)

// RFC 6962 domain-separated leaf/node hashing.
func leafHash(data []byte) []byte {
	h := sha256.New()
	h.Write([]byte{0x00})
	h.Write(data)
	return h.Sum(nil)
}

func nodeHash(l, r []byte) []byte {
	h := sha256.New()
	h.Write([]byte{0x01})
	h.Write(l)
	h.Write(r)
	return h.Sum(nil)
}

// Inclusion runs the RFC 6962 §2.1.1 inclusion-proof algorithm: leaf is the canonical leaf
// bytes, index/size locate it in the tree, path is the sibling hashes leaf-to-root, and root
// is the committed Merkle root (an STH's Root). Reports whether leaf is provably at index in a
// size-leaf tree committed by root.
func Inclusion(root, leaf []byte, index, size int, path [][]byte) bool {
	if index < 0 || size < 0 || index >= size {
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

// Consistency runs the RFC 6962 §2.1.2 consistency-proof algorithm: reports whether firstRoot
// (over the first `first` leaves) is an append-only prefix of laterRoot (over `size` leaves).
func Consistency(first, size int, path [][]byte, firstRoot, laterRoot []byte) bool {
	switch {
	case first > size:
		return false
	case first == size:
		return len(path) == 0 && bytes.Equal(firstRoot, laterRoot)
	case first == 0:
		return len(path) == 0
	case len(path) == 0:
		return false
	}

	inner := uint(bits.Len64(uint64(first-1) ^ uint64(size-1)))
	border := uint(bits.OnesCount64(uint64(first-1) >> inner))
	shift := uint(bits.TrailingZeros64(uint64(first)))
	inner -= shift

	seed, start := path[0], 1
	if first == 1<<shift {
		seed, start = firstRoot, 0
	}
	if len(path) != int(start)+int(inner)+int(border) {
		return false
	}
	p := path[start:]
	mask := (uint64(first) - 1) >> shift

	chainInner := func(seed []byte, proof [][]byte, index uint64) []byte {
		for i, h := range proof {
			if (index>>uint(i))&1 == 0 {
				seed = nodeHash(seed, h)
			} else {
				seed = nodeHash(h, seed)
			}
		}
		return seed
	}
	chainInnerRight := func(seed []byte, proof [][]byte, index uint64) []byte {
		for i, h := range proof {
			if (index>>uint(i))&1 == 1 {
				seed = nodeHash(h, seed)
			}
		}
		return seed
	}
	chainBorderRight := func(seed []byte, proof [][]byte) []byte {
		for _, h := range proof {
			seed = nodeHash(h, seed)
		}
		return seed
	}

	hash1 := chainBorderRight(chainInnerRight(seed, p[:inner], mask), p[inner:])
	hash2 := chainBorderRight(chainInner(seed, p[:inner], mask), p[inner:])
	return bytes.Equal(hash1, firstRoot) && bytes.Equal(hash2, laterRoot)
}

// TreeHead reports whether sig is a valid Ed25519 signature, under pub, of the signed tree
// head committing to (root, size, timestamp). The canonical encoding is domain-separated and
// length-prefixed, byte-identical to audit.TreeHead.canonical(), so an STH signed by the SDK
// verifies here and vice versa.
func TreeHead(root []byte, size int, timestamp int64, sig, pub []byte) bool {
	if len(pub) != ed25519.PublicKeySize {
		return false
	}
	b := append([]byte(nil), "go-agents.audit.sth.v1\x00"...)
	b = binary.BigEndian.AppendUint64(b, uint64(size))
	b = binary.BigEndian.AppendUint64(b, uint64(len(root)))
	b = append(b, root...)
	b = binary.BigEndian.AppendUint64(b, uint64(timestamp))
	return ed25519.Verify(pub, b, sig)
}
