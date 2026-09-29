package audit_test

import (
	"context"
	"crypto/ed25519"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
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
	root := audit.AbsenceRoot(recs, audit.ToolUseKeys)

	// Absent in the middle (between d and f).
	proof, err := audit.ProveAbsent(recs, audit.ToolUseKeys, "tooluse:e")
	if err != nil {
		t.Fatalf("ProveAbsent: %v", err)
	}
	if ok, err := audit.VerifyAbsence(root, proof); err != nil || !ok {
		t.Fatalf("valid absence proof failed (ok=%v err=%v)", ok, err)
	}

	// A present key cannot be proven absent.
	if _, err := audit.ProveAbsent(recs, audit.ToolUseKeys, "tooluse:d"); err == nil {
		t.Fatal("ProveAbsent succeeded for a present key")
	}
}

// TestAbsence_Boundaries: keys before the first and after the last verify with a single
// neighbor; the empty key set proves everything absent.
func TestAbsence_Boundaries(t *testing.T) {
	recs := keyRecs("b", "d", "f")
	root := audit.AbsenceRoot(recs, audit.ToolUseKeys)

	before, _ := audit.ProveAbsent(recs, audit.ToolUseKeys, "tooluse:a") // sorts before all
	if before.Left != nil || before.Right == nil {
		t.Fatalf("before-first should have only a right neighbor: %+v", before)
	}
	if ok, _ := audit.VerifyAbsence(root, before); !ok {
		t.Fatal("before-first absence failed to verify")
	}

	after, _ := audit.ProveAbsent(recs, audit.ToolUseKeys, "tooluse:z") // sorts after all
	if after.Right != nil || after.Left == nil {
		t.Fatalf("after-last should have only a left neighbor: %+v", after)
	}
	if ok, _ := audit.VerifyAbsence(root, after); !ok {
		t.Fatal("after-last absence failed to verify")
	}

	// Empty key set: anything is absent.
	emptyRoot := audit.AbsenceRoot(nil, audit.ToolUseKeys)
	empty, _ := audit.ProveAbsent(nil, audit.ToolUseKeys, "tooluse:x")
	if ok, _ := audit.VerifyAbsence(emptyRoot, empty); !ok {
		t.Fatal("empty-set absence failed to verify")
	}
}

// TestAbsence_AdjacencyIsEnforced is the soundness test (the correction over merkle-strata): a
// forged proof that brackets a PRESENT key with two NON-adjacent real neighbors must be
// rejected, because the adjacency (consecutive-index) check fails.
func TestAbsence_AdjacencyIsEnforced(t *testing.T) {
	recs := keyRecs("b", "d", "f") // indices: b=0, d=1, f=2
	root := audit.AbsenceRoot(recs, audit.ToolUseKeys)

	// Genuinely prove b and f (indices 0 and 2), then forge an "absence of d" by pairing them.
	// d IS present at index 1, so a sound verifier must reject this.
	bProof, _ := audit.ProveAbsent(recs, audit.ToolUseKeys, "tooluse:a") // gives right=b@0
	fProofSrc, _ := audit.ProveAbsent(recs, audit.ToolUseKeys, "tooluse:z")
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
	ctx := context.Background()
	store := agent.NewMemStore()
	for _, r := range keyRecs("b", "d", "f") {
		r := r
		if _, err := store.Do(ctx, "run", r.ToolUseID, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
			t.Fatal(err)
		}
	}
	recs, _ := store.History(ctx, "run")
	journal, err := audit.NewTreeHead(ctx, store, "run", 1000)
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	sth, err := audit.SignAbsenceRoot(recs, audit.ToolUseKeys, journal, priv, 1000)
	if err != nil {
		t.Fatalf("SignAbsenceRoot: %v", err)
	}

	bundle, err := audit.ProveAbsentBundle(recs, audit.ToolUseKeys, "tooluse:e", sth)
	if err != nil {
		t.Fatalf("ProveAbsentBundle: %v", err)
	}
	if ok, err := bundle.Verify(pub, audit.ToolUseKeys); err != nil || !ok {
		t.Fatalf("absence bundle failed to verify (ok=%v err=%v)", ok, err)
	}
	if ok, _ := bundle.Verify(mustOtherKey(t), audit.ToolUseKeys); ok {
		t.Fatal("absence bundle verified under the wrong key")
	}
	if ok, _ := bundle.Verify(pub, audit.PolicyUsedKeys); ok {
		t.Fatal("a tool-use absence bundle verified as a used-policy absence")
	}

	// An STH over a different run's key set is rejected at proof time (fidelity guard).
	other := agent.NewMemStore()
	for _, r := range keyRecs("x", "y", "z") {
		r := r
		_, _ = other.Do(ctx, "run", r.ToolUseID, func(context.Context) (agent.Record, error) { return r, nil })
	}
	otherRecs, _ := other.History(ctx, "run")
	otherJournal, _ := audit.NewTreeHead(ctx, other, "run", 1000)
	otherSTH, err := audit.SignAbsenceRoot(otherRecs, audit.ToolUseKeys, otherJournal, priv, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audit.ProveAbsentBundle(recs, audit.ToolUseKeys, "tooluse:e", otherSTH); err == nil {
		t.Fatal("ProveAbsentBundle accepted an STH that does not commit to this run's key set")
	}
	// A key-set head must name the journal it came from: NewAbsenceTreeHead refuses a mismatched one.
	if _, err := audit.NewAbsenceTreeHead(recs, audit.ToolUseKeys, otherJournal, 1); err == nil {
		t.Fatal("NewAbsenceTreeHead accepted a journal head that does not match the records")
	}
}

func mustOtherKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(nil)
	return pub
}
