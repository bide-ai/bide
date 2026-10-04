package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent/agenttest"
)

// keyring issues a signer/verifier per issuer name, standing in for a PKI/IdP.
type keyring struct {
	priv map[string]ed25519.PrivateKey
	pub  map[string]ed25519.PublicKey
}

func newKeyring(issuers ...string) *keyring {
	k := &keyring{priv: map[string]ed25519.PrivateKey{}, pub: map[string]ed25519.PublicKey{}}
	for _, name := range issuers {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		k.priv[name], k.pub[name] = priv, pub
	}
	return k
}

func (k *keyring) signer(issuer string) Signer { return Ed25519Signer{Priv: k.priv[issuer]} }
func (k *keyring) verifier(issuer string) (Verifier, bool) {
	pub, ok := k.pub[issuer]
	return Ed25519Verifier{Pub: pub}, ok
}

// TestSignedGrant_VerifyAndTamper confirms an issuer-signed grant verifies under the issuer's key,
// fails under a different key, and fails if the grant is altered after signing.
func TestSignedGrant_VerifyAndTamper(t *testing.T) {
	kr := newKeyring("desk-EQ-US", "impostor")
	g := Grant{ID: "grant#a1b2", Issuer: "desk-EQ-US", Subject: "exec-agent@1.4.2",
		Scope: map[string]string{"market": "EQ-US", "limit": "7"}, NotAfterUnix: 1000}
	sg, err := SignGrant(g, kr.signer("desk-EQ-US"))
	if err != nil {
		t.Fatalf("SignGrant: %v", err)
	}

	v, _ := kr.verifier("desk-EQ-US")
	if err := sg.Verify(v); err != nil {
		t.Fatalf("valid grant did not verify: err=%v", err)
	}
	imp, _ := kr.verifier("impostor")
	if err := sg.Verify(imp); err == nil {
		t.Fatalf("grant verified under the wrong issuer key")
	}
	tampered := sg
	tampered.Grant.Scope = map[string]string{"market": "EQ-US", "limit": "9"} // widen after signing
	if err := tampered.Verify(v); err == nil {
		t.Fatalf("tampered grant (limit 7 -> 9) still verified")
	}
	if !g.Expired(1001) || g.Expired(999) {
		t.Fatalf("expiry check wrong: Expired(1001)=%v Expired(999)=%v", g.Expired(1001), g.Expired(999))
	}
}

// TestGrant_AnchorAndProve confirms a signed grant anchors as a leaf and its inclusion proof
// verifies and recovers the grant, so an action's AuthorityRef can link to an in-log authority.
func TestGrant_AnchorAndProve(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	const runID = "run1"
	kr := newKeyring("desk-EQ-US")
	g := Grant{ID: "grant#a1b2", Issuer: "desk-EQ-US", Subject: "exec-agent@1.4.2",
		Scope: map[string]string{"limit": "7"}}
	sg, _ := SignGrant(g, kr.signer("desk-EQ-US"))

	if _, err := RecordGrant(ctx, store, runID, sg); err != nil {
		t.Fatalf("RecordGrant: %v", err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader) // log operator key, distinct from the issuer's
	th, err := NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		t.Fatalf("NewTreeHead: %v", err)
	}
	sth := signTH(t, th, priv)

	pb, err := ProveGrant(ctx, store, runID, g.Digest(), sth)
	if err != nil {
		t.Fatalf("ProveGrant: %v", err)
	}
	if err := pb.Verify(edV(pub)); err != nil {
		t.Fatalf("grant bundle did not verify: err=%v", err)
	}
	var got SignedGrant
	if err := json.Unmarshal(recOf(t, pb).Result, &got); err != nil {
		t.Fatalf("decode grant leaf: %v", err)
	}
	if got.Grant.Digest() != g.Digest() {
		t.Fatalf("anchored grant digest mismatch")
	}
}

// chain builds a root -> desk -> exec delegation chain with the given leaf limit, each hop signed
// by its issuer, ParentRef hash-linked to its parent.
func chain(kr *keyring, leafLimit string) []SignedGrant {
	g0 := Grant{ID: "g0", Issuer: "corp-treasury", Subject: "desk-EQ-US", Scope: map[string]string{"limit": "10"}}
	g1 := Grant{ID: "g1", Issuer: "desk-EQ-US", Subject: "exec-agent@1.4.2",
		Scope: map[string]string{"limit": "7"}, ParentRef: g0.Digest()}
	g2 := Grant{ID: "g2", Issuer: "exec-agent@1.4.2", Subject: "exec-subagent@1.0",
		Scope: map[string]string{"limit": leafLimit}, ParentRef: g1.Digest()}
	s0, _ := SignGrant(g0, kr.signer("corp-treasury"))
	s1, _ := SignGrant(g1, kr.signer("desk-EQ-US"))
	s2, _ := SignGrant(g2, kr.signer("exec-agent@1.4.2"))
	return []SignedGrant{s0, s1, s2}
}

// TestDelegationChain confirms a well-formed, attenuating chain verifies, and that a widening
// sub-grant or a broken parent link is rejected.
func TestDelegationChain(t *testing.T) {
	kr := newKeyring("corp-treasury", "desk-EQ-US", "exec-agent@1.4.2")
	atten := ScopeRules{"limit": NumericAtMost}

	// Valid: 10 -> 7 -> 3, each hop narrows.
	if err := VerifyDelegationChain(chain(kr, "3"), kr.verifier, atten); err != nil {
		t.Fatalf("valid delegation chain rejected: err=%v", err)
	}

	// Widening: sub-grant asks for 8 under a parent limited to 7. Must be rejected.
	if err := VerifyDelegationChain(chain(kr, "8"), kr.verifier, atten); err == nil {
		t.Fatalf("widening sub-grant (7 -> 8) was accepted")
	}

	// Broken link: tamper the leaf's ParentRef so it no longer hashes to its parent.
	broken := chain(kr, "3")
	broken[2].Grant.ParentRef = "deadbeef"
	broken[2], _ = SignGrant(broken[2].Grant, kr.signer("exec-agent@1.4.2")) // re-sign so it is the link, not the sig, that fails
	if err := VerifyDelegationChain(broken, kr.verifier, atten); err == nil {
		t.Fatalf("chain with a broken parent link was accepted")
	}
}
