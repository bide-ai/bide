package verify_test

import (
	"crypto/ed25519"
	"testing"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/verify"
)

// SignTreeHead signs a head of any shape, but audit refuses one whose shape no commitment can
// have: a journal or event head naming a source journal, an absence head without one, or a kind
// it does not know. The standalone verifier must reach the same verdict on every signed head, so
// a third party checking with it is not told a head verifies that the SDK rejects.
func TestVerify_TreeHeadShapeMatchesAudit(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	root := make([]byte, 32)
	ref := &audit.TreeRef{Size: 3, Root: root}
	for name, th := range map[string]audit.TreeHead{
		"journal head":                   {Kind: audit.TreeJournal, RunID: "r", Size: 3, Root: root},
		"event head":                     {Kind: audit.TreeEvents, RunID: "r", Size: 3, Root: root},
		"absence head":                   {Kind: audit.TreeToolUse, RunID: "r", Size: 1, Root: root, Journal: ref},
		"journal head naming a journal":  {Kind: audit.TreeJournal, RunID: "r", Size: 3, Root: root, Journal: ref},
		"event head naming a journal":    {Kind: audit.TreeEvents, RunID: "r", Size: 3, Root: root, Journal: ref},
		"absence head naming no journal": {Kind: audit.TreeToolUse, RunID: "r", Size: 1, Root: root},
		"unknown kind":                   {Kind: "ledger", RunID: "r", Size: 3, Root: root},
		"unknown kind naming a journal":  {Kind: "ledger", RunID: "r", Size: 3, Root: root, Journal: ref},
		"bare absence prefix":            {Kind: "absence/", RunID: "r", Size: 1, Root: root, Journal: ref},
		"absence head, negative journal": {Kind: audit.TreeToolUse, RunID: "r", Size: 1, Root: root, Journal: &audit.TreeRef{Size: -1, Root: root}},
		"empty kind":                     {Kind: "", RunID: "r", Size: 3, Root: root},
	} {
		sth := audit.SignTreeHead(th, priv)
		var jref *verify.TreeRef
		if th.Journal != nil {
			jref = &verify.TreeRef{Size: th.Journal.Size, Root: th.Journal.Root}
		}
		want := sth.Verify(pub)
		if got := verify.TreeHead(th.Kind, th.RunID, th.Size, th.Root, th.Timestamp, jref, sth.Signature, pub); got != want {
			t.Errorf("%s: verify.TreeHead = %v, audit SignedTreeHead.Verify = %v", name, got, want)
		}
	}
}
