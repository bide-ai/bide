package main

// In-process test of the cryptographic-conformance story: run the flow against a
// MemStore, commit to its journal with a signed audit tree head, prove the flow:digest
// record is included under the signed root, and check the proven digest equals the
// declared flow's Digest(). It also asserts the proof is REJECTED when the record is
// tampered, which is the property that makes the check meaningful.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

// TestCryptographicConformance proves offline that a run followed THIS declared
// topology: the flow:digest record Run journals first is included under a signed tree
// head and equals flow.Digest().
func TestCryptographicConformance(t *testing.T) {
	ctx := context.Background()
	flow, err := buildFlow(config{})
	if err != nil {
		t.Fatalf("buildFlow: %v", err)
	}
	store := agenttest.MemJournal()
	const runID = "crypto-run"
	if _, err := flow.Run(ctx, store, runID, Order{ID: runID, Amount: 500}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Commit to the journal and prove the flow:digest record's inclusion.
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	th, err := audit.NewTreeHead(ctx, store, runID, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("NewTreeHead: %v", err)
	}
	sth, err := audit.SignTreeHead(th, audit.Ed25519Signer{Priv: priv})
	if err != nil {
		t.Fatalf("SignTreeHead: %v", err)
	}

	recs, err := store.History(ctx, runID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	idx := -1
	for i, r := range recs {
		if r.Name == "flow:digest" {
			idx = i
			break
		}
	}
	if idx != 2 {
		t.Fatalf("flow:digest record index = %d, want 2 (Run records it after the journal header and the run's start)", idx)
	}

	bundle, err := audit.ProveRecord(ctx, store, runID, idx, sth)
	if err != nil {
		t.Fatalf("ProveRecord: %v", err)
	}

	// The proof verifies under the signer's key, and the proven digest equals the
	// declared topology's Digest(): the run committed to THIS diagram.
	v := audit.Ed25519Verifier{Pub: pub}
	if err := bundle.Verify(v); err != nil {
		t.Fatalf("bundle.Verify: %v", err)
	}
	rec, err := bundle.Record()
	if err != nil {
		t.Fatal(err)
	}
	var proven string
	if err := json.Unmarshal(rec.Result, &proven); err != nil {
		t.Fatalf("decode proven digest: %v", err)
	}
	if proven != flow.Digest() {
		t.Fatalf("proven digest %q != flow.Digest() %q", proven, flow.Digest())
	}

	// Tamper the disclosed record: the inclusion proof must no longer verify, so a
	// forged topology digest cannot be passed off as committed under the signed root.
	forged := bundle
	forged.RecordBytes = bytes.Replace(bundle.RecordBytes, []byte(`"result":"`+flow.Digest()+`"`), []byte(`"result":"deadbeef"`), 1)
	if bytes.Equal(forged.RecordBytes, bundle.RecordBytes) {
		t.Fatal("found no digest to tamper with in the record bytes")
	}
	if err := forged.Verify(v); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("a tampered flow:digest record: err = %v, want ErrNotVerified", err)
	}
}
