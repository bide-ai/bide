package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// A flow node's single-approval gate passes only on a decision recorded after its request: Approve
// refuses one before the gate paused, then a decision after it passes the gate (and a denial
// denies it), and the recorded outcome is final on every later evaluation.
func TestFlowGate_SingleDecisionAfterRequest(t *testing.T) {
	ctx := context.Background()
	for _, approved := range []bool{true, false} {
		j := memJournal()
		args := json.RawMessage(`{"amount":1}`)
		if err := Approve(ctx, j, "r1", "node:pay", approved); !errors.Is(err, ErrConfig) {
			t.Fatalf("Approve before the request = %v; want ErrConfig", err)
		}
		if r, found, err := j.Get(ctx, "r1", approvalStep("node:pay")); err != nil || found {
			t.Fatalf("a refused Approve recorded a decision: %+v, %v", r, err)
		}
		var pend *ApprovalPending
		if _, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, SingleApproval(), nil); !errors.As(err, &pend) {
			t.Fatalf("first evaluation = %v; want a pause", err)
		}
		if _, ok, _ := j.Get(ctx, "r1", ApprovalPendingStep("node:pay")); !ok {
			t.Error("the pause recorded no ApprovalWait note")
		}
		if err := Approve(ctx, j, "r1", "node:pay", approved); err != nil {
			t.Fatalf("Approve(%v) after the request = %v", approved, err)
		}
		for i := 0; i < 2; i++ {
			got, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, SingleApproval(), nil)
			if err != nil || got != approved {
				t.Fatalf("evaluation %d after Approve(%v) = %v, %v", i, approved, got, err)
			}
		}
	}
}

// A single decision recorded under the gate's name before its request cannot count: the gate stays
// paused, and Approve reports that the gate cannot pass (ErrConfig).
func TestFlowGate_DecisionBeforeRequestCannotCount(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	writeRaw(t, j, "r1", approvalStep("node:pay"), Record{Kind: StepApproval, ToolUseID: "node:pay", Approved: true})
	var pend *ApprovalPending
	if _, err := flowGate(ctx, j, "r1", "node:pay", "pay", json.RawMessage(`1`), SingleApproval(), nil); !errors.As(err, &pend) {
		t.Fatalf("gate with a pre-request decision = %v; want the pause", err)
	}
	if err := Approve(ctx, j, "r1", "node:pay", true); !errors.Is(err, ErrConfig) {
		t.Fatalf("Approve over a pre-request decision = %v; want ErrConfig", err)
	}
	if _, err := flowGate(ctx, j, "r1", "node:pay", "pay", json.RawMessage(`1`), SingleApproval(), nil); !errors.As(err, &pend) {
		t.Fatalf("gate after the refused Approve = %v; want it still paused", err)
	}
}

// An m-of-n flow gate counts signed decisions after its request, records a terminal tally, and the
// tally is final: the node passes on quorum, and a denial that makes quorum unreachable denies it.
func TestFlowGate_MofnQuorumAndDenial(t *testing.T) {
	ctx := context.Background()
	vf := fakeVerifiers("a", "b")
	pol := &ApprovalPolicy{Need: 2, Approvers: []string{"a", "b"}}
	args := json.RawMessage(`{"amount":7}`)
	subj := ApprovalSubject{RunID: "r1", ToolUseID: "node:pay", ToolName: "pay", Args: args}
	run := func(t *testing.T, votes map[string]bool) (*Journal, bool, error) {
		j := memJournal()
		var pend *ApprovalPending
		if _, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, pol, vf); !errors.As(err, &pend) || pend.Quorum == nil {
			t.Fatalf("first evaluation = %v; want a quorum pause", err)
		}
		for _, id := range []string{"a", "b"} {
			v, ok := votes[id]
			if !ok {
				continue
			}
			if err := decideAs(ctx, j, "r1", "node:pay", id, v, fakeSign(id, ApprovalDecisionBytes(subj, id, v, testDecidedAt))); err != nil {
				t.Fatalf("decision by %s: %v", id, err)
			}
		}
		ok, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, pol, vf)
		return j, ok, err
	}

	t.Run("one of two is still pending", func(t *testing.T) {
		_, _, err := run(t, map[string]bool{"a": true})
		var pend *ApprovalPending
		if !errors.As(err, &pend) || pend.Quorum.Approved != 1 {
			t.Fatalf("err = %v; want a pause with one approval counted", err)
		}
	})
	t.Run("quorum passes and the tally is final", func(t *testing.T) {
		j, ok, err := run(t, map[string]bool{"a": true, "b": true})
		if err != nil || !ok {
			t.Fatalf("gate = %v, %v; want passed", ok, err)
		}
		if r, found, err := j.Get(ctx, "r1", ApprovalTallyStep("node:pay")); err != nil || !found || r.Kind != StepValue {
			t.Fatalf("no terminal tally recorded: %+v, %v, %v", r, found, err)
		}
		if ok, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, pol, vf); err != nil || !ok {
			t.Fatalf("replay = %v, %v; want the recorded pass", ok, err)
		}
	})
	t.Run("a denial that makes quorum unreachable denies", func(t *testing.T) {
		j, ok, err := run(t, map[string]bool{"a": false})
		if err != nil || ok {
			t.Fatalf("gate = %v, %v; want denied", ok, err)
		}
		if ok, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, pol, vf); err != nil || ok {
			t.Fatalf("replay = %v, %v; want the recorded denial", ok, err)
		}
	})
}

