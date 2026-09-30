// Package audit turns an agent's durable journal into a verifiable, tamper-evident
// record. The journal already captures every step of a run (model turns, tool calls and
// results, approvals, saga outcomes); this package commits to that history with a hash
// chain and Merkle trees and lets you sign/verify the commitment under ed25519, ML-DSA-65,
// or a hybrid of both. Stdlib-only.
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
	"crypto/sha256"
	"fmt"

	"github.com/bide-ai/bide/agent"
)

// domain separates this hash use from any other, seeding the chain. It names the version of the
// record encoding the chain covers: v2 chains over the bytes the journal stores for each record,
// which do not HTML-escape, so a head over the v1 encoding (where <, >, and & were escaped) is told
// apart from a head over the same journal in v2 rather than read as a fork.
var domain = sha256.Sum256([]byte("bide.audit.v2"))

// headSigTag domain-separates a signature over a linear head from every other message a key signs.
const headSigTag = "bide.audit.head.v1\x00"

// Head returns the hash-chain commitment to runID's journal: head_0 = H(domain), and
// head_i = H(head_{i-1} || stored(record_i)) over the records in persisted order, where stored is
// the bytes the journal stores for the record (agent.Record.Raw), verbatim. Two runs produce the
// same head iff their journals are byte-identical in the same order, so the head is a
// deterministic fingerprint of the entire execution history.
func Head(ctx context.Context, store agent.Durable, runID string) ([]byte, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	head := domain[:]
	for i, r := range recs {
		b := r.Raw()
		if b == nil {
			return nil, fmt.Errorf("audit: record %d (%q) has no stored bytes", i, r.Name)
		}
		sum := sha256.Sum256(append(append([]byte{}, head...), b...))
		head = sum[:]
	}
	return head, nil
}

// Sign returns s's signature over a head commitment ("bide.audit.head.v1\x00" || head). Anchor it
// (store it in a separate trust domain) to make the journal tamper-evident against later rewrites.
func Sign(head []byte, s Signer) ([]byte, error) {
	if err := checkSigner(s); err != nil {
		return nil, err
	}
	return s.Sign(append([]byte(headSigTag), head...))
}

// VerifySignature returns nil if sig is a valid signature of head (see Sign) under v, and an error
// wrapping ErrNotVerified otherwise.
func VerifySignature(head, sig []byte, v Verifier) error {
	if err := checkVerifier(v); err != nil {
		return err
	}
	if !v.Verify(append([]byte(headSigTag), head...), sig) {
		return notVerified("audit: the head signature does not verify under this %s key", v.Alg())
	}
	return nil
}
