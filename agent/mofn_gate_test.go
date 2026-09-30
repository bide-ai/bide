package agent

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
)

// fakeVerifier accepts only signatures produced by fakeSign for the same approver, so a
// test can forge a bad signature or sign as someone else without real cryptography.
type fakeVerifier struct{ id string }

func (v fakeVerifier) Verify(message, sig []byte) bool {
	return bytes.Equal(sig, fakeSign(v.id, message))
}

func fakeSign(approverID string, message []byte) []byte {
	return append([]byte(approverID+"|"), message...)
}

// fakeVerifiers resolves every id in known to a fakeVerifier; any other id is unknown.
func fakeVerifiers(known ...string) ApproverVerifierFor {
	set := map[string]bool{}
	for _, k := range known {
		set[k] = true
	}
	return func(id string) (ApproverVerifier, bool) {
		if !set[id] {
			return nil, false
		}
		return fakeVerifier{id: id}, true
	}
}

// subjectOf reads the recorded call, the way an approver works from pend.Subject().
func subjectOf(t *testing.T, store Durable, runID, toolUseID string) ApprovalSubject {
	t.Helper()
	recs, err := store.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	_, call, ok := FindToolCall(recs, toolUseID)
	if !ok {
		t.Fatalf("no recorded call %s in run %s", toolUseID, runID)
	}
	return ApprovalSubject{RunID: runID, ToolUseID: toolUseID, ToolName: call.Name, Args: call.Args}
}

// approveAs records a correctly signed decision for approverID on the recorded call.
func approveAs(t *testing.T, store Durable, runID, toolUseID, approverID string, approved bool) {
	t.Helper()
	sig := fakeSign(approverID, ApprovalDecisionBytes(subjectOf(t, store, runID, toolUseID), approverID, approved))
	if err := ApproveAs(context.Background(), store, runID, toolUseID, approverID, approved, sig); err != nil {
		t.Fatalf("ApproveAs(%s): %v", approverID, err)
	}
}

// writeRaw appends a decision record directly, bypassing ApproveAs, the way someone with
// write access to the journal could.
func writeRaw(t *testing.T, store Durable, runID, name string, rec Record) {
	t.Helper()
	if _, err := store.Do(context.Background(), runID, name, func(context.Context) (Record, error) { return rec, nil }); err != nil {
		t.Fatal(err)
	}
}

// counts is the part of a tally a test asserts: the numbers and who, not record names.
type counts struct {
	Need, Approved, Denied int
	ApprovedBy, DeniedBy   []string
	Pending                []string
}

func countsOf(t ApprovalTally) counts {
	return counts{t.Need, t.Approved, t.Denied, t.ApprovedBy, t.DeniedBy, t.Pending}
}

// mofnRun drives one run of a fresh agent over store with a quorum-gated "charge" tool. The
// first run needs the tool turn; resumes replay it from the journal and only need the answer.
func mofnRun(store Durable, runID string, first bool, pol *ApprovalPolicy, verifiers ApproverVerifierFor, charged *int) (Message, error) {
	turns := [][]Emit{textTurn("done")}
	if first {
		turns = [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}
	}
	charge := &countingTool{name: "charge", safety: Safety{Approval: pol}, calls: charged}
	a := New(&scriptModel{turns: turns}, store, charge)
	if verifiers != nil {
		a.WithApproverVerifiers(verifiers)
	}
	return a.Run(context.Background(), runID, "pay")
}

func wantPending(t *testing.T, err error, want counts) {
	t.Helper()
	var pend *PendingApproval
	if !errors.As(err, &pend) {
		t.Fatalf("err = %v, want *PendingApproval", err)
	}
	if pend.Quorum == nil {
		t.Fatalf("PendingApproval.Quorum = nil, want %+v", want)
	}
	if got := countsOf(*pend.Quorum); !reflect.DeepEqual(got, want) {
		t.Fatalf("Quorum = %+v, want %+v", got, want)
	}
	if pend.ToolUseID != "c1" || pend.ToolName != "charge" {
		t.Fatalf("pend = %+v, want charge/c1", pend)
	}
}

