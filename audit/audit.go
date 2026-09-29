// Package audit turns an agent's durable journal into a verifiable, tamper-evident
// record. The journal already captures every step of a run (model turns, tool calls and
// results, approvals, saga outcomes); this package commits to that history with a hash
// chain and lets you sign/verify the commitment. Stdlib-only (crypto/sha256, ed25519).
//
// Security model — read this before relying on it for compliance:
//
//   - Head computes a SHA-256 hash chain over the run's journal in its persisted order.
//     Any modification, insertion, deletion, or reordering of a record changes the head.
//   - That gives INTEGRITY (detect corruption / partial writes) unconditionally, and
//     TAMPER-EVIDENCE only if you ANCHOR the head out-of-band — sign it with a key the
//     app tier doesn't fully control, and/or publish it to a separate trust domain.
//     A hash chain stored in the same database an attacker controls can be rewritten and
//     rehashed; the guarantee is "you committed the head elsewhere, so divergence is
//     provable." This package returns the head and provides signing; anchoring policy is
//     yours.
package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"

	"github.com/bide-ai/bide/agent"
)

// domain separates this hash use from any other, seeding the chain. It names the version of the
// record encoding the chain covers: v2 chains over the journal encoding (agent.EncodeRecord),
// which does not HTML-escape, so a head over the v1 encoding (where <, >, and & were escaped) is
// told apart from a head over the same journal in v2 rather than read as a fork.
var domain = sha256.Sum256([]byte("bide.audit.v2"))

// Head returns the hash-chain commitment to runID's journal: head_0 = H(domain), and
// head_i = H(head_{i-1} || canonical(record_i)) over the records in persisted order, where
// canonical is agent.EncodeRecord: the bytes the store persisted for the record. Two
// runs produce the same head iff their journals are byte-identical in the same order, so
// the head is a deterministic fingerprint of the entire execution history.
func Head(ctx context.Context, store agent.Durable, runID string) ([]byte, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	head := domain[:]
	for i, r := range recs {
		b, err := agent.EncodeRecord(r)
		if err != nil {
			return nil, fmt.Errorf("audit: canonicalize record %d: %w", i, err)
		}
		sum := sha256.Sum256(append(append([]byte{}, head...), b...))
		head = sum[:]
	}
	return head, nil
}

// Sign returns an Ed25519 signature over a head commitment — anchor this (store it in a
// separate trust domain) to make the journal tamper-evident against later rewrites.
func Sign(head []byte, priv ed25519.PrivateKey) []byte {
	return ed25519.Sign(priv, head)
}

// VerifySignature reports whether sig is a valid signature of head under pub. A public key of the
// wrong length is reported as not verifying rather than panicking.
func VerifySignature(head, sig []byte, pub ed25519.PublicKey) bool {
	return len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, head, sig)
}
