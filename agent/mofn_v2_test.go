package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// The lockout regression: a forged decision recorded under an approver's id does not take
// their place. Their real decision, recorded afterwards, counts.
func TestMofn_ForgedThenRealCounts(t *testing.T) {
	store := NewMemStore()
	pol := &ApprovalPolicy{Need: 2, Approvers: abc}
	vf := fakeVerifiers(abc...)
	var charged int

	_, _ = mofnRun(store, "r1", true, pol, vf, &charged)
	subj := subjectOf(t, store, "r1", "c1")
	forged := fakeSign("bob", ApprovalDecisionBytes(subj, "alice", true)) // bob signs as alice
	if err := ApproveAs(context.Background(), store, "r1", "c1", "alice", true, forged); err != nil {
		t.Fatal(err)
	}
	_, err := mofnRun(store, "r1", false, pol, vf, &charged)
	wantPending(t, err, counts{Need: 2, Pending: []string{"alice", "bob", "carol"}})

	approveAs(t, store, "r1", "c1", "alice", true) // alice's real decision
	approveAs(t, store, "r1", "c1", "bob", true)
	out, err := mofnRun(store, "r1", false, pol, vf, &charged)
	if err != nil || textOf(out) != "done" || charged != 1 {
		t.Fatalf("out=%q charged=%d err=%v, want done/1", textOf(out), charged, err)
	}
	final := journaledTally(t, store, "r1", "c1")
	if !reflect.DeepEqual(final.ApprovedBy, []string{"alice", "bob"}) || len(final.Records) != 3 {
		t.Fatalf("final tally = %+v, want alice and bob counted over 3 records read", final)
	}
	_, checks := TallyApprovals(mofnHistory(t, store, "r1"), subj, *pol, vf)
	if checks[0].Reason != ReasonBadSig || !checks[1].Counted {
		t.Fatalf("checks = %+v, want the forgery rejected and alice's real decision counted", checks)
	}
}

// A signature approves one exact call: the same run and call id with other arguments, another
// tool name, another call, or another run does not verify.
func TestMofn_SignatureBindsTheCall(t *testing.T) {
	store := NewMemStore()
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"alice"}}
	vf := fakeVerifiers("alice")
	var charged int

	_, _ = mofnRun(store, "r1", true, pol, vf, &charged)
	real := subjectOf(t, store, "r1", "c1")
	variants := map[string]func(s *ApprovalSubject){
		"args":     func(s *ApprovalSubject) { s.Args = json.RawMessage(`{"amount":999}`) },
		"toolName": func(s *ApprovalSubject) { s.ToolName = "refund" },
		"call":     func(s *ApprovalSubject) { s.ToolUseID = "c2" },
		"run":      func(s *ApprovalSubject) { s.RunID = "r2" },
	}
	for name, mutate := range variants {
		s := real
		mutate(&s)
		sig := fakeSign("alice", ApprovalDecisionBytes(s, "alice", true))
		if err := ApproveAs(context.Background(), store, "r1", "c1", "alice", true, sig); err != nil {
			t.Fatal(err)
		}
		_, err := mofnRun(store, "r1", false, pol, vf, &charged)
		var pend *PendingApproval
		if !errors.As(err, &pend) || pend.Quorum.Approved != 0 || charged != 0 {
			t.Fatalf("%s: a signature over another subject counted (err=%v, charged=%d)", name, err, charged)
		}
	}
	approveAs(t, store, "r1", "c1", "alice", true)
	if _, err := mofnRun(store, "r1", false, pol, vf, &charged); err != nil || charged != 1 {
		t.Fatalf("real approval after 4 mis-bound ones: err=%v charged=%d, want it to count", err, charged)
	}
}

