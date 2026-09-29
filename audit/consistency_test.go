package audit

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/bide-ai/bide/agent"
)

func leavesN(n int) [][]byte {
	ls := make([][]byte, n)
	for i := range ls {
		ls[i] = []byte(fmt.Sprintf("record-%d", i))
	}
	return ls
}

// For every (m, n), a generated consistency proof verifies against the RFC-correct roots
// of the m-prefix and the n-tree (merkleRoot is already spec-verified), and a wrong first
// root is rejected. This anchors consistency to the spec via the verified MTH.
func TestConsistency_RoundTripAgainstRFCRoots(t *testing.T) {
	for n := 1; n <= 24; n++ {
		leaves := leavesN(n)
		rootN := merkleRoot(leaves)
		for m := 0; m <= n; m++ {
			rootM := merkleRoot(leaves[:m])
			proof := consistencyProof(m, leaves)
			if !verifyConsistency(m, n, proof, rootM, rootN) {
				t.Fatalf("n=%d m=%d: valid consistency proof rejected", n, m)
			}
			if m > 0 && m < n && verifyConsistency(m, n, proof, rootN, rootN) {
				t.Fatalf("n=%d m=%d: accepted a wrong first root", n, m)
			}
		}
	}
}

// Hand-derived vector: PROOF(1, {d0,d1}) = SUBPROOF(1,{d0,d1},true) = {} ++ MTH({d1}) =
// [leafHash(d1)]. And it verifies against root1=leafHash(d0), root2=node(leaf(d0),leaf(d1)).
func TestConsistency_HandDerived1to2(t *testing.T) {
	d0, d1 := []byte("a"), []byte("b")
	leaves := [][]byte{d0, d1}
	proof := consistencyProof(1, leaves)
	if len(proof) != 1 || !bytes.Equal(proof[0], leafHash(d1)) {
		t.Fatalf("proof = %x, want [leafHash(d1)]", proof)
	}
	root1 := leafHash(d0)
	root2 := nodeHash(leafHash(d0), leafHash(d1))
	if !verifyConsistency(1, 2, proof, root1, root2) {
		t.Fatal("hand-derived 1->2 consistency proof failed to verify")
	}
}

// A rewrite is detectable: if an early record is altered, no consistency proof makes the
// original prefix root consistent with the tampered tree.
func TestConsistency_DetectsRewrite(t *testing.T) {
	orig := leavesN(10)
	rootM := merkleRoot(orig[:4]) // committed earlier

	tampered := leavesN(10)
	tampered[1] = []byte("REWRITTEN") // change a record inside the first 4
	rootNTampered := merkleRoot(tampered)
	proof := consistencyProof(4, tampered) // even a proof built from the tampered tree...

	if verifyConsistency(4, 10, proof, rootM, rootNTampered) {
		t.Fatal("consistency verified despite a rewritten early record")
	}
}

// End-to-end over a journal: commit an early root, append more steps, prove the earlier
// prefix is an append-only prefix of the grown journal.
func TestConsistency_JournalAppendOnly(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	add := func(name, v string) {
		if _, err := agent.Step(ctx, store, "run", name, func(context.Context) (string, error) { return v, nil }, agent.StepSafety(agent.Safety{ReadOnly: true})); err != nil {
			t.Fatal(err)
		}
	}
	add("s0", "a")
	add("s1", "b")
	rootEarly, _ := Root(ctx, store, "run") // commitment at size 2
	add("s2", "c")
	add("s3", "d")
	rootNow, _ := Root(ctx, store, "run") // size 4

	proof, err := ProveConsistency(ctx, store, "run", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyConsistency(rootEarly, rootNow, proof) {
		t.Fatal("append-only growth failed the consistency check")
	}
	if VerifyConsistency(rootNow, rootEarly, proof) {
		t.Fatal("consistency must be directional (earlier ⊑ later)")
	}
}