// A store that cannot be read or written while a gate evaluates is a storage error (ErrStorage on
// a read), never a pass, a denial or a silent skip.
func TestFlowGate_StoreFaults(t *testing.T) {
	ctx := context.Background()
	args := json.RawMessage(`1`)
	vf := fakeVerifiers("a")
	mofn := &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	cases := []struct {
		name    string
		sub     string
		pol     *ApprovalPolicy
		wantSto bool
	}{
		{"single outcome read", "approval:", SingleApproval(), true},
		{"m-of-n tally read", "approval-tally:", mofn, true},
		{"request write", "approval-request:", SingleApproval(), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var st Store = NewMemStore()
			if c.name == "request write" {
				st = covInsertFail{st, c.sub}
			} else {
				st = covGetFail{st, c.sub}
			}
			j := mustJournal(st)
			_, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, c.pol, vf)
			if err == nil || errors.Is(err, ErrConfig) {
				t.Fatalf("flowGate = %v; want a store error", err)
			}
			var pend *ApprovalPending
			if errors.As(err, &pend) {
				t.Fatalf("flowGate paused on a store fault: %v", err)
			}
			if c.wantSto && !errors.Is(err, ErrStorage) {
				t.Fatalf("flowGate = %v; want ErrStorage", err)
			}
		})
	}
	t.Run("request read in Approve", func(t *testing.T) {
		j := mustJournal(covGetFail{NewMemStore(), "approval-request:"})
		if err := Approve(ctx, j, "r1", "node:pay", true); !errors.Is(err, ErrStorage) {
			t.Fatalf("Approve = %v; want ErrStorage", err)
		}
	})
	t.Run("start read in Approve", func(t *testing.T) {
		j := mustJournal(covGetFail{NewMemStore(), runStartStep})
		if err := Approve(ctx, j, "r1", "node:pay", true); err == nil {
			t.Fatal("Approve with an unreadable run:start succeeded")
		}
	})
}

// A request record that is not the gate's request (not strict JSON of the right shape, or naming
// another node) is ErrConfig: the gate does not evaluate decisions against it.
func TestFlowGate_RequestRecordMustBeTheGates(t *testing.T) {
	ctx := context.Background()
	for name, body := range map[string]string{
		"another node":    `{"tool_use_id":"node:other","tool_name":"pay","args":1}`,
		"unknown field":   `{"tool_use_id":"node:pay","tool_name":"pay","args":1,"extra":true}`,
		"empty object":    `{}`,
		"scalar not json": `7`,
	} {
		t.Run(name, func(t *testing.T) {
			j := memJournal()
			writeRaw(t, j, "r1", ApprovalRequestStep("node:pay"), Record{Kind: StepValue, Result: json.RawMessage(body)})
			_, err := flowGate(ctx, j, "r1", "node:pay", "pay", json.RawMessage(`1`), SingleApproval(), nil)
			if !errors.Is(err, ErrConfig) {
				t.Fatalf("flowGate = %v; want ErrConfig", err)
			}
		})
	}
}