// Records written straight into the journal, bypassing ApproveAs, can neither block an
// approver nor force a denial: invalid denials do not count toward "unreachable".
func TestMofn_JunkCannotBlockOrDeny(t *testing.T) {
	store := NewMemStore()
	pol := &ApprovalPolicy{Need: 2, Approvers: abc}
	vf := fakeVerifiers(abc...)
	var charged int

	_, _ = mofnRun(store, "r1", true, pol, vf, &charged)
	for _, id := range abc {
		writeRaw(t, store, "r1", approvalStep("c1")+":"+id+":junk", Record{Kind: StepApproval, ToolUseID: "c1", Approver: id, Approved: false, Signature: []byte("junk")})
	}
	_, err := mofnRun(store, "r1", false, pol, vf, &charged)
	wantPending(t, err, counts{Need: 2, Pending: abc})
	if _, ok := hasStep(t, store, "r1", "approval-tally:c1"); ok {
		t.Fatal("invalid denials forced a terminal tally; want the gate still open")
	}

	approveAs(t, store, "r1", "c1", "alice", true)
	approveAs(t, store, "r1", "c1", "carol", true)
	if _, err := mofnRun(store, "r1", false, pol, vf, &charged); err != nil || charged != 1 {
		t.Fatalf("err=%v charged=%d, want the real approvals to pass the gate", err, charged)
	}
}

// The signed bytes depend on the arguments' value, not their formatting: key order,
// whitespace, and HTML escaping (which encoding/json applies when re-marshaling) do not change
// them, a stored-and-reloaded call signs the same as the live one, and every bound field does.
func TestApprovalDecisionBytes_Canonical(t *testing.T) {
	base := ApprovalSubject{RunID: "r1", ToolUseID: "c1", ToolName: "refund", Args: json.RawMessage(`{"a":[1,2],"b":"<x>"}`)}
	b := func(s ApprovalSubject, approver string, approved bool) []byte {
		return ApprovalDecisionBytes(s, approver, approved)
	}
	want := b(base, "alice", true)
	for _, args := range []string{
		`{"b":"<x>","a":[1,2]}`,
		"{ \"a\": [1, 2],\n \"b\": \"<x>\" }",
		`{"a":[1,2],"b":"<x>"}`,
	} {
		s := base
		s.Args = json.RawMessage(args)
		if !bytes.Equal(b(s, "alice", true), want) {
			t.Fatalf("args %s: decision bytes differ from the same value formatted differently", args)
		}
	}

	// Empty arguments are the empty object.
	empty := base
	empty.Args = nil
	obj := base
	obj.Args = json.RawMessage(`{}`)
	if !bytes.Equal(b(empty, "alice", true), b(obj, "alice", true)) {
		t.Fatal("empty args and {} sign differently")
	}

	// Large integers are kept verbatim, not rounded through float64.
	n1, n2 := base, base
	n1.Args = json.RawMessage(`{"n":12345678901234567890}`)
	n2.Args = json.RawMessage(`{"n":12345678901234567891}`)
	if bytes.Equal(b(n1, "alice", true), b(n2, "alice", true)) {
		t.Fatal("distinct large integers signed the same")
	}

	// Every bound field changes the bytes.
	changed := map[string][]byte{
		"args": func() []byte {
			s := base
			s.Args = json.RawMessage(`{"a":[1,2],"b":"<y>"}`)
			return b(s, "alice", true)
		}(),
		"runID":    func() []byte { s := base; s.RunID = "r2"; return b(s, "alice", true) }(),
		"toolUse":  func() []byte { s := base; s.ToolUseID = "c2"; return b(s, "alice", true) }(),
		"toolName": func() []byte { s := base; s.ToolName = "charge"; return b(s, "alice", true) }(),
		"approver": b(base, "bob", true),
		"approved": b(base, "alice", false),
	}
	for field, got := range changed {
		if bytes.Equal(got, want) {
			t.Fatalf("changing %s did not change the decision bytes", field)
		}
	}

	// A call recorded in the journal and read back signs the same as the live call.
	store := NewMemStore()
	msg := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "refund", Args: base.Args}}}
	if _, err := store.Do(context.Background(), "r1", "@llm/0", func(context.Context) (Record, error) {
		return Record{Kind: StepModel, Message: &msg}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b(subjectOf(t, store, "r1", "c1"), "alice", true), want) {
		t.Fatal("the journaled call signs differently from the live call")
	}
}

