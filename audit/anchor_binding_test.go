package audit

import (
	"crypto/ed25519"
	"errors"
	"testing"
)

// An anchor entry says which position it holds (Seq) and which run it anchors (RunID), and a
// monitor reads both. The anchor log is another party's service, so a proof of an entry must not
// verify when the entry misstates either: its Seq must be the index the proof proves, and its
// RunID the run its signed head names (MemAnchorLog.Publish writes nothing else).
func TestVerifyAnchorInclusion_BindsSeqAndRun(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	head := func(run string) SignedTreeHead {
		return signTH(t, TreeHead{Kind: TreeJournal, RunID: run, Size: 1, Root: make([]byte, 32)}, priv)
	}
	for name, entries := range map[string][]AnchorEntry{
		"seq of another position": {{Format: AnchorEntryFormat, Seq: 0, RunID: "a", STH: head("a")}, {Format: AnchorEntryFormat, Seq: 5, RunID: "b", STH: head("b")}},
		"run not the head's run":  {{Format: AnchorEntryFormat, Seq: 0, RunID: "a", STH: head("a")}, {Format: AnchorEntryFormat, Seq: 1, RunID: "b", STH: head("a")}},
	} {
		leaves := make([][]byte, len(entries))
		for i, e := range entries {
			b, err := canonicalAnchorEntry(e)
			if err != nil {
				t.Fatal(err)
			}
			leaves[i] = b
		}
		root := merkleRoot(leaves)
		for i := range entries {
			p := Inclusion{Index: i, Size: len(leaves), Path: auditPath(i, leaves)}
			err := VerifyAnchorInclusion(root, entries[i], p)
			bad := entries[i].Seq != i || entries[i].RunID != entries[i].STH.RunID
			if bad && err == nil {
				t.Errorf("%s: entry %d (seq %d, run %q, head run %q) verified at index %d", name, i, entries[i].Seq, entries[i].RunID, entries[i].STH.RunID, i)
			}
			if bad && err != nil && !errors.Is(err, ErrNotVerified) {
				t.Errorf("%s: entry %d: err = %v, want ErrNotVerified", name, i, err)
			}
			if !bad && err != nil {
				t.Errorf("%s: well-formed entry %d did not verify: %v", name, i, err)
			}
		}
	}
}
