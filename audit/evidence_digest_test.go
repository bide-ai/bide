package audit_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
)

// limitedSigner stands in for a KMS that signs at most max bytes (AWS KMS: 4096): it refuses any
// longer message, as the service does, and records the longest message it was asked to sign.
type limitedSigner struct {
	audit.Signer
	max     int
	longest int
	calls   int
}

func (l *limitedSigner) Sign(m []byte) ([]byte, error) {
	l.calls++
	l.longest = max(l.longest, len(m))
	if len(m) > l.max {
		return nil, fmt.Errorf("kms: message is %d bytes, the limit is %d", len(m), l.max)
	}
	return l.Signer.Sign(m)
}

// buildBigRun journals a run of n completed tool calls.
func buildBigRun(t *testing.T, n int) (*agent.Journal, string) {
	t.Helper()
	ctx := context.Background()
	j := agenttest.MustJournal(agent.NewMemStore())
	const runID = "run-big"
	for i := range n {
		id := fmt.Sprintf("call-%03d", i)
		if _, err := journaltest.Do(ctx, j, runID, "call:"+id, func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepToolResult, ToolUseID: id, Result: json.RawMessage(`{"charged":true,"note":"a payment that is long enough to make the package large"}`)}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return j, runID
}

// A package of 50 actions is far over 4096 bytes as JSON; sealing it must still work with a signer
// that refuses messages over 4096 bytes, because the seal signs a fixed-size digest.
func TestEvidence_SealedOverDigestWithSizeLimitedSigner(t *testing.T) {
	ctx := context.Background()
	j, runID := buildBigRun(t, 50)
	for name, inner := range map[string]audit.Signer{
		"ed25519":    p11Signers(t)["ed25519"],
		"ecdsa-p256": newP256Signer(t),
	} {
		t.Run(name, func(t *testing.T) {
			s := &limitedSigner{Signer: inner, max: 4096}
			pkg, err := audit.Evidence(ctx, j, runID, s, 1700000000)
			if err != nil {
				t.Fatalf("Evidence with a 4096-byte signer: %v", err)
			}
			raw, _ := json.Marshal(pkg)
			if len(raw) <= 4096*2 || len(pkg.Actions) != 50 {
				t.Fatalf("package is %d bytes with %d actions; the test needs a large package", len(raw), len(pkg.Actions))
			}
			if s.longest > 256 {
				t.Fatalf("the signer was asked to sign %d bytes; the seal must be a fixed-size digest message", s.longest)
			}
			v, _ := audit.VerifierOf(inner)
			rep, err := pkg.Verify(v)
			if err != nil || !rep.OK {
				t.Fatalf("verify: ok=%v err=%v problems=%v", rep.OK, err, rep.Problems)
			}
			// Any change after sealing still breaks it.
			pkg.Label = "edited"
			if rep, _ := pkg.Verify(v); rep.OK {
				t.Fatal("an edited package verified")
			}
		})
	}
}

// The seal message is exactly the v6 tag followed by SHA-256 of the package JSON with the signature
// empty, so an independent implementation can verify the seal.
func TestEvidence_SealMessageIsTagAndDigest(t *testing.T) {
	ctx := context.Background()
	j, runID := buildBigRun(t, 3)
	s := newP256Signer(t)
	pkg, err := audit.Evidence(ctx, j, runID, s, 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Format != "bide.audit.evidence.v6" {
		t.Fatalf("format %q", pkg.Format)
	}
	unsealed := pkg
	unsealed.Signature = nil
	b, _ := json.Marshal(unsealed)
	d := sha256.Sum256(b)
	msg := append([]byte("bide.audit.evidence.v6.seal\x00"), d[:]...)
	v, _ := audit.VerifierOf(s)
	if !v.Verify(msg, pkg.Signature) {
		t.Fatal("the seal is not a signature over tag || SHA-256(package JSON)")
	}
	// The whole-package message of format v5 is not what a v6 seal signs.
	if v.Verify(append([]byte("bide.audit.evidence.v5.seal\x00"), b...), pkg.Signature) {
		t.Fatal("a v6 seal verified as a v5 seal")
	}
}

// legacySeal re-seals p as a v5 package: format v5, the signature over the v5 tag and the whole JSON.
func legacySeal(t *testing.T, p audit.EvidencePackage, s audit.Signer) audit.EvidencePackage {
	t.Helper()
	p.Format = audit.EvidenceFormatV5
	p.Signature = nil
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	p.Signature, err = s.Sign(append([]byte("bide.audit.evidence.v5.seal\x00"), b...))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Evidence sealed before v6 (format v5, whole-package seal) still verifies, and the two formats
// cannot be mistaken for each other.
func TestEvidence_V5StillVerifies(t *testing.T) {
	ctx := context.Background()
	store, runID, pub, priv := buildEvidenceRun(t)
	s := edS(priv)
	v6, err := audit.Evidence(ctx, store, runID, s, 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	v5 := legacySeal(t, v6, s)

	// Through the wire path a CLI uses.
	raw, _ := json.Marshal(v5)
	var got audit.EvidencePackage
	if err := audit.UnmarshalStrict(raw, &got); err != nil {
		t.Fatalf("a v5 package is refused on read: %v", err)
	}
	rep, err := got.Verify(edV(pub))
	if err != nil || !rep.OK {
		t.Fatalf("v5 verify: ok=%v err=%v problems=%v", rep.OK, err, rep.Problems)
	}

	// A v5 package with a changed label fails.
	bad := got
	bad.Label = "edited"
	if rep, _ := bad.Verify(edV(pub)); rep.OK {
		t.Fatal("an edited v5 package verified")
	}
	// A v6 package relabelled v5, and a v5 package relabelled v6, fail: the seals differ.
	down := v6
	down.Format = audit.EvidenceFormatV5
	if rep, _ := down.Verify(edV(pub)); rep.OK {
		t.Fatal("a v6 package relabelled v5 verified")
	}
	up := v5
	up.Format = audit.EvidenceFormat
	if rep, _ := up.Verify(edV(pub)); rep.OK {
		t.Fatal("a v5 package relabelled v6 verified")
	}
	// A v5 package is never re-sealed (that would write a v6 seal under a v5 format).
	if err := got.Seal(s); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Seal of a v5 package = %v, want ErrConfig", err)
	}
	// Older formats are still refused with the format error.
	old := strings.Replace(string(raw), `"bide.audit.evidence.v5"`, `"bide.audit.evidence.v4"`, 1)
	var o audit.EvidencePackage
	if err := audit.UnmarshalStrict([]byte(old), &o); !errors.Is(err, audit.ErrFormat) {
		t.Fatalf("v4 = %v, want ErrFormat", err)
	}
	if _, err := o.Verify(edV(pub)); !errors.Is(err, audit.ErrFormat) {
		t.Fatalf("v4 Verify = %v, want ErrFormat", err)
	}
	if !bytes.Contains(raw, []byte("evidence.v5")) {
		t.Fatal("test package is not v5")
	}
}

// The seal binds the signing scheme through the package's Alg and key, so a P-256 package does not
// verify under an Ed25519 key made from the same bytes, and vice versa.
func TestEvidence_P256AndEd25519SealsDoNotCross(t *testing.T) {
	ctx := context.Background()
	j, runID := buildBigRun(t, 2)
	p := newP256Signer(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pp, err := audit.Evidence(ctx, j, runID, p, 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := audit.Evidence(ctx, j, runID, edS(priv), 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	pv, _ := audit.VerifierOf(p)
	if rep, _ := pp.Verify(edV(pub)); rep.OK {
		t.Fatal("P-256 package verified under Ed25519")
	}
	if rep, _ := ep.Verify(pv); rep.OK {
		t.Fatal("Ed25519 package verified under P-256")
	}
	swapped := pp
	swapped.Signature = ep.Signature
	if rep, _ := swapped.Verify(pv); rep.OK {
		t.Fatal("an Ed25519 seal on a P-256 package verified")
	}
}
