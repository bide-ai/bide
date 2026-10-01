package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"testing"

	"github.com/bide-ai/bide/agent"
)

func journal(t *testing.T, store agent.Durable, runID string, vals ...string) {
	t.Helper()
	for i, v := range vals {
		if _, err := agent.Step(context.Background(), store, runID, fmt.Sprintf("s%d", i),
			func(context.Context) (string, error) { return v, nil }, agent.WithSafety(agent.Safety{ReadOnly: true})); err != nil {
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
	if th.Size != 4 || len(th.Root) != 32 { // the journal header, then a, b, c
		t.Fatalf("tree head = %+v, want size 4 / 32-byte root", th)
	}
	sth := signTH(t, th, priv)
	if sth.Verify(edV(pub)) != nil {
		t.Fatal("valid STH failed to verify")
	}

	// Any field change breaks the signature (the sig commits to size + root + time).
	bad := sth
	bad.Size = 2
	if bad.Verify(edV(pub)) == nil {
		t.Fatal("STH verified after Size tampering")
	}
	bad = sth
	bad.TimestampNanos++
	if bad.Verify(edV(pub)) == nil {
		t.Fatal("STH verified after Timestamp tampering")
	}
	bad = sth
	bad.Root = append([]byte(nil), sth.Root...)
	bad.Root[0] ^= 0xff
	if bad.Verify(edV(pub)) == nil {
		t.Fatal("STH verified after Root tampering")
	}
	bad = sth
	bad.RunID = "other"
	if bad.Verify(edV(pub)) == nil {
		t.Fatal("STH verified after RunID tampering")
	}
	for _, kind := range []string{TreeEvents, TreeToolUse, TreePolicyUsed, "", "absence/"} {
		bad = sth
		bad.Kind = kind
		if bad.Verify(edV(pub)) == nil {
			t.Fatalf("STH verified after Kind tampering to %q", kind)
		}
	}
	bad = sth
	bad.Journal = &TreeRef{Size: 0, Root: merkleRoot(nil)}
	if bad.Verify(edV(pub)) == nil {
		t.Fatal("STH verified after adding a Journal reference")
	}
	// A head that is malformed for its kind never verifies, even when signed: a journal head with a
	// Journal reference, an absence head without one, a negative size, an unknown kind.
	for name, th := range map[string]TreeHead{
		"journal with a source journal": {Kind: TreeJournal, RunID: "run", Journal: &TreeRef{}},
		"absence without one":           {Kind: TreeToolUse, RunID: "run"},
		"negative size":                 {Kind: TreeJournal, RunID: "run", Size: -1},
		"negative journal size":         {Kind: TreeToolUse, RunID: "run", Journal: &TreeRef{Size: -1}},
		"unknown kind":                  {Kind: "ledger", RunID: "run"},
		"bare absence prefix":           {Kind: "absence/", RunID: "run", Journal: &TreeRef{}},
	} {
		if _, err := SignTreeHead(th, edS(priv)); err == nil {
			t.Fatalf("SignTreeHead signed a %s", name)
		}
		if err := forceSignTH(th, priv).Verify(edV(pub)); !errors.Is(err, ErrMalformed) {
			t.Fatalf("a signed %s: err = %v, want ErrMalformed", name, err)
		}
	}
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if sth.Verify(edV(otherPub)) == nil {
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
	sth1 := signTH(t, th1, priv)

	// Auditor: verify the STH, then verify a single disclosed record against its root.
	if sth1.Verify(edV(pub)) != nil {
		t.Fatal("auditor could not verify the STH")
	}
	recs, _ := store.History(ctx, "case-42")
	proof, _ := Prove(ctx, store, "case-42", 1) // disclose only the "charge-500" record
	if err := VerifyInclusion(sth1.Root, recs[1].Raw(), proof); err != nil {
		t.Fatalf("auditor could not verify disclosed record against signed root: err=%v", err)
	}

	// The run continues; a new signed STH is published.
	journal(t, store, "case-42", "close")
	th2, _ := NewTreeHead(ctx, store, "case-42", 2000)
	sth2 := signTH(t, th2, priv)

	// Prove the earlier signed state is an append-only prefix of the later one.
	cproof, _ := ProveConsistency(ctx, store, "case-42", sth1.Size)
	if sth2.Verify(edV(pub)) != nil || VerifyConsistency(sth1.Root, sth2.Root, cproof) != nil {
		t.Fatal("could not prove append-only growth between the two signed STHs")
	}
}
