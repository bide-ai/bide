package audit_test

import (
	"crypto/ed25519"
	"testing"

	agent "github.com/dayna/go-agents"
	"github.com/dayna/go-agents/audit"
)

// keyRecs builds a set of records whose ToolUseKeys are exactly the given tool-use IDs.
func keyRecs(ids ...string) []agent.Record {
	recs := make([]agent.Record, len(ids))
	for i, id := range ids {
		recs[i] = agent.Record{Kind: agent.StepToolResult, ToolUseID: id, Result: []byte(`"ok"`)}
	}
	return recs
}

// TestAbsence_ProveAndVerify: an absent key proves against the root; a present key cannot.
func TestAbsence_ProveAndVerify(t *testing.T) {
	recs := keyRecs("b", "d", "f") // keys: tooluse:b, tooluse:d, tooluse:f
	root := audit.AbsenceRoot(recs, audit.ToolUseKey)

	// Absent in the middle (between d and f).
	proof, err := audit.ProveAbsent(recs, audit.ToolUseKey, "tooluse:e")
	if err != nil {
		t.Fatalf("ProveAbsent: %v", err)
	}
	if ok, err := audit.VerifyAbsence(root, proof); err != nil || !ok {
		t.Fatalf("honest absence proof failed (ok=%v err=%v)", ok, err)
	}

	// A present key cannot be proven absent.
	if _, err := audit.ProveAbsent(recs, audit.ToolUseKey, "tooluse:d"); err == nil {
		t.Fatal("ProveAbsent succeeded for a present key")
	}
}

// TestAbsence_Boundaries: keys before the first and after the last verify with a single
// neighbor; the empty key set proves everything absent.
func TestAbsence_Boundaries(t *testing.T) {
	recs := keyRecs("b", "d", "f")
	root := audit.AbsenceRoot(recs, audit.ToolUseKey)

	before, _ := audit.ProveAbsent(recs, audit.ToolUseKey, "tooluse:a") // sorts before all
	if before.Left != nil || before.Right == nil {
		t.Fatalf("before-first should have only a right neighbor: %+v", before)
	}
	if ok, _ := audit.VerifyAbsence(root, before); !ok {
		t.Fatal("before-first absence failed to verify")
	}

	after, _ := audit.ProveAbsent(recs, audit.ToolUseKey, "tooluse:z") // sorts after all
	if after.Right != nil || after.Left == nil {
		t.Fatalf("after-last should have only a left neighbor: %+v", after)
	}
	if ok, _ := audit.VerifyAbsence(root, after); !ok {
		t.Fatal("after-last absence failed to verify")
	}

	// Empty key set: anything is absent.
	emptyRoot := audit.AbsenceRoot(nil, audit.ToolUseKey)
	empty, _ := audit.ProveAbsent(nil, audit.ToolUseKey, "tooluse:x")
	if ok, _ := audit.VerifyAbsence(emptyRoot, empty); !ok {
		t.Fatal("empty-set absence failed to verify")
	}
}

// TestAbsence_AdjacencyIsEnforced is the soundness test (the correction over merkle-strata): a
// forged proof that brackets a PRESENT key with two NON-adjacent real neighbors must be
// rejected, because the adjacency (consecutive-index) check fails.
func TestAbsence_AdjacencyIsEnforced(t *testing.T) {
	recs := keyRecs("b", "d", "f") // indices: b=0, d=1, f=2
	root := audit.AbsenceRoot(recs, audit.ToolUseKey)

	// Honestly prove b and f (indices 0 and 2), then forge an "absence of d" by pairing them.
	// d IS present at index 1, so a sound verifier must reject this.
	bProof, _ := audit.ProveAbsent(recs, audit.ToolUseKey, "tooluse:a") // gives right=b@0
	fProofSrc, _ := audit.ProveAbsent(recs, audit.ToolUseKey, "tooluse:z")
	forged := audit.Absence{
		Key:   "tooluse:d",
		Size:  3,
		Left:  bProof.Right,   // tooluse:b at index 0 (b < d)
		Right: fProofSrc.Left, // tooluse:f at index 2 (d < f)
	}
	// Both neighbors are genuinely included and bracket d, but indices 0 and 2 are NOT adjacent.
	if ok, _ := audit.VerifyAbsence(root, forged); ok {
		t.Fatal("verifier accepted a non-adjacent bracket around a PRESENT key (adjacency check missing)")
	}
}

// TestAbsence_Bundle: the anchored, portable form verifies under the signer's key and binds to
// the committed key set (a wrong-run STH is rejected at proof time).
func TestAbsence_Bundle(t *testing.T) {
	recs := keyRecs("b", "d", "f")
	pub, priv, _ := ed25519.GenerateKey(nil)
	th := audit.TreeHead{Size: 3, Root: audit.AbsenceRoot(recs, audit.ToolUseKey), Timestamp: 1000}
	sth := audit.SignTreeHead(th, priv)

	bundle, err := audit.ProveAbsentBundle(recs, audit.ToolUseKey, "tooluse:e", "run", sth)
	if err != nil {
		t.Fatalf("ProveAbsentBundle: %v", err)
	}
	if ok, err := bundle.Verify(pub); err != nil || !ok {
		t.Fatalf("absence bundle failed to verify (ok=%v err=%v)", ok, err)
	}
	if ok, _ := bundle.Verify(mustOtherKey(t)); ok {
		t.Fatal("absence bundle verified under the wrong key")
	}

	// An STH over a different key set is rejected at proof time (fidelity guard).
	otherTH := audit.TreeHead{Size: 3, Root: audit.AbsenceRoot(keyRecs("x", "y", "z"), audit.ToolUseKey), Timestamp: 1000}
	otherSTH := audit.SignTreeHead(otherTH, priv)
	if _, err := audit.ProveAbsentBundle(recs, audit.ToolUseKey, "tooluse:e", "run", otherSTH); err == nil {
		t.Fatal("ProveAbsentBundle accepted an STH that does not commit to this run's key set")
	}
}

func mustOtherKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(nil)
	return pub
}
