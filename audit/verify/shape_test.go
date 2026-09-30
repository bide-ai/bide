package verify_test

import (
	"crypto/ed25519"
	"testing"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/verify"
)

// audit refuses a head whose shape no commitment can have: a journal or event head naming a source
// journal, an absence head without one, or a kind it does not know (SignTreeHead will not sign one,
// and SignedTreeHead.Verify rejects one whatever its signature). The standalone verifier must reach
// the same verdict on every signed head, so a third party checking with it is not told a head
// verifies that the SDK rejects. The ill-shaped heads are signed here over the documented encoding.
func TestVerify_TreeHeadShapeMatchesAudit(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	root := make([]byte, 32)
	ref := &audit.TreeRef{Size: 3, Root: root}
	av := audit.Ed25519Verifier{Pub: pub}
	sv, _ := verify.NewVerifier("ed25519", pub)
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
		sth := audit.SignedTreeHead{Format: audit.STHFormat, TreeHead: th, Alg: audit.AlgEd25519, Signature: ed25519.Sign(priv, canonicalV5("ed25519", th))}
		_, signErr := audit.SignTreeHead(th, audit.Ed25519Signer{Priv: priv})
		want := sth.Verify(av) == nil
		if (signErr == nil) != want {
			t.Errorf("%s: SignTreeHead err = %v, but Verify of the signed head = %v", name, signErr, want)
		}
		if got := verify.TreeHead(head(sth), sth.Signature, sv); got != want {
			t.Errorf("%s: verify.TreeHead = %v, audit SignedTreeHead.Verify = %v", name, got, want)
		}
	}
}