func hasStep(t *testing.T, store Durable, runID, name string) (Record, bool) {
	t.Helper()
	recs, err := store.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Name == name {
			return r, true
		}
	}
	return Record{}, false
}

var abc = []string{"alice", "bob", "carol"}

// The gate pauses below Need with a running tally, and proceeds at exactly Need approvals.
func TestMofn_ProceedsAtNeed(t *testing.T) {
	store := NewMemStore()
	pol := &ApprovalPolicy{Need: 2, Approvers: abc}
	vf := fakeVerifiers(abc...)
	var charged int

	_, err := mofnRun(store, "r1", true, pol, vf, &charged)
	wantPending(t, err, counts{Need: 2, Pending: []string{"alice", "bob", "carol"}})

	approveAs(t, store, "r1", "c1", "alice", true)
	_, err = mofnRun(store, "r1", false, pol, vf, &charged)
	wantPending(t, err, counts{Need: 2, Approved: 1, ApprovedBy: []string{"alice"}, Pending: []string{"bob", "carol"}})
	if charged != 0 {
		t.Fatalf("charge ran %d times below quorum, want 0", charged)
	}
	if _, ok := hasStep(t, store, "r1", "approval-tally:c1"); ok {
		t.Fatal("non-final tally journaled on a pause; want only the terminal tally recorded")
	}

	approveAs(t, store, "r1", "c1", "bob", true)
	out, err := mofnRun(store, "r1", false, pol, vf, &charged)
	if err != nil {
		t.Fatalf("run at quorum: %v", err)
	}
	if textOf(out) != "done" || charged != 1 {
		t.Fatalf("out=%q charged=%d, want done/1", textOf(out), charged)
	}
}

// The final tally is journaled as its own StepValue and reads back as an ApprovalTally.
func TestMofn_TallyStepJournaled(t *testing.T) {
	store := NewMemStore()
	pol := &ApprovalPolicy{Need: 2, Approvers: abc}
	vf := fakeVerifiers(abc...)
	var charged int

	_, _ = mofnRun(store, "r1", true, pol, vf, &charged)
	approveAs(t, store, "r1", "c1", "alice", true)
	approveAs(t, store, "r1", "c1", "carol", true)
	if _, err := mofnRun(store, "r1", false, pol, vf, &charged); err != nil {
		t.Fatalf("run at quorum: %v", err)
	}

	rec, ok := hasStep(t, store, "r1", "approval-tally:c1")
	if !ok || rec.Kind != StepValue {
		t.Fatalf("approval-tally:c1 = %+v (found %v), want a StepValue", rec, ok)
	}
	// step returns the recorded value without running fn, so the tally is read from the journal.
	got, err := step(context.Background(), store, "r1", ApprovalTallyStep("c1"), func(context.Context) (ApprovalTally, error) {
		return ApprovalTally{}, errors.New("tally step not journaled")
	})
	if err != nil {
		t.Fatal(err)
	}
	want := counts{Need: 2, Approved: 2, ApprovedBy: []string{"alice", "carol"}, Pending: []string{"bob"}}
	if c := countsOf(got); !reflect.DeepEqual(c, want) {
		t.Fatalf("journaled tally = %+v, want %+v", c, want)
	}
	// It records the policy it enforced and every decision record it read.
	if !reflect.DeepEqual(got.Approvers, abc) || len(got.Records) != 2 {
		t.Fatalf("journaled tally Approvers=%v Records=%v, want the policy set and 2 records", got.Approvers, got.Records)
	}
}

