package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// streamMofn drives one streamed run of a quorum-gated "charge" tool and returns every
// ApprovalRequired event it emitted plus the Final result.
func streamMofn(t *testing.T, store Durable, runID string, first bool, pol *ApprovalPolicy, vf ApproverVerifierFor, charged *int) ([]ApprovalRequired, Message, error) {
	t.Helper()
	turns := [][]Emit{textTurn("done")}
	if first {
		turns = [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}
	}
	charge := &countingTool{name: "charge", safety: Safety{Approval: pol}, calls: charged}
	as := New(&scriptModel{turns: turns}, store, charge).WithApproverVerifiers(vf).Stream(context.Background(), runID, "pay")
	var reqs []ApprovalRequired
	for ev := range as.Events() {
		if r, ok := ev.(ApprovalRequired); ok {
			reqs = append(reqs, r)
		}
	}
	msg, err := as.Final()
	return reqs, msg, err
}

// The streaming path runs the same m-of-n gate as Run: each pause emits ApprovalRequired
// carrying the running tally (so a UI can show progress before Final), Final returns the
// matching *PendingApproval, and the run proceeds exactly once at Need.
func TestMofn_StreamEmitsTally(t *testing.T) {
	store := NewMemStore()
	pol := &ApprovalPolicy{Need: 2, Approvers: abc}
	vf := fakeVerifiers(abc...)
	var charged int

	wantPause := func(reqs []ApprovalRequired, err error, want ApprovalTally) {
		t.Helper()
		if len(reqs) != 1 || reqs[0].Quorum == nil {
			t.Fatalf("ApprovalRequired events = %+v, want one carrying a tally", reqs)
		}
		if !reflect.DeepEqual(*reqs[0].Quorum, want) {
			t.Fatalf("event Quorum = %+v, want %+v", *reqs[0].Quorum, want)
		}
		wantPending(t, err, want)
	}

	reqs, _, err := streamMofn(t, store, "s1", true, pol, vf, &charged)
	wantPause(reqs, err, ApprovalTally{Need: 2, Pending: []string{"alice", "bob", "carol"}})

	// The event's tally is its own copy: mutating it does not reach the returned PendingApproval.
	reqs[0].Quorum.Pending[0] = "mutated"
	var pend *PendingApproval
	errors.As(err, &pend)
	if pend.Quorum.Pending[0] != "alice" {
		t.Fatalf("PendingApproval.Quorum shares the event's Pending slice: %v", pend.Quorum.Pending)
	}

	approveAs(t, store, "s1", "c1", "alice", true)
	reqs, _, err = streamMofn(t, store, "s1", false, pol, vf, &charged)
	wantPause(reqs, err, ApprovalTally{Need: 2, Approved: 1, Pending: []string{"bob", "carol"}})
	if charged != 0 {
		t.Fatalf("charge ran %d times below quorum, want 0", charged)
	}

	approveAs(t, store, "s1", "c1", "bob", true)
	reqs, out, err := streamMofn(t, store, "s1", false, pol, vf, &charged)
	if err != nil {
		t.Fatalf("streamed run at quorum: %v", err)
	}
	if len(reqs) != 0 {
		t.Fatalf("ApprovalRequired fired at quorum: %+v", reqs)
	}
	if textOf(out) != "done" || charged != 1 {
		t.Fatalf("out=%q charged=%d, want done/1", textOf(out), charged)
	}
}

// A 1-of-1 gate's ApprovalRequired carries no tally.
func TestMofn_StreamOneOfOneHasNoTally(t *testing.T) {
	var charged int
	charge := &countingTool{name: "charge", safety: Safety{RequiresApproval: true}, calls: &charged}
	as := New(&scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}, NewMemStore(), charge).
		Stream(context.Background(), "s1", "pay")
	var seen bool
	for ev := range as.Events() {
		if r, ok := ev.(ApprovalRequired); ok {
			seen = true
			if r.Quorum != nil {
				t.Fatalf("1-of-1 ApprovalRequired.Quorum = %+v, want nil", r.Quorum)
			}
		}
	}
	if !seen {
		t.Fatal("no ApprovalRequired event for a RequiresApproval tool")
	}
	if _, err := as.Final(); !errors.As(err, new(*PendingApproval)) {
		t.Fatalf("Final err = %v, want *PendingApproval", err)
	}
}

// An m-of-n gate on a tool inside a sub-agent pauses the whole tree: the parent's Run
// surfaces the sub-run's *PendingApproval (with its tally), and approvers must sign against
// that SUB-run's id, because the run id is part of the signed decision bytes. A decision
// signed against the parent's run id does not count. At Need the parent completes and the
// tool runs exactly once.
func TestMofn_InsideSubAgent(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	pol := &ApprovalPolicy{Need: 2, Approvers: abc}
	vf := fakeVerifiers(abc...)
	var charged int
	charge := &countingTool{name: "charge", safety: Safety{Approval: pol}, calls: &charged}
	// The gate runs in the sub-agent, so the sub-agent carries the verifier resolver.
	sub := New(&scriptModel{turns: [][]Emit{toolTurn("s1", "charge", `{}`), textTurn("sub-done")}}, store, charge).
		WithApproverVerifiers(vf)
	parent := New(&scriptModel{turns: [][]Emit{
		toolTurn("c1", "worker", `{"task":"charge it"}`),
		textTurn("parent-done"),
	}}, store, SubAgent("worker", "does work", sub))

	_, err := parent.Run(ctx, "root", "delegate")
	var pend *PendingApproval
	if !errors.As(err, &pend) || pend.Quorum == nil {
		t.Fatalf("parent Run err = %v, want an m-of-n *PendingApproval from the sub-agent", err)
	}
	const subRunID = "root/c1"
	if pend.RunID != subRunID || pend.ToolUseID != "s1" || pend.ToolName != "charge" {
		t.Fatalf("pend = %+v, want sub-run %s charge/s1", pend, subRunID)
	}
	if complete, _ := IsComplete(ctx, store, "root"); complete {
		t.Fatal("parent marked complete while the sub-agent's gate is pending")
	}

	// Signed against the PARENT run id: the signature does not verify for the sub-run's
	// decision bytes, so it is ignored.
	sig := fakeSign("alice", ApprovalDecisionBytes("root", "s1", "alice", true))
	if err := ApproveAs(ctx, store, subRunID, "s1", "alice", true, sig); err != nil {
		t.Fatal(err)
	}
	_, err = parent.Run(ctx, "root", "delegate")
	if !errors.As(err, &pend) || pend.Quorum == nil || pend.Quorum.Approved != 0 {
		t.Fatalf("after a parent-run-id signature: err=%v, want still 0 approved", err)
	}

	// bob and carol sign against the sub-run id (alice's slot is already used by her first,
	// mis-signed decision, so she cannot count).
	approveAs(t, store, subRunID, "s1", "bob", true)
	approveAs(t, store, subRunID, "s1", "carol", true)
	out, err := parent.Run(ctx, "root", "delegate")
	if err != nil {
		t.Fatalf("parent re-run at quorum: %v", err)
	}
	if textOf(out) != "parent-done" || charged != 1 {
		t.Fatalf("out=%q charged=%d, want parent-done/1", textOf(out), charged)
	}
	if complete, _ := IsComplete(ctx, store, "root"); !complete {
		t.Fatal("parent not marked complete after the sub-agent's gate passed")
	}
}
