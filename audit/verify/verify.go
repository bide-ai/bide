// Package verify is a dependency-light, standalone verifier for Bide audit proofs. It
// depends on nothing but the Go standard library (crypto/sha256, crypto/ed25519,
// encoding/binary, bytes, math/bits, strings), deliberately NOT the agent core or gsm, so a third
// party (an auditor, a regulator) can verify a proof without importing the SDK, or reimplement
// this file from RFC 6962 and check us against it. That is what "verifiable without trusting
// the vendor" means in practice.
//
// It operates on canonical LEAF BYTES, not typed records, precisely so it needs no domain
// types. A leaf's bytes are a versioned tag naming its kind followed by its content: JournalLeaf
// builds a journal record's leaf from the record's journal encoding (the bytes a store persists,
// the record's random salt included), KeyLeaf an absence key's, EventLeaf an event's (its random
// salt included), and
// AnchorLeaf an anchor entry's. The algorithms mirror the audit package exactly and are
// cross-checked against it in the tests, so the intentional duplication cannot drift.
//
// This mirror is verification-only, by design: it can check a proof, never mint one.
package verify

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"math/bits"
	"strings"
)

// JournalLeaf returns the leaf bytes of a journal record from its journal encoding (the JSON a
// store persists for it, including its "salt"): "bide.audit.journal-leaf.v1\x00" || record.
func JournalLeaf(record []byte) []byte { return tagged("bide.audit.journal-leaf.v1\x00", record) }

// KeyLeaf returns the leaf bytes of an absence key: "bide.audit.key-leaf.v1\x00" || key.
func KeyLeaf(key string) []byte { return tagged("bide.audit.key-leaf.v1\x00", []byte(key)) }

// EventLeaf returns the leaf bytes of an event from its canonical JSON, the event's salt included
// ({"kind":...,"event":...,"salt":...}, the salt the event's proof discloses, in base64):
// "bide.audit.event-leaf.v2\x00" || event.
func EventLeaf(event []byte) []byte { return tagged("bide.audit.event-leaf.v2\x00", event) }

// AnchorLeaf returns the leaf bytes of an anchor log entry from its JSON:
// "bide.audit.anchor-leaf.v1\x00" || entry.
func AnchorLeaf(entry []byte) []byte { return tagged("bide.audit.anchor-leaf.v1\x00", entry) }

func tagged(tag string, data []byte) []byte { return append([]byte(tag), data...) }

// RFC 6962 domain-separated leaf/node hashing: a leaf hashes as SHA-256(0x00 || leaf bytes).
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
// bytes (JournalLeaf, KeyLeaf, EventLeaf, or AnchorLeaf of the content), index/size locate it in
// the tree, path is the sibling hashes leaf-to-root, and root
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
	case first < 0 || first > size: // first > size then covers a negative size
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

// TreeRef names one tree by its size and root: the source journal of an absence key-set head.
type TreeRef struct {
	Size int    `json:"size"`
	Root []byte `json:"root"`
}

// TreeHead reports whether sig is a valid Ed25519 signature, under pub, of the signed tree head
// committing to (kind, runID, size, root, timestamp, journal). kind is the tree's kind
// ("journal", "events", or an "absence/..." key set); journal is the source journal tree of an
// absence key-set head and nil otherwise. The canonical encoding is domain-separated and
// length-prefixed, byte-identical to audit.TreeHead.canonical(), so an STH signed by the SDK
// verifies here and vice versa. The caller checks that kind and runID are the tree it expects.
//
// Like audit, it refuses a head whose shape no commitment has, whatever its signature: a
// "journal" or "events" head that names a source journal, an "absence/<set>" head that names none,
// and a head of any other kind.
func TreeHead(kind, runID string, size int, root []byte, timestamp int64, journal *TreeRef, sig, pub []byte) bool {
	if len(pub) != ed25519.PublicKeySize || size < 0 || !wellFormed(kind, journal) {
		return false
	}
	field := func(b, f []byte) []byte {
		b = binary.BigEndian.AppendUint64(b, uint64(len(f)))
		return append(b, f...)
	}
	b := append([]byte(nil), "bide.audit.sth.v4\x00"...)
	b = field(b, []byte(kind))
	b = field(b, []byte(runID))
	b = binary.BigEndian.AppendUint64(b, uint64(size))
	b = field(b, root)
	b = binary.BigEndian.AppendUint64(b, uint64(timestamp))
	if journal == nil {
		b = append(b, 0)
	} else {
		b = append(b, 1)
		b = binary.BigEndian.AppendUint64(b, uint64(journal.Size))
		b = field(b, journal.Root)
	}
	return ed25519.Verify(pub, b, sig)
}

// wellFormed mirrors audit's TreeHead.wellFormed: the kinds a signed head can have, and whether
// each names a source journal.
func wellFormed(kind string, journal *TreeRef) bool {
	switch {
	case kind == "journal" || kind == "events":
		return journal == nil
	case strings.HasPrefix(kind, "absence/") && len(kind) > len("absence/"):
		return journal != nil && journal.Size >= 0
	default:
		return false
	}
}
