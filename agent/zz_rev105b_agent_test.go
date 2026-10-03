package agent_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
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
		recs = append(recs, agent.Record{Name: "approval:c:" + id, Kind: agent.StepApproval, ToolUseID: "c", Approved: true, ApproverSignature: &agent.ApproverSignature{Approver: id, ApproverAlg: "svc", Signature: sig}})
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
	wire := agent.MustFunc("wire", "", func(context.Context, struct{}) (string, error) { ran++; return "sent", nil }, agent.WithApproval(&p))
	_, err := agenttest.MustNew(
		agenttest.NewScriptedModel(agenttest.ToolTurn("c", "wire", `{}`), agenttest.TextTurn("done")),
		agenttest.MemJournal(),
		agent.WithTools(wire), agent.WithApproverVerifiers(resolve)).Run(context.Background(), "r", agent.UserText("hi"))
	if !errors.Is(err, agent.ErrConfig) || ran != 0 {
		t.Fatalf("a gate whose approvers resolve to verifiers with no key identity: err %v, tool ran %d times; want ErrConfig and no run", err, ran)
	}
}

// keyedVerifier is an ApproverVerifier with a key of its own, verifying signatures made with it.
type keyedVerifier struct{ key string }

func (keyedVerifier) Alg() agent.Alg     { return "svc" }
func (v keyedVerifier) KeyIDs() []string { return []string{"svc:" + v.key} }
func (v keyedVerifier) Verify(m, sig []byte) bool {
	return bytes.Equal(sig, append([]byte(v.key+"|"), m...))
}

// Round-3 review of #105. The gate reuses a journaled tally once it exists, and read it with
// json.Unmarshal while audit.VerifyApprovals reads it strictly (UnmarshalStrict). A stored tally
// with "approved":0 and then a case variant "Approved":2 read as a passed 2-of-2 gate to the gate,
// which ran the tool, while the audit refused the same record as malformed. The gate must read it
// as the audit does: refuse it, and never run the tool.
func Test_R105c_GateReadsTheRecordedTallyStrictly(t *testing.T) {
	ctx := context.Background()
	p := agent.ApprovalPolicy{Need: 2, Approvers: []string{"alice", "bob"}}
	ran := 0
	wire := agent.MustFunc("wire", "", func(context.Context, struct{}) (string, error) { ran++; return "sent", nil }, agent.WithApproval(&p))
	resolve := func(id string) (agent.ApproverVerifier, bool) { return keyedVerifier{id + "-key"}, true }
	s := agenttest.MemJournal()
	newAgent := func() *agent.Agent {
		return agenttest.MustNew(
			agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "wire", `{}`), agenttest.TextTurn("done")),
			s,
			agent.WithTools(wire), agent.WithApproverVerifiers(resolve))
	}
	var pend *agent.ApprovalPending
	if _, err := newAgent().Run(ctx, "r", agent.UserText("hi")); !errors.As(err, &pend) {
		t.Fatalf("setup: first run err %v, want a pending approval", err)
	}
	salt := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, agent.SaltSize))
	tally := `{"name":"` + agent.ApprovalTallyStep("c1") + `","kind":"value","result":{"need":2,"approvers":["alice","bob"],"approved":0,"denied":0,"Approved":2},"salt":"` + salt + `"}`
	if _, _, err := s.Store().Insert(ctx, "r", agent.ApprovalTallyStep("c1"), []byte(tally)); err != nil {
		t.Fatal(err)
	}
	_, err := newAgent().Run(ctx, "r", agent.UserText("hi"))
	if err == nil || ran != 0 {
		t.Fatalf("a recorded tally that reads approved 0 to an exact-name reader and 2 to encoding/json: run err %v, tool ran %d times; want an error and no run", err, ran)
	}
}