// WithDecisionCheck rejects, at submission, a decision that would not count, and records
// nothing for it; correctness never depends on it.
func TestApproveAs_DecisionCheck(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	pol := &ApprovalPolicy{Need: 2, Approvers: abc}
	vf := fakeVerifiers(abc...)
	var charged int
	_, _ = mofnRun(store, "r1", true, pol, vf, &charged)
	subj := subjectOf(t, store, "r1", "c1")
	check := WithDecisionCheck(vf)
	sign := func(s ApprovalSubject, signer, as string, approved bool) []byte {
		return fakeSign(signer, ApprovalDecisionBytes(s, as, approved))
	}
	records := func() int { return countDecisions(t, store, "r1", "c1", "alice") }

	if err := ApproveAs(ctx, store, "r1", "c1", "alice", true, sign(subj, "alice", "alice", true), check); err != nil {
		t.Fatalf("valid decision: %v", err)
	}
	if err := ApproveAs(ctx, store, "r1", "c1", "alice", true, sign(subj, "alice", "alice", true), check); err != nil {
		t.Fatalf("identical resubmission: %v", err)
	}
	if n := records(); n != 1 {
		t.Fatalf("%d records after a valid decision and its resubmission, want 1", n)
	}

	other := subj
	other.Args = json.RawMessage(`{"amount":1}`)
	rejected := []struct {
		name     string
		call     string
		as       string
		approved bool
		sig      []byte
		want     error
	}{
		{"forged", "c1", "alice", true, sign(subj, "bob", "alice", true), ErrInvalidApproval},
		{"other args", "c1", "alice", true, sign(other, "alice", "alice", true), ErrInvalidApproval},
		{"unknown approver", "c1", "mallory", true, sign(subj, "mallory", "mallory", true), ErrInvalidApproval},
		{"unknown call", "nope", "alice", true, sign(subj, "alice", "alice", true), ErrInvalidApproval},
		{"flip after a valid decision", "c1", "alice", false, sign(subj, "alice", "alice", false), ErrAlreadyDecided},
	}
	for _, tc := range rejected {
		err := ApproveAs(ctx, store, "r1", tc.call, tc.as, tc.approved, tc.sig, check)
		if !errors.Is(err, tc.want) || !errors.Is(err, ErrConfig) {
			t.Fatalf("%s: err = %v, want %v (an ErrConfig)", tc.name, err, tc.want)
		}
	}
	if n := records(); n != 1 {
		t.Fatalf("%d records by alice after rejected submissions, want 1", n)
	}
	if err := ApproveAs(ctx, store, "r1", "c1", "alice", true, nil); !errors.Is(err, ErrConfig) {
		t.Fatalf("empty signature: err = %v, want ErrConfig", err)
	}
}

func TestApprovalPolicy_Validate(t *testing.T) {
	cases := []struct {
		pol ApprovalPolicy
		ok  bool
	}{
		{ApprovalPolicy{Need: 2, Approvers: abc}, true},
		{ApprovalPolicy{Need: 3, Approvers: abc}, true},
		{ApprovalPolicy{Need: 0, Approvers: abc}, false},
		{ApprovalPolicy{Need: 4, Approvers: abc}, false},
		{ApprovalPolicy{Need: 1}, false},
		{ApprovalPolicy{Need: 1, Approvers: []string{"alice", ""}}, false},
		{ApprovalPolicy{Need: 1, Approvers: []string{"alice", "alice"}}, false},
	}
	for _, tc := range cases {
		err := tc.pol.Validate()
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrConfig)) {
			t.Fatalf("%+v: Validate() = %v, want ok=%v", tc.pol, err, tc.ok)
		}
	}
}
