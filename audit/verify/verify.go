// Package verify is a dependency-light, standalone verifier for Bide audit proofs. It
// depends on nothing but the Go standard library (crypto/sha256, crypto/ed25519, crypto/mldsa,
// encoding/binary, bytes, container/list, math/big, math/bits, strings, sync), deliberately NOT
// the agent core or gsm, so a third party (an auditor, a regulator) can verify a proof without
// importing the SDK, or reimplement this file from RFC 6962 and check us against it. That is what "verifiable without trusting
// the vendor" means in practice.
//
// It operates on canonical LEAF BYTES, not typed records, precisely so it needs no domain
// types. A leaf's bytes are a versioned tag naming its kind followed by its content: JournalLeaf
// builds a journal record's leaf from the bytes a store persists for it (a proof's record_bytes,
// the record's random salt included), KeyLeaf an absence key's, EventLeaf an event's (its random
// salt included), and AnchorLeaf an anchor entry's. TreeHead checks a signed tree head under any of
// the three schemes (ed25519, ml-dsa-65, and their hybrid), with a Verifier from NewVerifier. The algorithms mirror the audit package exactly and are
// cross-checked against it in the tests, so the intentional duplication cannot drift.
//
// This mirror is verification-only, by design: it can check a proof, never mint one.
package verify

import (
	"bytes"
	"crypto/ed25519"
	"crypto/mldsa"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math/bits"
	"strings"
)

// JournalLeaf returns the leaf bytes of a journal record from the bytes a store persists for it
// (a proof's record_bytes, verbatim, including its "salt"): "bide.audit.journal-leaf.v1\x00" ||
// record. The bytes are hashed as they are, never decoded, so a record with fields this verifier
// does not know verifies all the same.
func JournalLeaf(record []byte) []byte { return tagged("bide.audit.journal-leaf.v1\x00", record) }

// KeyLeaf returns the leaf bytes of an absence key: "bide.audit.key-leaf.v1\x00" || key.
func KeyLeaf(key string) []byte { return tagged("bide.audit.key-leaf.v1\x00", []byte(key)) }

// EventLeaf returns the leaf bytes of an event from its canonical JSON, the event's salt included
// ({"kind":...,"event":...,"salt":...}, with a snake_case kind and event, and the salt the event's
// proof discloses in base64): "bide.audit.event-leaf.v3\x00" || event.
func EventLeaf(event []byte) []byte { return tagged("bide.audit.event-leaf.v3\x00", event) }

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

// Head is the content of a signed tree head (the "sth" of a proof): the scheme it is signed
// under, the tree's kind and run, its size and root, when it was signed, and, for an absence
// key-set head, the source journal tree.
type Head struct {
	Alg            string   // "ed25519", "ml-dsa-65", or "ed25519+ml-dsa-65"
	Kind           string   // "journal", "events", or an "absence/..." key set
	RunID          string   // the run the tree is about
	Size           int      // number of leaves
	Root           []byte   // RFC 6962 root over them
	TimestampNanos int64    // when the head was signed, Unix nanoseconds
	Journal        *TreeRef // the source journal tree of an absence key-set head; nil otherwise
}

// TreeHead reports whether sig is a valid signature, under v, of the signed tree head h. h.Alg
// must be v's scheme: the scheme is part of the signed bytes, and a signature is never checked
// under another. The canonical encoding is domain-separated and length-prefixed, byte-identical
// to the audit package's (format bide.audit.sth.v5):
//
//	"bide.audit.sth.v5\x00" || field(alg) || field(kind) || field(run_id) || u64(size) ||
//	field(root) || u64(timestamp_nanos) || (0x00 | 0x01 || u64(journal.size) || field(journal.root))
//
// where field(x) is u64(len(x)) || x and u64 is 8 bytes big-endian. So an STH signed by the SDK
// verifies here and vice versa. The caller checks that kind and runID are the tree it expects.
//
// A weak pub (not canonically encoded, small order, or outside the prime-order subgroup) verifies
// nothing: crypto/ed25519 alone accepts forged signatures under a small-order key.
//
// Like audit, it refuses a head whose shape no commitment has, whatever its signature: a
// "journal" or "events" head that names a source journal, an "absence/<set>" head that names none,
// and a head of any other kind.
func TreeHead(h Head, sig []byte, v Verifier) bool {
	if v == nil || h.Alg != v.Alg() || h.Size < 0 || !wellFormed(h.Kind, h.Journal) {
		return false
	}
	field := func(b, f []byte) []byte {
		b = binary.BigEndian.AppendUint64(b, uint64(len(f)))
		return append(b, f...)
	}
	b := append([]byte(nil), "bide.audit.sth.v5\x00"...)
	b = field(b, []byte(h.Alg))
	b = field(b, []byte(h.Kind))
	b = field(b, []byte(h.RunID))
	b = binary.BigEndian.AppendUint64(b, uint64(h.Size))
	b = field(b, h.Root)
	b = binary.BigEndian.AppendUint64(b, uint64(h.TimestampNanos))
	if h.Journal == nil {
		b = append(b, 0)
	} else {
		b = append(b, 1)
		b = binary.BigEndian.AppendUint64(b, uint64(h.Journal.Size))
		b = field(b, h.Journal.Root)
	}
	return v.Verify(b, sig)
}

