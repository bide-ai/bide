package audit_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"math/big"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// ECDSA signatures are malleable: (r, n-s) verifies whenever (r, s) does. The scheme refuses the high-S
// twin everywhere a decision is counted, so a hostile endpoint, journal writer or relay cannot turn a
// recorded decision into a second valid one with other bytes.
func TestP256DecisionHighSTwinIsNeverCounted(t *testing.T) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sg := audit.ECDSAP256Signer{Priv: k}
	v, err := audit.VerifierOf(sg)
	if err != nil {
		t.Fatal(err)
	}
	s := agent.ApprovalSubject{RunID: "r", ToolUseID: "c1", ToolName: "charge", Args: []byte(`{"amount":1}`)}
	msg := agent.ApprovalDecisionBytes(s, "ops", true, testDecidedAt)
	sig, err := sg.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	n := elliptic.P256().Params().N
	twin := append([]byte(nil), sig...)
	sv := new(big.Int).SetBytes(sig[32:])
	new(big.Int).Sub(n, sv).FillBytes(twin[32:])
	if v.Verify(msg, twin) {
		t.Fatal("the verifier accepts the high-S twin of a decision signature")
	}
	rec := func(sig []byte) agent.Record {
		return agent.Record{Name: "d", Kind: agent.StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &agent.ApproverSignature{
			Approver: "ops", ApproverAlg: audit.AlgECDSAP256, Signature: sig, DecidedAt: agent.FormatTimestamp(testDecidedAt)}}
	}
	pol := agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}
	vf := func(id string) (agent.ApproverVerifier, bool) { return v, id == "ops" }
	if tally, _ := agent.TallyApprovals([]agent.Record{rec(sig)}, s, pol, vf); tally.Approved != 1 {
		t.Fatalf("the genuine decision does not count: %+v", tally)
	}
	tally, checks := agent.TallyApprovals([]agent.Record{rec(twin)}, s, pol, vf)
	if tally.Approved != 0 || len(checks) != 1 || checks[0].Reason != agent.ReasonBadSig {
		t.Fatalf("the high-S twin counts: %+v %+v", tally, checks)
	}
	// Every signature a P-256 signer emits is low-S, over many signs.
	half := new(big.Int).Rsh(n, 1)
	for i := 0; i < 200; i++ {
		g, err := sg.Sign([]byte{byte(i)})
		if err != nil || new(big.Int).SetBytes(g[32:]).Cmp(half) > 0 {
			t.Fatalf("sign %d: %v (high S)", i, err)
		}
	}
}