// noKeyVerifier is a verifier that reports no key identity, which no approver can sit on.
type noKeyVerifier struct{ fakeVerifier }

func (noKeyVerifier) KeyIDs() []string { return nil }

// A policy the gate cannot honor is refused (ErrConfig) before anything is recorded: an invalid
// m-of-n policy, and an approver whose verifier reports no key identity.
func TestFlowGate_PolicyRefusedBeforeRecording(t *testing.T) {
	ctx := context.Background()
	noKey := func(id string) (ApproverVerifier, bool) { return noKeyVerifier{fakeVerifier{id}}, true }
	for name, c := range map[string]struct {
		pol *ApprovalPolicy
		vf  ApproverVerifierFor
	}{
		"need above approvers": {&ApprovalPolicy{Need: 3, Approvers: []string{"a"}}, fakeVerifiers("a")},
		"verifier with no key": {&ApprovalPolicy{Need: 1, Approvers: []string{"a"}}, noKey},
	} {
		t.Run(name, func(t *testing.T) {
			j := memJournal()
			if _, err := flowGate(ctx, j, "r1", "node:pay", "pay", nil, c.pol, c.vf); !errors.Is(err, ErrConfig) {
				t.Fatalf("flowGate = %v; want ErrConfig", err)
			}
			if _, ok, err := j.Get(ctx, "r1", ApprovalRequestStep("node:pay")); err != nil || ok {
				t.Fatalf("a refused policy recorded the gate's request: %v, %v", ok, err)
			}
		})
	}
}

// SubmitDecision on a flow node refuses a decision before the gate paused (no request), and an
// identical m-of-n decision recorded before the request is reported once the gate has paused,
// whereas the approver re-signing at a new decided_at is recorded and counts.
func TestFlowGate_MofnDecisionTiming(t *testing.T) {
	ctx := context.Background()
	vf := fakeVerifiers("a")
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	args := json.RawMessage(`1`)
	subj := ApprovalSubject{RunID: "r1", ToolUseID: "node:pay", ToolName: "pay", Args: args}
	sig := func(at Decision) []byte {
		return fakeSign("a", ApprovalDecisionBytes(subj, "a", true, at.DecidedAt))
	}
	d := Decision{RunID: "r1", ToolUseID: "node:pay", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg}
	d.Signature = sig(d)

	j := memJournal()
	if err := SubmitDecision(ctx, j, d); !errors.Is(err, ErrConfig) {
		t.Fatalf("SubmitDecision before the request = %v; want ErrConfig", err)
	}

	// Plant the identical decision under the name a real submit would use, before the gate pauses.
	j2 := memJournal()
	probe := memJournal()
	if _, err := flowGate(ctx, probe, "r1", "node:pay", "pay", args, pol, vf); err == nil {
		t.Fatal("probe gate did not pause")
	}
	if err := SubmitDecision(ctx, probe, d); err != nil {
		t.Fatalf("SubmitDecision after the request: %v", err)
	}
	recs, _ := probe.History(ctx, "r1")
	var name string
	var rec Record
	for _, r := range recs {
		if IsApprovalDecision(r, "node:pay") {
			name, rec = r.Name, r
		}
	}
	if name == "" {
		t.Fatal("no decision record found after SubmitDecision")
	}
	writeRaw(t, j2, "r1", name, rec)
	var pend *ApprovalPending
	if _, err := flowGate(ctx, j2, "r1", "node:pay", "pay", args, pol, vf); !errors.As(err, &pend) {
		t.Fatalf("gate over a pre-request decision = %v; want the pause", err)
	}
	if err := SubmitDecision(ctx, j2, d); !errors.Is(err, ErrConfig) {
		t.Fatalf("re-submitting the stale decision = %v; want ErrConfig", err)
	}
	d2 := d
	d2.DecidedAt = d.DecidedAt.Add(1e6)
	d2.Signature = sig(d2)
	if err := SubmitDecision(ctx, j2, d2); err != nil {
		t.Fatalf("re-signing at a new decided_at: %v", err)
	}
	if ok, err := flowGate(ctx, j2, "r1", "node:pay", "pay", args, pol, vf); err != nil || !ok {
		t.Fatalf("gate after the re-signed decision = %v, %v; want passed", ok, err)
	}
}