// Once n - denied < Need the gate can never pass: it auto-denies with the same denied tool
// result a 1-of-1 denial produces, and the model reacts to it.
func TestMofn_AutoDenyWhenUnreachable(t *testing.T) {
	store := NewMemStore()
	pol := &ApprovalPolicy{Need: 2, Approvers: abc}
	vf := fakeVerifiers(abc...)
	var charged int

	_, _ = mofnRun(store, "r1", true, pol, vf, &charged)
	approveAs(t, store, "r1", "c1", "bob", false)
	_, err := mofnRun(store, "r1", false, pol, vf, &charged)
	wantPending(t, err, counts{Need: 2, Denied: 1, DeniedBy: []string{"bob"}, Pending: []string{"alice", "carol"}})

	approveAs(t, store, "r1", "c1", "carol", false)
	out, err := mofnRun(store, "r1", false, pol, vf, &charged)
	if err != nil {
		t.Fatalf("run after unreachable quorum: %v", err)
	}
	if textOf(out) != "done" || charged != 0 {
		t.Fatalf("out=%q charged=%d, want done/0", textOf(out), charged)
	}
	got, ok := hasStep(t, store, "r1", ToolResultStep("c1"))
	if !ok {
		t.Fatal("no tool result recorded for the denied call")
	}
	if tr, ok := hasStep(t, store, "r1", "approval-tally:c1"); !ok || tr.Kind != StepValue {
		t.Fatalf("approval-tally:c1 = %+v (found %v), want the final deny tally journaled", tr, ok)
	}

	// The same call denied through the 1-of-1 path.
	legacy := NewMemStore()
	var n int
	one := &countingTool{name: "charge", safety: Safety{RequiresApproval: true}, calls: &n}
	_, _ = New(&scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`)}}, legacy, one).Run(context.Background(), "r1", "pay")
	if err := Approve(context.Background(), legacy, "r1", "c1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := New(&scriptModel{turns: [][]Emit{textTurn("done")}}, legacy, one).Run(context.Background(), "r1", "pay"); err != nil {
		t.Fatal(err)
	}
	want, _ := hasStep(t, legacy, "r1", ToolResultStep("c1"))
	if got.Kind != want.Kind || !got.IsError || string(got.Result) != string(want.Result) {
		t.Fatalf("m-of-n denied result = %+v, want the 1-of-1 denied result %+v", got, want)
	}
}

// A decision from an approver outside the policy's set is ignored, even when well signed.
func TestMofn_IneligibleApproverIgnored(t *testing.T) {
	store := NewMemStore()
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"alice", "bob"}}
	vf := fakeVerifiers("alice", "bob", "mallory")
	var charged int

	_, _ = mofnRun(store, "r1", true, pol, vf, &charged)
	approveAs(t, store, "r1", "c1", "mallory", true)
	_, err := mofnRun(store, "r1", false, pol, vf, &charged)
	wantPending(t, err, counts{Need: 1, Pending: []string{"alice", "bob"}})
	if charged != 0 {
		t.Fatalf("charge ran %d times on an ineligible approval, want 0", charged)
	}
}

// A decision whose signature does not verify is ignored.
func TestMofn_BadSignatureIgnored(t *testing.T) {
	store := NewMemStore()
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"alice", "bob"}}
	vf := fakeVerifiers("alice", "bob")
	var charged int

	_, _ = mofnRun(store, "r1", true, pol, vf, &charged)
	// bob signs alice's decision: the signature is well formed but not alice's.
	forged := fakeSign("bob", ApprovalDecisionBytes(subjectOf(t, store, "r1", "c1"), "alice", true))
	if err := ApproveAs(context.Background(), store, "r1", "c1", "alice", true, forged); err != nil {
		t.Fatal(err)
	}
	_, err := mofnRun(store, "r1", false, pol, vf, &charged)
	wantPending(t, err, counts{Need: 1, Pending: []string{"alice", "bob"}})
	if charged != 0 {
		t.Fatalf("charge ran %d times on a forged approval, want 0", charged)
	}
}

// An approver's later valid decision is superseded: the first valid one counts, and an
// identical resubmission is a no-op.
func TestMofn_DuplicateApproverDeduped(t *testing.T) {
	store := NewMemStore()
	pol := &ApprovalPolicy{Need: 2, Approvers: abc}
	vf := fakeVerifiers(abc...)
	var charged int

	_, _ = mofnRun(store, "r1", true, pol, vf, &charged)
	approveAs(t, store, "r1", "c1", "alice", true)
	approveAs(t, store, "r1", "c1", "alice", true) // identical: same record name, no-op
	approveAs(t, store, "r1", "c1", "alice", false)
	_, err := mofnRun(store, "r1", false, pol, vf, &charged)
	wantPending(t, err, counts{Need: 2, Approved: 1, ApprovedBy: []string{"alice"}, Pending: []string{"bob", "carol"}})

	recs, _ := store.History(context.Background(), "r1")
	tally, checks := TallyApprovals(recs, subjectOf(t, store, "r1", "c1"), *pol, vf)
	if len(tally.Records) != 2 || len(checks) != 2 || checks[1].Reason != ReasonSuperseded {
		t.Fatalf("records=%v checks=%+v, want 2 records with the flip superseded", tally.Records, checks)
	}
}

// A tool with a nil Approval keeps the 1-of-1 path: it pauses with no Quorum, a per-approver
// ApproveAs decision does not satisfy it, and Approve resumes it.
func TestMofn_NilApprovalKeepsOneOfOne(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()
	var charged int
	charge := &countingTool{name: "charge", safety: Safety{RequiresApproval: true}, calls: &charged}
	run := func(turns ...[]Emit) error {
		_, err := New(&scriptModel{turns: turns}, store, charge).WithApproverVerifiers(fakeVerifiers(abc...)).Run(ctx, "r1", "pay")
		return err
	}

	err := run(toolTurn("c1", "charge", `{}`), textTurn("done"))
	var pend *PendingApproval
	if !errors.As(err, &pend) || pend.Quorum != nil {
		t.Fatalf("err = %v, want *PendingApproval with nil Quorum", err)
	}

	approveAs(t, store, "r1", "c1", "alice", true)
	if err := run(textTurn("done")); !errors.As(err, &pend) {
		t.Fatalf("after ApproveAs err = %v, want still *PendingApproval on the 1-of-1 path", err)
	}

	if err := Approve(ctx, store, "r1", "c1", true); err != nil {
		t.Fatal(err)
	}
	if err := run(textTurn("done")); err != nil {
		t.Fatalf("resume after Approve: %v", err)
	}
	if charged != 1 {
		t.Fatalf("charged = %d, want 1", charged)
	}
	if _, ok := hasStep(t, store, "r1", "approval-tally:c1"); ok {
		t.Fatal("1-of-1 path journaled an approval tally")
	}
}

// Misconfiguration fails loudly rather than tallying zero decisions.
func TestMofn_ConfigErrors(t *testing.T) {
	cases := []struct {
		name string
		pol  *ApprovalPolicy
		vf   ApproverVerifierFor
	}{
		{"no-verifiers", &ApprovalPolicy{Need: 1, Approvers: abc}, nil},
		{"need-zero", &ApprovalPolicy{Need: 0, Approvers: abc}, fakeVerifiers(abc...)},
		{"need-above-n", &ApprovalPolicy{Need: 4, Approvers: abc}, fakeVerifiers(abc...)},
		{"duplicate-approver", &ApprovalPolicy{Need: 2, Approvers: []string{"alice", "alice", "bob"}}, fakeVerifiers(abc...)},
		{"empty-approver", &ApprovalPolicy{Need: 1, Approvers: []string{"alice", ""}}, fakeVerifiers(abc...)},
		{"no-approvers", &ApprovalPolicy{Need: 1}, fakeVerifiers(abc...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var charged int
			_, err := mofnRun(NewMemStore(), "r1", true, tc.pol, tc.vf, &charged)
			if !errors.Is(err, ErrConfig) {
				t.Fatalf("err = %v, want ErrConfig", err)
			}
			if charged != 0 {
				t.Fatalf("charged = %d, want 0", charged)
			}
		})
	}
}
