package audit_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
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
			func(context.Context) (string, error) { return v, nil }, agent.StepSafety(agent.Safety{ReadOnly: true})); err != nil {
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
	s1 := agent.NewMemStore()
	record(t, s1, "r", "alpha", "beta", "gamma")
	recs, err := s1.History(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	s2 := fixedHistory(recs) // the same records, salts included, held elsewhere
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
	s4 := fixedHistory{recs[1], recs[0], recs[2]}
	if bytes.Equal(head(t, s1, "r"), head(t, s4, "r")) {
		t.Fatal("reordering records must change the head")
	}

	// The head commits to each record's salt: the same content under another salt diverges.
	resalted := append(fixedHistory(nil), recs...)
	resalted[1].Salt = bytes.Repeat([]byte{1}, agent.SaltSize)
	if bytes.Equal(head(t, s1, "r"), head(t, resalted, "r")) {
		t.Fatal("a record's salt must be part of the head")
	}
}

// The empty journal has a stable, non-zero head (the domain seed chain).
func TestHead_EmptyJournalStable(t *testing.T) {
	s1, s2 := agent.NewMemStore(), agent.NewMemStore()
	if !bytes.Equal(head(t, s1, "none"), head(t, s2, "none")) {
		t.Fatal("empty journals must share a head")
	}
}

// The head chains over each record's journal encoding (agent.EncodeRecord), the bytes a store
// persists, so a record whose JSON carries HTML-significant characters is committed as stored.
// The chain's seed names the encoding version, bide.audit.v2: v2 is the first chain over the
// journal encoding without HTML escaping, so a head over the older encoding cannot be mistaken
// for a fork of a head over the newer one.
func TestHead_ChainsTheJournalEncoding(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	rec, err := store.Do(ctx, "run", "c1", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: []byte(`{"html":"<b>a & b</b>"}`)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := agent.EncodeRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	seed := sha256.Sum256([]byte("bide.audit.v2"))
	want := sha256.Sum256(append(seed[:], leaf...))
	if got := head(t, store, "run"); !bytes.Equal(got, want[:]) {
		t.Fatalf("head = %x, want the chain over the journal encoding %q (%x)", got, leaf, want)
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
