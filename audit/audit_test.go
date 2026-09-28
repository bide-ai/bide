package audit_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// record writes a sequence of durable value-steps into a run's journal.
func record(t *testing.T, store agent.Durable, runID string, vals ...string) {
	t.Helper()
	for i, v := range vals {
		if _, err := agent.Step(context.Background(), store, runID, fmt.Sprintf("s%d", i),
			func(context.Context) (string, error) { return v, nil }); err != nil {
			t.Fatal(err)
		}
	}
}

func head(t *testing.T, store agent.Durable, runID string) []byte {
	t.Helper()
	h, err := audit.Head(context.Background(), store, runID)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Byte-identical journals commit to the same head; a single differing record diverges.
func TestHead_DeterministicAndTamperEvident(t *testing.T) {
	s1, s2 := agent.NewMemStore(), agent.NewMemStore()
	record(t, s1, "r", "alpha", "beta", "gamma")
	record(t, s2, "r", "alpha", "beta", "gamma")
	if !bytes.Equal(head(t, s1, "r"), head(t, s2, "r")) {
		t.Fatal("identical journals must have the same head")
	}
	if len(head(t, s1, "r")) != 32 {
		t.Fatal("head should be a 32-byte SHA-256")
	}

	// Tamper: one record differs -> head diverges.
	s3 := agent.NewMemStore()
	record(t, s3, "r", "alpha", "TAMPERED", "gamma")
	if bytes.Equal(head(t, s1, "r"), head(t, s3, "r")) {
		t.Fatal("a modified record must change the head (tamper-evidence)")
	}

	// Order matters: same records, different order -> different head.
	s4 := agent.NewMemStore()
	record(t, s4, "r", "beta", "alpha", "gamma")
	if bytes.Equal(head(t, s1, "r"), head(t, s4, "r")) {
		t.Fatal("reordering records must change the head")
	}
}

// The empty journal has a stable, non-zero head (the domain seed chain).
func TestHead_EmptyJournalStable(t *testing.T) {
	s1, s2 := agent.NewMemStore(), agent.NewMemStore()
	if !bytes.Equal(head(t, s1, "none"), head(t, s2, "none")) {
		t.Fatal("empty journals must share a head")
	}
}

// A signed head verifies; a different head or key does not.
func TestSign_Roundtrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	store := agent.NewMemStore()
	record(t, store, "r", "a", "b")
	h := head(t, store, "r")

	sig := audit.Sign(h, priv)
	if !audit.VerifySignature(h, sig, pub) {
		t.Fatal("valid signature must verify")
	}
	if audit.VerifySignature([]byte("not the head, padded to length.."), sig, pub) {
		t.Fatal("signature must not verify against a different head")
	}
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if audit.VerifySignature(h, sig, otherPub) {
		t.Fatal("signature must not verify under a different key")
	}
}
