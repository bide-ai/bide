package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// keylessVerifier is an ApproverVerifier that reports no key identity (a verifier that checks
// through a service and does not expose its keys). Each value verifies only signatures made with
// its own secret, so two values are two different keys.
type keylessVerifier struct{ secret string }

func (keylessVerifier) Alg() agent.Alg   { return "svc" }
func (keylessVerifier) KeyIDs() []string { return nil }
func (v keylessVerifier) Verify(m, sig []byte) bool {
	return bytes.Equal(sig, append([]byte(v.secret+"|"), m...))
}

// L3 follow-up (re-review of #105). #105's counting rule keyed "one key counts once" on
// Alg()+PublicKey(), so two approvers with different keys whose verifiers exposed no key collided,
// while audit.VerifyApprovals took them as distinct: the gate and the audit disagreed on the same
// resolver. Under the one seat-per-key rule (#109, KeyIDs) a verifier that reports no key identity
// names no key, and both layers refuse it: the counting rule never counts such an approver
// (ReasonNoKeyID), and the gate refuses the policy with ErrConfig before the tool can run.
func Test_R105b_KeylessApproverIsRefused(t *testing.T) {
	s := agent.ApprovalSubject{RunID: "r", ToolUseID: "c", ToolName: "wire", Args: json.RawMessage(`{}`)}
	keys := map[string]keylessVerifier{"alice": {"a-secret"}, "bob": {"b-secret"}}
	resolve := func(id string) (agent.ApproverVerifier, bool) {
		v, ok := keys[id]
		return v, ok
	}
	var recs []agent.Record
	for _, id := range []string{"alice", "bob"} {
		sig := append([]byte(keys[id].secret+"|"), agent.ApprovalDecisionBytes(s, id, true)...)
		recs = append(recs, agent.Record{Name: "approval:c:" + id, Kind: agent.StepApproval, ToolUseID: "c", Approved: true,
			Approver: id, ApproverAlg: "svc", Signature: sig})
	}
	p := agent.ApprovalPolicy{Need: 1, Approvers: []string{"alice", "bob"}}
	got, checks := agent.TallyApprovals(recs, s, p, resolve)
	if got.Approved != 0 {
		t.Fatalf("the counting rule counted %d approvals from verifiers with no key identity, want 0 (checks %+v)", got.Approved, checks)
	}
	for _, c := range checks {
		if c.Counted || c.Reason != agent.ReasonNoKeyID {
			t.Fatalf("decision %s: counted %v, reason %q; want not counted, %q", c.Step, c.Counted, c.Reason, agent.ReasonNoKeyID)
		}
	}

	// The gate refuses the policy before any decision is read, and the tool never runs.
	ran := 0
	wire := agent.Func("wire", "", agent.Safety{Approval: &p}, func(context.Context, struct{}) (string, error) { ran++; return "sent", nil })
	_, err := agent.New(agent.NewScriptedModel(agent.ToolTurn("c", "wire", `{}`), agent.TextTurn("done")), agent.NewMemStore(), wire).
		WithApproverVerifiers(resolve).Run(context.Background(), "r", "hi")
	if !errors.Is(err, agent.ErrConfig) || ran != 0 {
		t.Fatalf("a gate whose approvers resolve to verifiers with no key identity: err %v, tool ran %d times; want ErrConfig and no run", err, ran)
	}
}
