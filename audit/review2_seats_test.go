package audit_test

import (
	"encoding/hex"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// One person holding a hybrid key and also its Ed25519 or ML-DSA half under another approver id must not
// fill two seats, in an approval policy (m-of-n) and in a stop policy (lift quorum).
func TestReview2_HybridAndItsHalfAreOneSeat(t *testing.T) {
	sg := p11Signers(t)
	hy := sg["hybrid"].(audit.HybridSigner)
	halves := map[string]audit.Signer{"ed25519 half": hy.Ed, "ml-dsa half": hy.ML}
	for name, half := range halves {
		t.Run("approval/"+name, func(t *testing.T) {
			vh, err := audit.VerifierOf(hy)
			if err != nil {
				t.Fatal(err)
			}
			vp, err := audit.VerifierOf(half)
			if err != nil {
				t.Fatal(err)
			}
			byID := map[string]agent.ApproverVerifier{"alice": vh, "alice2": vp}
			pol := agent.ApprovalPolicy{Need: 2, Approvers: []string{"alice", "alice2"}}
			err = pol.ValidateKeys(func(id string) (agent.ApproverVerifier, bool) { v, ok := byID[id]; return v, ok })
			if err == nil {
				t.Fatalf("a hybrid key and its %s fill two seats of a 2-of-2 approval", name)
			}
		})
		t.Run("stop/"+name, func(t *testing.T) {
			doc := agent.StopPolicyDoc{DeploymentID: "d", Setters: []string{"alice"}, Lifters: []string{"alice", "alice2"}, LiftQuorum: 2,
				Approvers: map[string]agent.StopApprover{
					"alice":  {Alg: hy.Alg(), Key: hex.EncodeToString(hy.PublicKey())},
					"alice2": {Alg: half.Alg(), Key: hex.EncodeToString(half.PublicKey())},
				}}
			if _, err := audit.StopPolicyFromDoc(doc); err == nil {
				t.Fatalf("a hybrid key and its %s count as two lifters of a 2-quorum", name)
			}
		})
	}
}
