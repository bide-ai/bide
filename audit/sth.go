package audit

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"fmt"

	agent "github.com/dayna/go-agents"
)

// A Signed Tree Head (STH) is the anchoring artifact for the RFC 6962 Merkle commitment,
// modeled on Certificate Transparency. It binds the Merkle Root to the tree Size and a
// Timestamp and signs the bundle, so the signature commits to WHICH tree and WHEN — not
// just to root bytes that could be replayed across sizes. Publish/anchor the STH; then
// inclusion and consistency proofs are checked against its (verified) Root.

// TreeHead is a commitment to a run's journal at a point in time.
type TreeHead struct {
	Size      int    // number of records committed
	Root      []byte // RFC 6962 Merkle root over those records (see Root)
	Timestamp int64  // caller-supplied (e.g. time.Now().UnixNano())
}

// canonical is the deterministic, domain-separated, length-prefixed encoding signed by an
// STH — so two different TreeHeads can never share an encoding.
func (th TreeHead) canonical() []byte {
	b := append([]byte(nil), "go-agents.audit.sth.v1\x00"...)
	b = binary.BigEndian.AppendUint64(b, uint64(th.Size))
	b = binary.BigEndian.AppendUint64(b, uint64(len(th.Root)))
	b = append(b, th.Root...)
	b = binary.BigEndian.AppendUint64(b, uint64(th.Timestamp))
	return b
}

// SignedTreeHead is a TreeHead with an Ed25519 signature over its canonical encoding.
type SignedTreeHead struct {
	TreeHead         // the {Size, Root, Timestamp} commitment being signed
	Signature []byte // signature over TreeHead.canonical() under the scheme named by Alg
	// Alg names the signature scheme (see signing.go). Empty means "ed25519", so bundles
	// produced by the ed25519-only SignTreeHead / Verify path are unchanged on the wire and
	// keep verifying. Agility-aware producers set it explicitly (SignTreeHeadWith).
	Alg string `json:"alg,omitempty"`
}

// NewTreeHead builds a TreeHead committing to runID's journal at the given timestamp.
func NewTreeHead(ctx context.Context, store agent.Durable, runID string, timestamp int64) (TreeHead, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return TreeHead{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	leaves, err := canonicalLeaves(recs)
	if err != nil {
		return TreeHead{}, err
	}
	return TreeHead{Size: len(recs), Root: merkleRoot(leaves), Timestamp: timestamp}, nil
}

// SignTreeHead signs a TreeHead. Anchor the result out-of-band (this is what makes the
// journal tamper-evident against later rewrites).
func SignTreeHead(th TreeHead, priv ed25519.PrivateKey) SignedTreeHead {
	return SignedTreeHead{TreeHead: th, Signature: ed25519.Sign(priv, th.canonical())}
}

// Verify reports whether the STH's signature is valid under pub. Any change to Size, Root,
// or Timestamp invalidates it. This is the ed25519 fast path; it accepts an STH with Alg
// empty or "ed25519" and rejects any other scheme (use VerifyWith for those).
func (sth SignedTreeHead) Verify(pub ed25519.PublicKey) bool {
	if sth.Alg != "" && sth.Alg != AlgEd25519 {
		return false
	}
	return ed25519.Verify(pub, sth.canonical(), sth.Signature)
}

// SignTreeHeadWith signs a TreeHead under any scheme (ed25519, ML-DSA, or the hybrid of both),
// tagging the result with the signer's algorithm. Anchor the result out-of-band as usual.
func SignTreeHeadWith(th TreeHead, s Signer) (SignedTreeHead, error) {
	sig, err := s.Sign(th.canonical())
	if err != nil {
		return SignedTreeHead{}, err
	}
	return SignedTreeHead{TreeHead: th, Signature: sig, Alg: s.Alg()}, nil
}

// VerifyWith reports whether the STH's signature is valid under v, requiring the STH's algorithm
// to match the verifier's (an empty Alg is treated as ed25519). Use this for ML-DSA or hybrid
// STHs; Verify remains the ed25519-only convenience.
func (sth SignedTreeHead) VerifyWith(v Verifier) bool {
	alg := sth.Alg
	if alg == "" {
		alg = AlgEd25519
	}
	if alg != v.Alg() {
		return false
	}
	return v.Verify(sth.canonical(), sth.Signature)
}
