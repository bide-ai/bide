package audit_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"

	"github.com/bide-ai/bide/internal/journaltest"
)

// record writes a sequence of durable value-steps into a run's journal.
func record(t *testing.T, store *agent.Journal, runID string, vals ...string) {
	t.Helper()
	for i, v := range vals {
		if _, err := agent.Step(context.Background(), store, runID, fmt.Sprintf("s%d", i),
			func(context.Context) (string, error) { return v, nil }, agent.WithSafety(agent.Safety{ReadOnly: true})); err != nil {
			t.Fatal(err)
		}
	}
}

func head(t *testing.T, store *agent.Journal, runID string) []byte {
	t.Helper()
	h, err := audit.Head(context.Background(), store, runID)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Byte-identical journals commit to the same head; a single differing record diverges.
func TestHead_DeterministicAndTamperEvident(t *testing.T) {
	s1 := agenttest.MemJournal()
	record(t, s1, "r", "alpha", "beta", "gamma")
	recs, err := s1.History(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	s2 := fixedHistory(recs).journal() // the same records, salts included, held elsewhere
	if !bytes.Equal(head(t, s1, "r"), head(t, s2, "r")) {
		t.Fatal("identical journals must have the same head")
	}
	if len(head(t, s1, "r")) != 32 {
		t.Fatal("head should be a 32-byte SHA-256")
	}

	// Tamper: one record differs -> head diverges.
	s3 := agenttest.MemJournal()
	record(t, s3, "r", "alpha", "TAMPERED", "gamma")
	if bytes.Equal(head(t, s1, "r"), head(t, s3, "r")) {
		t.Fatal("a modified record must change the head (tamper-evidence)")
	}

	// Order matters: same records, different order -> different head.
	s4 := fixedHistory{recs[0], recs[2], recs[1], recs[3]}.journal() // the header stays first
	if bytes.Equal(head(t, s1, "r"), head(t, s4, "r")) {
		t.Fatal("reordering records must change the head")
	}

	// The head commits to each record's salt: the same content under another salt diverges.
	resalted := append(fixedHistory(nil), recs...)
	resalted[1] = stored(withSalt(resalted[1], bytes.Repeat([]byte{1}, agent.SaltSize)))
	if bytes.Equal(head(t, s1, "r"), head(t, resalted.journal(), "r")) {
		t.Fatal("a record's salt must be part of the head")
	}
}

// The empty journal has a stable, non-zero head (the domain seed chain).
func TestHead_EmptyJournalStable(t *testing.T) {
	s1, s2 := agent.NewMemStore(), agent.NewMemStore()
	j2 := agenttest.MustJournal(s2)
	j := agenttest.MustJournal(s1)
	if !bytes.Equal(head(t, j, "none"), head(t, j2, "none")) {
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
	store := agenttest.MemJournal()
	rec, err := journaltest.Do(ctx, store, "run", "c1", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: []byte(`{"html":"<b>a & b</b>"}`)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := agent.EncodeRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := store.History(ctx, "run")
	header, err := agent.EncodeRecord(all[0]) // the journal header is the chain's first record
	if err != nil {
		t.Fatal(err)
	}
	seed := sha256.Sum256([]byte("bide.audit.v2"))
	first := sha256.Sum256(append(seed[:], header...))
	want := sha256.Sum256(append(first[:], leaf...))
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
	store := agenttest.MemJournal()
	record(t, store, "r", "a", "b")
	h := head(t, store, "r")

	sig, _ := audit.Sign(h, edS(priv))
	if audit.VerifySignature(h, sig, edV(pub)) != nil {
		t.Fatal("valid signature must verify")
	}
	if audit.VerifySignature([]byte("not the head, padded to length.."), sig, edV(pub)) == nil {
		t.Fatal("signature must not verify against a different head")
	}
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if audit.VerifySignature(h, sig, edV(otherPub)) == nil {
		t.Fatal("signature must not verify under a different key")
	}
}

// edS and edV wrap raw ed25519 keys as an audit Signer and Verifier.
func edS(priv ed25519.PrivateKey) audit.Signer { return audit.Ed25519Signer{Priv: priv} }
func edV(pub ed25519.PublicKey) audit.Verifier { return audit.Ed25519Verifier{Pub: pub} }

// signTH signs th with priv under ed25519, failing the test on error.
func signTH(tb testing.TB, th audit.TreeHead, priv ed25519.PrivateKey) audit.SignedTreeHead {
	tb.Helper()
	sth, err := audit.SignTreeHead(th, edS(priv))
	if err != nil {
		tb.Fatal(err)
	}
	return sth
}

// mustAuditedStore wraps inner in an AuditedStore signing with priv, failing the test on error.
func mustAuditedStore(tb testing.TB, inner *agent.Journal, priv ed25519.PrivateKey, anchor audit.Anchor) *audit.AuditedStore {
	tb.Helper()
	s, err := audit.NewAuditedStore(inner.Store(), edS(priv), anchor)
	if err != nil {
		tb.Fatal(err)
	}
	return s
}

// recOf decodes the record a bundle proves, failing the test if it does not decode.
func recOf(tb testing.TB, b audit.ProofBundle) agent.Record {
	tb.Helper()
	r, err := b.Record()
	if err != nil {
		tb.Fatal(err)
	}
	return r
}

// editRec decodes the record b proves, applies edit to it, and puts its journal encoding back
// in b: a tampered bundle.
func editRec(tb testing.TB, b *audit.ProofBundle, edit func(*agent.Record)) {
	tb.Helper()
	r := recOf(tb, *b)
	edit(&r)
	enc, err := agent.EncodeRecord(r)
	if err != nil {
		tb.Fatal(err)
	}
	b.RecordBytes = enc
}

// reportErr checks a report verifier's error against its verdict: nil when ok, ErrNotVerified when
// not. It returns a description of any other combination, or nil.
func reportErr(ok bool, err error) error {
	switch {
	case ok && err == nil, !ok && errors.Is(err, audit.ErrNotVerified):
		return nil
	case ok:
		return fmt.Errorf("verdict OK with error %w", err)
	default:
		return fmt.Errorf("verdict not OK with error %v, want ErrNotVerified", err)
	}
}