// Verifier checks signatures under one scheme with one public key. NewVerifier builds one for each
// scheme the audit package signs with.
type Verifier interface {
	// Alg names the scheme: "ed25519", "ml-dsa-65", or "ed25519+ml-dsa-65".
	Alg() string
	// Verify reports whether sig is a valid signature over message.
	Verify(message, sig []byte) bool
}

// NewVerifier returns the verifier of scheme alg for the encoded public key pub, with the
// encodings the audit package uses: ed25519, the 32-byte key; ml-dsa-65, the 1952-byte ML-DSA-65
// public key; ed25519+ml-dsa-65, a 4-byte big-endian length of the Ed25519 key, the Ed25519 key,
// then the ML-DSA-65 key. A weak Ed25519 key is refused, as audit.NewVerifier refuses it.
//
// The schemes are the audit package's: ML-DSA-65 signs with the context "bide.audit.mldsa65.v1", and a
// hybrid signature is len32(ed) || ed || mldsa, where the Ed25519 component signs
// "bide.hybrid.ed25519.v1\x00" || message and the ML-DSA-65 component signs
// "bide.hybrid.mldsa65.v1\x00" || message, and both must verify.
func NewVerifier(alg string, pub []byte) (Verifier, error) {
	switch alg {
	case "ed25519":
		if len(pub) != ed25519.PublicKeySize {
			return nil, errors.New("verify: an ed25519 public key is 32 bytes")
		}
		// A weak key (small order, mixed order, or not canonically encoded) is not a verification
		// key: a small-order key accepts forged signatures (see usableKey).
		if !usableKey(pub) {
			return nil, errors.New("verify: a weak ed25519 public key (small or mixed order, or not canonically encoded)")
		}
		return edVerifier(bytes.Clone(pub)), nil
	case "ml-dsa-65":
		pk, err := mldsa.NewPublicKey(mldsa.MLDSA65(), pub)
		if err != nil {
			return nil, err
		}
		return mldsaVerifier{pk}, nil
	case "ed25519+ml-dsa-65":
		ed, ml, ok := split32(pub)
		if !ok {
			return nil, errors.New("verify: a hybrid public key has no valid length prefix")
		}
		e, err := NewVerifier("ed25519", ed)
		if err != nil {
			return nil, err
		}
		m, err := NewVerifier("ml-dsa-65", ml)
		if err != nil {
			return nil, err
		}
		return hybridVerifier{e.(edVerifier), m.(mldsaVerifier)}, nil
	default:
		return nil, errors.New("verify: unknown signature scheme " + alg)
	}
}

type edVerifier ed25519.PublicKey

func (edVerifier) Alg() string { return "ed25519" }
// Verify reports whether sig is a valid Ed25519 signature over m. A weak key (not canonically
// encoded, small order, or outside the prime-order subgroup; see usableKey) verifies nothing:
// crypto/ed25519 alone accepts forged signatures under a small-order key.
func (v edVerifier) Verify(m, sig []byte) bool {
	return usableKey(v) && ed25519.Verify(ed25519.PublicKey(v), m, sig)
}

type mldsaVerifier struct{ pk *mldsa.PublicKey }

func (mldsaVerifier) Alg() string { return "ml-dsa-65" }
func (v mldsaVerifier) Verify(m, sig []byte) bool {
	return v.pk != nil && mldsa.Verify(v.pk, m, sig, &mldsa.Options{Context: "bide.audit.mldsa65.v1"}) == nil
}

type hybridVerifier struct {
	ed edVerifier
	ml mldsaVerifier
}

func (hybridVerifier) Alg() string { return "ed25519+ml-dsa-65" }
func (v hybridVerifier) Verify(m, sig []byte) bool {
	e, d, ok := split32(sig)
	return ok &&
		v.ed.Verify(append([]byte("bide.hybrid.ed25519.v1\x00"), m...), e) &&
		v.ml.Verify(append([]byte("bide.hybrid.mldsa65.v1\x00"), m...), d)
}

// split32 splits len32(a) || a || b.
func split32(b []byte) (x, y []byte, ok bool) {
	if len(b) < 4 {
		return nil, nil, false
	}
	n := uint64(binary.BigEndian.Uint32(b[:4]))
	if n > uint64(len(b)-4) {
		return nil, nil, false
	}
	return b[4 : 4+n], b[4+n:], true
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
