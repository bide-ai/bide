package audit

import (
	"context"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// ctLeaves / ctRoots are the canonical RFC 6962 (Certificate Transparency) reference test
// tree: 8 leaves and the published Merkle Tree Hash at each size 0..8. Matching these
// proves our MTH is the real RFC 6962 construction, not a homegrown look-alike.
var ctLeaves = [][]byte{
	{},
	{0x00},
	{0x10},
	{0x20, 0x21},
	{0x30, 0x31},
	{0x40, 0x41, 0x42, 0x43},
	{0x50, 0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57},
	{0x60, 0x61, 0x62, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68, 0x69, 0x6a, 0x6b, 0x6c, 0x6d, 0x6e, 0x6f},
}

var ctRoots = []string{
	"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", // 0 (empty = SHA-256(""))
	"6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d", // 1
	"fac54203e7cc696cf0dfcb42c92a1d9dbaf70ad9e621f4bd8d98662f00e3c125", // 2
	"aeb6bcfe274b70a14fb067a5e5578264db0fa9b51af5e0ba159158f329e06e77", // 3
	"d37ee418976dd95753c1c73862b9398fa2a2cf9b4ff0fdfe8b30cd95209614b7", // 4
	"4e3bbb1f7b478dcfe71fb631631519a3bca12c9aefca1612bfce4c13a86264d4", // 5
	"76e67dadbcdf1e10e1b74ddc608abd2f98dfb16fbce75277b5232a127f2087ef", // 6
	"ddb89be403809e325750d3d263cd78929c2942b7942a34b77e122c9594a74c8c", // 7
	"5dc9da79a70659a9ad559cb701ded9a2ab9d823aad2f4960cfe370eff4604328", // 8
}

func TestMerkle_MatchesRFC6962Vectors(t *testing.T) {
	for size := 0; size <= 8; size++ {
		got := hex.EncodeToString(merkleRoot(ctLeaves[:size]))
		if got != ctRoots[size] {
			t.Errorf("size %d: root = %s\n            want %s (RFC 6962)", size, got, ctRoots[size])
		}
	}
}

// Every leaf's inclusion proof verifies against the root, and tampering fails — for trees
// of many sizes (exercising the ragged-tree recursion, not just powers of two).
func TestMerkle_InclusionRoundTrip(t *testing.T) {
	for size := 1; size <= 33; size++ {
		leaves := make([][]byte, size)
		for i := range leaves {
			leaves[i] = []byte(fmt.Sprintf("record-%d", i))
		}
		root := merkleRoot(leaves)

		for m := 0; m < size; m++ {
			path := auditPath(m, leaves)
			if !verifyPath(root, leaves[m], m, size, path) {
				t.Fatalf("size=%d leaf=%d: valid proof rejected", size, m)
			}
			if verifyPath(root, []byte("forged"), m, size, path) {
				t.Fatalf("size=%d leaf=%d: proof accepted a forged leaf", size, m)
			}
		}
		if size >= 2 { // a proof must not verify against a different tree's root
			if verifyPath(merkleRoot(leaves[:size-1]), leaves[0], 0, size, auditPath(0, leaves)) {
				t.Fatalf("size=%d: proof verified against the wrong root", size)
			}
		}
	}
}

// Selective disclosure over a real journal: prove one record is in the committed run using
// only that record + its proof + the root — no other records revealed.
func TestMerkle_JournalSelectiveDisclosure(t *testing.T) {
	store := agenttest.MemJournal()
	for i, v := range []string{"open-case", "charge-500", "email-receipt", "close-case"} {
		if _, err := store.Step(context.Background(), "run", fmt.Sprintf("s%d", i),
			func(context.Context) (string, error) { return v, nil }, agent.WithSafety(agent.Safety{ReadOnly: true})); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	root, err := Root(ctx, store, "run")
	if err != nil {
		t.Fatal(err)
	}

	recs, _ := store.History(ctx, "run")
	// Disclose only record 1 (the "charge-500" step) + its proof.
	proof, err := Prove(ctx, store, "run", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyInclusion(root, recs[1].Raw(), proof); err != nil {
		t.Fatalf("inclusion of record 1 failed: err=%v", err)
	}
	// The same proof must NOT verify a different record (can't swap what happened).
	if err := VerifyInclusion(root, recs[3].Raw(), proof); err == nil {
		t.Fatal("proof for record 1 verified a different record")
	}
	// Out-of-range prove is an error, not a panic.
	if _, err := Prove(ctx, store, "run", 99); err == nil {
		t.Fatal("expected out-of-range error")
	}
}
