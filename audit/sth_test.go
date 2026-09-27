package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"testing"

	agent "github.com/blackwell-systems/bide"
)

func journal(t *testing.T, store agent.Durable, runID string, vals ...string) {
	t.Helper()
	for i, v := range vals {
		if _, err := agent.Step(context.Background(), store, runID, fmt.Sprintf("s%d", i),
			func(context.Context) (string, error) { return v, nil }); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSTH_SignVerifyAndTamper(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	store := agent.NewMemStore()
	journal(t, store, "run", "a", "b", "c")

	th, err := NewTreeHead(context.Background(), store, "run", 1_700_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if th.Size != 3 || len(th.Root) != 32 {
		t.Fatalf("tree head = %+v, want size 3 / 32-byte root", th)
	}
	sth := SignTreeHead(th, priv)
	if !sth.Verify(pub) {
		t.Fatal("valid STH failed to verify")
	}

	// Any field change breaks the signature (the sig commits to size + root + time).
	bad := sth
	bad.Size = 2
	if bad.Verify(pub) {
		t.Fatal("STH verified after Size tampering")
	}
	bad = sth
	bad.Timestamp++
	if bad.Verify(pub) {
		t.Fatal("STH verified after Timestamp tampering")
	}
	bad = sth
	bad.Root = append([]byte(nil), sth.Root...)
	bad.Root[0] ^= 0xff
	if bad.Verify(pub) {
		t.Fatal("STH verified after Root tampering")
	}
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if sth.Verify(otherPub) {
		t.Fatal("STH verified under the wrong key")
	}
}

// The whole compliance flow: sign an STH, later disclose ONE record with an inclusion
// proof, and a third party verifies it against the STH's signed root — exposing nothing
// else. Then prove the journal only grew (consistency) between two signed STHs.
func TestSTH_EndToEndComplianceFlow(t *testing.T) {
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	store := agent.NewMemStore()
	journal(t, store, "case-42", "open", "charge-500", "email-receipt")

	// Vendor publishes a signed commitment to the run so far.
	th1, _ := NewTreeHead(ctx, store, "case-42", 1000)
	sth1 := SignTreeHead(th1, priv)

	// Auditor: verify the STH, then verify a single disclosed record against its root.
	if !sth1.Verify(pub) {
		t.Fatal("auditor could not verify the STH")
	}
	recs, _ := store.History(ctx, "case-42")
	proof, _ := Prove(ctx, store, "case-42", 1) // disclose only the "charge-500" record
	ok, err := VerifyInclusion(sth1.Root, recs[1], proof)
	if err != nil || !ok {
		t.Fatalf("auditor could not verify disclosed record against signed root: ok=%v err=%v", ok, err)
	}

	// The run continues; a new signed STH is published.
	journal(t, store, "case-42", "close")
	th2, _ := NewTreeHead(ctx, store, "case-42", 2000)
	sth2 := SignTreeHead(th2, priv)

	// Prove the earlier signed state is an append-only prefix of the later one.
	cproof, _ := ProveConsistency(ctx, store, "case-42", sth1.Size)
	if !sth2.Verify(pub) || !VerifyConsistency(sth1.Root, sth2.Root, cproof) {
		t.Fatal("could not prove append-only growth between the two signed STHs")
	}
}
