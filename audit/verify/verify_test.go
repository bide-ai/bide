package verify_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"testing"

	agent "github.com/blackwell-systems/bide"
	"github.com/blackwell-systems/bide/audit"
	"github.com/blackwell-systems/bide/audit/verify"
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
		if _, err := agent.Step(ctx, store, "run", name, func(context.Context) (string, error) { return v, nil }); err != nil {
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
		if _, err := agent.Step(ctx, twoStore, "run", vv, func(context.Context) (string, error) { return vv, nil }); err != nil {
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
	if !verify.TreeHead(sth.Root, sth.Size, sth.Timestamp, sth.Signature, pub) {
		t.Fatal("standalone verifier rejected a valid SDK-signed STH")
	}
	if verify.TreeHead(sth.Root, sth.Size+1, sth.Timestamp, sth.Signature, pub) {
		t.Fatal("standalone verifier accepted an STH with a tampered size")
	}
	if verify.TreeHead(sth.Root, sth.Size, sth.Timestamp+1, sth.Signature, pub) {
		t.Fatal("standalone verifier accepted an STH with a tampered timestamp")
	}
}
