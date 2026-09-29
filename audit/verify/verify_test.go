package verify_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/verify"
)

// The standalone verifier must agree, bit for bit, with the full audit package on the same
// proofs; that cross-check is what licenses the intentional duplication. It also must depend
// on nothing but the record's canonical leaf bytes (json.Marshal of the record), never the
// typed record, so a third party can verify without the SDK.

func journal(t *testing.T) (agent.Durable, string) {
	t.Helper()
	ctx := context.Background()
	store := agent.NewMemStore()
	for i, v := range []string{"a", "b", "c", "d", "e"} {
		name := v
		if _, err := agent.Step(ctx, store, "run", name, func(context.Context) (string, error) { return v, nil }, agent.StepSafety(agent.Safety{ReadOnly: true})); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	return store, "run"
}

func leafBytes(t *testing.T, rec agent.Record) []byte {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	return b
}

// TestVerify_InclusionMatchesAudit: every record's inclusion proof verifies via the standalone
// verifier on leaf bytes, and agrees with audit.VerifyInclusion.
func TestVerify_InclusionMatchesAudit(t *testing.T) {
	ctx := context.Background()
	store, runID := journal(t)
	root, _ := audit.Root(ctx, store, runID)
	recs, _ := store.History(ctx, runID)

	for i := range recs {
		proof, err := audit.Prove(ctx, store, runID, i)
		if err != nil {
			t.Fatalf("Prove(%d): %v", i, err)
		}
		viaAudit, _ := audit.VerifyInclusion(root, recs[i], proof)
		viaStandalone := verify.Inclusion(root, leafBytes(t, recs[i]), proof.Index, proof.Size, proof.Path)
		if !viaAudit || !viaStandalone {
			t.Fatalf("record %d: audit=%v standalone=%v (both must be true)", i, viaAudit, viaStandalone)
		}
	}

	// A wrong leaf must fail the standalone verifier.
	proof0, _ := audit.Prove(ctx, store, runID, 0)
	if verify.Inclusion(root, []byte(`{"forged":true}`), proof0.Index, proof0.Size, proof0.Path) {
		t.Fatal("standalone verifier accepted a forged leaf")
	}
}

// TestVerify_ConsistencyMatchesAudit: the standalone consistency check agrees with audit that
// the size-2 prefix is append-only-contained in the full tree.
func TestVerify_ConsistencyMatchesAudit(t *testing.T) {
	ctx := context.Background()
	store, runID := journal(t)
	rootFull, _ := audit.Root(ctx, store, runID)

	proof, err := audit.ProveConsistency(ctx, store, runID, 2)
	if err != nil {
		t.Fatalf("ProveConsistency: %v", err)
	}

	// The size-2 root, recomputed by replaying just the first two steps into a fresh store.
	twoStore := agent.NewMemStore()
	for _, v := range []string{"a", "b"} {
		vv := v
		if _, err := agent.Step(ctx, twoStore, "run", vv, func(context.Context) (string, error) { return vv, nil }, agent.StepSafety(agent.Safety{ReadOnly: true})); err != nil {
			t.Fatalf("two-step: %v", err)
		}
	}
	rootEarly, _ := audit.Root(ctx, twoStore, "run")

	viaAudit := audit.VerifyConsistency(rootEarly, rootFull, proof)
	viaStandalone := verify.Consistency(proof.First, proof.Size, proof.Path, rootEarly, rootFull)
	if !viaAudit || !viaStandalone {
		t.Fatalf("consistency: audit=%v standalone=%v (both must be true)", viaAudit, viaStandalone)
	}
}

// TestVerify_TreeHeadMatchesAudit: an STH signed by the SDK verifies via the standalone
// verifier, and any tamper to its fields breaks the signature.
func TestVerify_TreeHeadMatchesAudit(t *testing.T) {
	ctx := context.Background()
	store, runID := journal(t)
	pub, priv, _ := ed25519.GenerateKey(nil)
	th, _ := audit.NewTreeHead(ctx, store, runID, 1700000000)
	sth := audit.SignTreeHead(th, priv)

	if !sth.Verify(pub) {
		t.Fatal("audit STH did not self-verify")
	}
	if !verify.TreeHead(sth.Kind, sth.RunID, sth.Size, sth.Root, sth.Timestamp, nil, sth.Signature, pub) {
		t.Fatal("standalone verifier rejected a valid SDK-signed STH")
	}
	for name, ok := range map[string]bool{
		"size":      verify.TreeHead(sth.Kind, sth.RunID, sth.Size+1, sth.Root, sth.Timestamp, nil, sth.Signature, pub),
		"timestamp": verify.TreeHead(sth.Kind, sth.RunID, sth.Size, sth.Root, sth.Timestamp+1, nil, sth.Signature, pub),
		"kind":      verify.TreeHead(audit.TreePolicyUsed, sth.RunID, sth.Size, sth.Root, sth.Timestamp, nil, sth.Signature, pub),
		"run id":    verify.TreeHead(sth.Kind, "other", sth.Size, sth.Root, sth.Timestamp, nil, sth.Signature, pub),
		"journal":   verify.TreeHead(sth.Kind, sth.RunID, sth.Size, sth.Root, sth.Timestamp, &verify.TreeRef{}, sth.Signature, pub),
	} {
		if ok {
			t.Fatalf("standalone verifier accepted an STH with a tampered %s", name)
		}
	}

	// An absence key-set head, which names its source journal, verifies the same way.
	recs, _ := store.History(ctx, runID)
	abs, err := audit.SignAbsenceRoot(recs, audit.ToolUseKeys, th, priv, 7)
	if err != nil {
		t.Fatal(err)
	}
	ref := &verify.TreeRef{Size: abs.Journal.Size, Root: abs.Journal.Root}
	if !verify.TreeHead(abs.Kind, abs.RunID, abs.Size, abs.Root, abs.Timestamp, ref, abs.Signature, pub) {
		t.Fatal("standalone verifier rejected a valid SDK-signed absence head")
	}
	if verify.TreeHead(abs.Kind, abs.RunID, abs.Size, abs.Root, abs.Timestamp, &verify.TreeRef{Size: ref.Size + 1, Root: ref.Root}, abs.Signature, pub) {
		t.Fatal("standalone verifier accepted an absence head with a tampered journal size")
	}
}
