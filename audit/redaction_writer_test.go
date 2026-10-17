package audit_test

// The tombstone agent.Redact writes, read back by the audit package: every reader keeps working
// on the records that remain, and the ones that cannot project a redacted journal say so.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

// agent.LeafHash, which a redaction records in its tombstone, is the hash audit commits to.
func TestRedactionWriter_LeafHashIsTheJournalLeaf(t *testing.T) {
	for _, b := range [][]byte{nil, []byte("x"), bytes.Repeat([]byte("record"), 1000)} {
		if got, want := agent.LeafHash(b), audit.JournalLeafHash(b); !bytes.Equal(got, want) {
			t.Fatalf("agent.LeafHash(%d bytes) = %x, audit.JournalLeafHash = %x", len(b), got, want)
		}
	}
}

func TestRedactionWriter_ProofsAndHeadsSurviveARealRedaction(t *testing.T) {
	ctx := context.Background()
	s, _ := p11GovernedRun(t)
	j := agenttest.MustJournal(s)
	if _, _, err := s.Insert(ctx, "gov", "run:complete", mustEntry(t, "run:complete", agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`"ok"`)})); err != nil {
		t.Fatal(err)
	}
	signer := p11Signers(t)["ed25519"]
	v := p11Verifier(t, signer)
	ts := p11Now()

	recs, err := j.History(ctx, "gov")
	if err != nil {
		t.Fatal(err)
	}
	before, err := audit.NewTreeHead(ctx, j, "gov", ts)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := audit.SignTreeHead(before, signer)
	if err != nil {
		t.Fatal(err)
	}
	var payIdx, noteIdx = -1, -1
	for i, r := range recs {
		switch r.Name {
		case "call:pay":
			payIdx = i
		case "note":
			noteIdx = i
		}
	}
	noteProof, err := audit.Prove(ctx, j, "gov", noteIdx)
	if err != nil {
		t.Fatal(err)
	}
	noteBytes := recs[noteIdx].Raw()

	first := rootOfPrefix(ctx, t, s, "gov", 3) // an earlier head's root, taken before the redaction
	plan, err := agent.PlanRedaction(ctx, j, "gov", agent.RedactionSelector{Names: []string{"call:pay"}})
	if err != nil || len(plan.Records) != 1 || !plan.Terminal {
		t.Fatalf("PlanRedaction = %+v, %v", plan, err)
	}
	if res, err := agent.Redact(ctx, j, plan, time.UnixMilli(ts)); err != nil || len(res.Redacted) != 1 {
		t.Fatalf("Redact = %+v, %v", res, err)
	}

	// The root, and so every head signed before, is unchanged.
	after, err := audit.NewTreeHead(ctx, j, "gov", ts)
	if err != nil || !bytes.Equal(after.Root, before.Root) || after.Size != before.Size {
		t.Fatalf("the tree head after the redaction = %x (size %d), %v; want the unchanged %x (size %d)", after.Root, after.Size, err, before.Root, before.Size)
	}
	if err := signed.Verify(v); err != nil {
		t.Errorf("a head signed before the redaction no longer verifies: %v", err)
	}
	// A proof made before for a record that remains still verifies, and a new one does too.
	if err := audit.VerifyInclusion(before.Root, noteBytes, noteProof); err != nil {
		t.Errorf("an inclusion proof made before the redaction fails: %v", err)
	}
	np, err := audit.Prove(ctx, j, "gov", noteIdx)
	if err != nil {
		t.Fatal(err)
	}
	if err := audit.VerifyInclusion(after.Root, noteBytes, np); err != nil {
		t.Errorf("an inclusion proof made after the redaction fails: %v", err)
	}
	// The redacted record cannot be proven.
	if _, err := audit.Prove(ctx, j, "gov", payIdx); err == nil {
		t.Errorf("a redacted record was proven")
	}
	// Projections that need the content refuse, as documented.
	rrecs, err := j.History(ctx, "gov")
	if err != nil {
		t.Fatal(err)
	}
	if !rrecs[payIdx].Redacted || rrecs[payIdx].Name != "call:pay" {
		t.Fatalf("the redacted record reads as %+v", rrecs[payIdx])
	}
	if _, err := audit.SignAbsenceRoot(rrecs, audit.ToolUseKeys, after, signer, ts); !errors.Is(err, audit.ErrRedacted) {
		t.Errorf("SignAbsenceRoot over a redacted journal = %v, want ErrRedacted", err)
	}
	if _, err := audit.PoliciesUsed(rrecs); !errors.Is(err, audit.ErrRedacted) {
		t.Errorf("PoliciesUsed over a redacted journal = %v, want ErrRedacted", err)
	}
	if _, err := audit.EventLogFromJournal(ctx, j, "gov"); !errors.Is(err, audit.ErrRedacted) {
		t.Errorf("EventLogFromJournal over a redacted journal = %v, want ErrRedacted", err)
	}
	if err := audit.PersistJournal(ctx, audit.NewMemEventStore(), j, "gov"); !errors.Is(err, audit.ErrRedacted) {
		t.Errorf("PersistJournal over a redacted journal = %v, want ErrRedacted", err)
	}
	// A consistency proof from an earlier size still holds across the redaction.
	c, err := audit.ProveConsistency(ctx, j, "gov", 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := audit.VerifyConsistency(first, after.Root, c); err != nil {
		t.Errorf("a consistency proof across the redaction fails: %v", err)
	}
	// The export keeps the tombstone, and a journal read from it agrees.
	x, err := audit.ExportJournal(ctx, j, "gov")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := x.Journal(); err != nil || !got[payIdx].Redacted {
		t.Errorf("the export of a redacted journal reads back as %+v, %v", got[payIdx], err)
	}
}

func mustEntry(t *testing.T, name string, r agent.Record) []byte {
	t.Helper()
	b, err := agent.JournalEntry(name, r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rootOfPrefix is the journal root over the first n entries of run in s.
func rootOfPrefix(ctx context.Context, t *testing.T, s *agent.MemStore, run string, n int) []byte {
	t.Helper()
	dst := agent.NewMemStore()
	i := 0
	for e, err := range s.Load(ctx, run, -1) {
		if err != nil {
			t.Fatal(err)
		}
		if i == n {
			break
		}
		if _, _, err := dst.Insert(ctx, run, e.Name, e.Data); err != nil {
			t.Fatal(err)
		}
		i++
	}
	root, err := audit.Root(ctx, agenttest.MustJournal(dst), run)
	if err != nil {
		t.Fatal(err)
	}
	return root
}
