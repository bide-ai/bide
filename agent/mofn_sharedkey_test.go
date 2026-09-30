package agent

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
)

// edVerifier is an ed25519 approver verifier local to this package's tests (agent cannot import
// audit, whose Ed25519Verifier is the production one).
type edVerifier struct{ pub ed25519.PublicKey }

func (v edVerifier) Verify(message, sig []byte) bool {
	return len(v.pub) == ed25519.PublicKeySize && ed25519.Verify(v.pub, message, sig)
}

// sharedKey is a deterministic ed25519 key pair: the key one person holds.
func sharedKey() (ed25519.PublicKey, ed25519.PrivateKey) {
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	return priv.Public().(ed25519.PublicKey), priv
}

// signAs records a decision for approverID signed with priv, over the recorded call.
func signAs(t *testing.T, store Durable, runID, toolUseID, approverID string, priv ed25519.PrivateKey) {
	t.Helper()
	sig := ed25519.Sign(priv, ApprovalDecisionBytes(subjectOf(t, store, runID, toolUseID), approverID, true))
	if err := SubmitDecision(context.Background(), store, Decision{RunID: runID, ToolUseID: toolUseID, ApproverID: approverID, Approved: true, Signature: sig}); err != nil {
		t.Fatalf("SubmitDecision(%s): %v", approverID, err)
	}
}

// F5 (spec/tla, findings/ap-shared-key): two approvers whose verifiers resolve to one key are
// two seats for one person. The holder of that key signs as both and meets a 2-of-2 quorum
// alone. The gate must refuse such a policy with ErrConfig and never run the tool.
func TestMofn_SharedKeyIsOneSeat(t *testing.T) {
	store := NewMemStore()
	pub, priv := sharedKey()
	pol := &ApprovalPolicy{Need: 2, Approvers: []string{"a1", "a2"}}
	vf := func(id string) (ApproverVerifier, bool) {
		if id == "a1" || id == "a2" {
			return edVerifier{pub: pub}, true
		}
		return nil, false
	}
	var charged int

	_, _ = mofnRun(store, "r1", true, pol, vf, &charged)
	signAs(t, store, "r1", "c1", "a1", priv)
	signAs(t, store, "r1", "c1", "a2", priv)
	_, err := mofnRun(store, "r1", false, pol, vf, &charged)
	if charged != 0 {
		t.Fatalf("charge ran %d times on one person's approval of a 2-of-2 quorum, want 0", charged)
	}
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want ErrConfig for a policy whose approvers share a key", err)
	}
}
