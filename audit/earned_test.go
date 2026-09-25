package audit

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

func mustSign(t *testing.T, g Grant, s Signer) SignedGrant {
	t.Helper()
	sg, err := SignGrant(g, s)
	if err != nil {
		t.Fatalf("SignGrant %s: %v", g.ID, err)
	}
	return sg
}

// TestEarnedAuthority_Ladder walks the control loop: baseline, promotion on a clean streak up to
// the ceiling, cap at the top, reset on anomaly, and confirms every earned grant is a child of the
// root that verifies within the ceiling.
func TestEarnedAuthority_Ladder(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := Ed25519Signer{Priv: priv}
	verifier := func(string) (Verifier, bool) { return Ed25519Verifier{Pub: pub}, true }

	root := mustSign(t, Grant{ID: "root", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "10"}}, signer)

	assertWithinCeiling := func(ea *EarnedAuthority) {
		t.Helper()
		if ea.Grant().Grant.ParentRef != root.Grant.Digest() {
			t.Fatalf("earned grant is not a child of root")
		}
		ok, err := VerifyDelegationChain([]SignedGrant{root, ea.Grant()}, verifier, AttenuatesNumericScope("limit"))
		if err != nil || !ok {
			t.Fatalf("earned grant at limit %d did not verify within the ceiling: ok=%v err=%v", ea.Limit(), ok, err)
		}
	}

	ea, err := NewEarnedAuthority([]int{2, 5, 10}, 3, root, signer, "exec")
	if err != nil {
		t.Fatalf("NewEarnedAuthority: %v", err)
	}
	if ea.Tier() != 0 || ea.Limit() != 2 {
		t.Fatalf("baseline should be tier 0 limit 2, got tier %d limit %d", ea.Tier(), ea.Limit())
	}
	assertWithinCeiling(ea)

	// Two compliant actions: no promotion yet.
	for i := 0; i < 2; i++ {
		if p, _ := ea.RecordCompliant(); p {
			t.Fatalf("promoted too early at action %d", i)
		}
	}
	// Third crosses the threshold: promote to limit 5.
	if p, _ := ea.RecordCompliant(); !p || ea.Limit() != 5 {
		t.Fatalf("expected promotion to limit 5, got promoted=%v limit=%d", p, ea.Limit())
	}
	assertWithinCeiling(ea)

	// Three more: promote to the ceiling rung, limit 10.
	ea.RecordCompliant()
	ea.RecordCompliant()
	if p, _ := ea.RecordCompliant(); !p || ea.Limit() != 10 {
		t.Fatalf("expected promotion to the ceiling limit 10, got promoted=%v limit=%d", p, ea.Limit())
	}
	assertWithinCeiling(ea)

	// Further clean actions cannot exceed the top rung.
	for i := 0; i < 6; i++ {
		if p, _ := ea.RecordCompliant(); p {
			t.Fatalf("promoted past the ceiling")
		}
	}
	if ea.Limit() != 10 {
		t.Fatalf("limit should stay at the ceiling 10, got %d", ea.Limit())
	}

	// An anomaly resets to baseline immediately.
	if c, _ := ea.FlagAnomaly(); !c || ea.Tier() != 0 || ea.Limit() != 2 {
		t.Fatalf("anomaly should reset to baseline, got changed=%v tier=%d limit=%d", c, ea.Tier(), ea.Limit())
	}
	assertWithinCeiling(ea)
}

// TestEarnedAuthority_CeilingEnforced confirms the controller refuses a ladder that would let an
// earned grant exceed the root grant.
func TestEarnedAuthority_CeilingEnforced(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := Ed25519Signer{Priv: priv}
	root := mustSign(t, Grant{ID: "root", Subject: "desk", Scope: map[string]string{"limit": "10"}}, signer)

	if _, err := NewEarnedAuthority([]int{2, 20}, 3, root, signer, "exec"); err == nil {
		t.Fatal("expected an error: a ladder rung (20) exceeds the root ceiling (10)")
	}
}
