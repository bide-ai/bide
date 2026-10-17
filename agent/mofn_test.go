package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// fakeVerifier accepts only signatures produced by fakeSign for the same approver, so a
// test can forge a bad signature or sign as someone else without real cryptography.
type fakeVerifier struct{ id string }

// fakeAlg is the scheme fakeVerifier checks and approveAs journals.
const fakeAlg Alg = "fake"

func (fakeVerifier) Alg() Alg { return fakeAlg }

func (v fakeVerifier) PublicKey() []byte { return []byte(v.id) }

// decideAs records a decision signed under fakeAlg.
func decideAs(ctx context.Context, store *Journal, runID, toolUseID, approverID string, approved bool, sig []byte, opts ...ApproveOption) error {
	return SubmitDecision(ctx, store, Decision{RunID: runID, ToolUseID: toolUseID, ApproverID: approverID, Approved: approved, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: sig}, opts...)
}

func (v fakeVerifier) Verify(message, sig []byte) bool {
	return bytes.Equal(sig, fakeSign(v.id, message))
}

// KeyIDs reports the approver's fake key: each approver has its own.
func (v fakeVerifier) KeyIDs() []string { return []string{"fake:" + v.id} }

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
func subjectOf(t *testing.T, store *Journal, runID, toolUseID string) ApprovalSubject {
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
func approveAs(t *testing.T, store *Journal, runID, toolUseID, approverID string, approved bool) {
	t.Helper()
	sig := fakeSign(approverID, ApprovalDecisionBytes(subjectOf(t, store, runID, toolUseID), approverID, approved, testDecidedAt))
	if err := decideAs(context.Background(), store, runID, toolUseID, approverID, approved, sig); err != nil {
		t.Fatalf("decideAs(%s): %v", approverID, err)
	}
}

// writeRaw appends a decision record directly, bypassing SubmitDecision, the way someone with
// write access to the journal could.
func writeRaw(t *testing.T, store *Journal, runID, name string, rec Record) {
	t.Helper()
	if _, err := store.do(context.Background(), runID, name, func(context.Context) (Record, error) { return rec, nil }); err != nil {
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
func mofnRun(store *Journal, runID string, first bool, pol *ApprovalPolicy, verifiers ApproverVerifierFor, charged *int) (Message, error) {
	turns := [][]Emit{textTurn("done")}
	if first {
		turns = [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}
	}
	charge := &countingTool{name: "charge", approval: pol, calls: charged}
	opts := []Option{WithTools(charge)}
	if verifiers != nil {
		opts = append(opts, WithApproverVerifiers(verifiers))
	}
	a, err := New(&scriptModel{turns: turns}, store, opts...)
	if err != nil {
		return Message{}, err // an invalid policy is refused when the agent is built
	}
	res, err := a.Run(context.Background(), runID, UserText("pay"))
	if err != nil {
		return Message{}, err
	}
	return res.Message, nil
}

func wantPending(t *testing.T, err error, want counts) {
	t.Helper()
	var pend *ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("err = %v, want *ApprovalPending", err)
	}
	if pend.Quorum == nil {
		t.Fatalf("ApprovalPending.Quorum = nil, want %+v", want)
	}
	if got := countsOf(*pend.Quorum); !reflect.DeepEqual(got, want) {
		t.Fatalf("Quorum = %+v, want %+v", got, want)
	}
	if pend.ToolUseID != "c1" || pend.ToolName != "charge" {
		t.Fatalf("pend = %+v, want charge/c1", pend)
	}
}

func hasStep(t *testing.T, store *Journal, runID, name string) (Record, bool) {
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
	store := memJournal()
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
	store := memJournal()
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
	store := memJournal()
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
	legacy := memJournal()
	var n int
	one := &countingTool{name: "charge", approval: SingleApproval(), calls: &n}
	_, _ = mustNew(&scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`)}}, legacy, WithTools(one)).Run(context.Background(), "r1", UserText("pay"))
	if err := Approve(context.Background(), legacy, "r1", "c1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := mustNew(&scriptModel{turns: [][]Emit{textTurn("done")}}, legacy, WithTools(one)).Run(context.Background(), "r1", UserText("pay")); err != nil {
		t.Fatal(err)
	}
	want, _ := hasStep(t, legacy, "r1", ToolResultStep("c1"))
	if got.Kind != want.Kind || !got.IsError || string(got.Result) != string(want.Result) {
		t.Fatalf("m-of-n denied result = %+v, want the 1-of-1 denied result %+v", got, want)
	}
}

// A decision from an approver outside the policy's set is ignored, even when well signed.
func TestMofn_IneligibleApproverIgnored(t *testing.T) {
	store := memJournal()
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
	store := memJournal()
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"alice", "bob"}}
	vf := fakeVerifiers("alice", "bob")
	var charged int

	_, _ = mofnRun(store, "r1", true, pol, vf, &charged)
	// bob signs alice's decision: the signature is well formed but not alice's.
	forged := fakeSign("bob", ApprovalDecisionBytes(subjectOf(t, store, "r1", "c1"), "alice", true, testDecidedAt))
	if err := decideAs(context.Background(), store, "r1", "c1", "alice", true, forged); err != nil {
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
	store := memJournal()
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
// SubmitDecision decision does not satisfy it, and Approve resumes it.
func TestMofn_NilApprovalKeepsOneOfOne(t *testing.T) {
	store := memJournal()
	ctx := context.Background()
	var charged int
	charge := &countingTool{name: "charge", approval: SingleApproval(), calls: &charged}
	run := func(turns ...[]Emit) error {
		_, err := mustNew(
			&scriptModel{turns: turns},
			store,
			WithTools(charge),
			WithApproverVerifiers(fakeVerifiers(abc...)),
		).Run(ctx, "r1", UserText("pay"))
		return err
	}

	err := run(toolTurn("c1", "charge", `{}`), textTurn("done"))
	var pend *ApprovalPending
	if !errors.As(err, &pend) || pend.Quorum != nil {
		t.Fatalf("err = %v, want *ApprovalPending with nil Quorum", err)
	}

	approveAs(t, store, "r1", "c1", "alice", true)
	if err := run(textTurn("done")); !errors.As(err, &pend) {
		t.Fatalf("after SubmitDecision err = %v, want still *ApprovalPending on the 1-of-1 path", err)
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
		// Only SingleApproval() is the one-decision gate: a literal with no approvers is refused.
		{"no-approvers", &ApprovalPolicy{Need: 1}, fakeVerifiers(abc...)},
		{"no-approvers-need-two", &ApprovalPolicy{Need: 2}, fakeVerifiers(abc...)},
		{"no-approvers-need-zero", &ApprovalPolicy{}, fakeVerifiers(abc...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var charged int
			_, err := mofnRun(memJournal(), "r1", true, tc.pol, tc.vf, &charged)
			if !errors.Is(err, ErrConfig) {
				t.Fatalf("err = %v, want ErrConfig", err)
			}
			if charged != 0 {
				t.Fatalf("charged = %d, want 0", charged)
			}
		})
	}
}

// streamMofn drives one streamed run of a quorum-gated "charge" tool and returns every
// ApprovalRequired event it emitted plus the Final result.
func streamMofn(t *testing.T, store *Journal, runID string, first bool, pol *ApprovalPolicy, vf ApproverVerifierFor, charged *int) ([]ApprovalRequired, Message, error) {
	t.Helper()
	turns := [][]Emit{textTurn("done")}
	if first {
		turns = [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}
	}
	charge := &countingTool{name: "charge", approval: pol, calls: charged}
	as := mustNew(&scriptModel{turns: turns}, store, WithTools(charge), WithApproverVerifiers(vf)).Stream(context.Background(), runID, UserText("pay"))
	var reqs []ApprovalRequired
	for ev := range as.Events() {
		if r, ok := ev.(ApprovalRequired); ok {
			reqs = append(reqs, r)
		}
	}
	res, err := as.Result()
	var msg Message
	if res != nil {
		msg = res.Message
	}
	return reqs, msg, err
}

// The streaming path runs the same m-of-n gate as Run: each pause emits ApprovalRequired
// carrying the running tally (so a UI can show progress before Final), Final returns the
// matching *ApprovalPending, and the run proceeds exactly once at Need.
func TestMofn_StreamEmitsTally(t *testing.T) {
	store := memJournal()
	pol := &ApprovalPolicy{Need: 2, Approvers: abc}
	vf := fakeVerifiers(abc...)
	var charged int

	wantPause := func(reqs []ApprovalRequired, err error, want counts) {
		t.Helper()
		if len(reqs) != 1 || reqs[0].Quorum == nil {
			t.Fatalf("ApprovalRequired events = %+v, want one carrying a tally", reqs)
		}
		if got := countsOf(*reqs[0].Quorum); !reflect.DeepEqual(got, want) {
			t.Fatalf("event Quorum = %+v, want %+v", got, want)
		}
		wantPending(t, err, want)
	}

	reqs, _, err := streamMofn(t, store, "s1", true, pol, vf, &charged)
	wantPause(reqs, err, counts{Need: 2, Pending: []string{"alice", "bob", "carol"}})

	// The event's tally is its own copy: mutating it does not reach the returned ApprovalPending.
	reqs[0].Quorum.Pending[0] = "mutated"
	var pend *ApprovalPending
	errors.As(err, &pend)
	if pend.Quorum.Pending[0] != "alice" {
		t.Fatalf("ApprovalPending.Quorum shares the event's Pending slice: %v", pend.Quorum.Pending)
	}

	approveAs(t, store, "s1", "c1", "alice", true)
	reqs, _, err = streamMofn(t, store, "s1", false, pol, vf, &charged)
	wantPause(reqs, err, counts{Need: 2, Approved: 1, ApprovedBy: []string{"alice"}, Pending: []string{"bob", "carol"}})
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
	charge := &countingTool{name: "charge", approval: SingleApproval(), calls: &charged}
	as := mustNew(
		&scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}},
		memJournal(),
		WithTools(charge),
	).Stream(context.Background(), "s1", UserText("pay"))
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
	if _, err := as.Result(); !errors.As(err, new(*ApprovalPending)) {
		t.Fatalf("Final err = %v, want *ApprovalPending", err)
	}
}

// An m-of-n gate on a tool inside a sub-agent pauses the whole tree: the parent's Run
// surfaces the sub-run's *ApprovalPending (with its tally), and approvers sign its Subject(),
// whose RunID is the SUB-run's id, because the run id is part of the signed decision bytes. A
// decision signed against the parent's run id does not count, and does not lock the approver
// out: re-signed correctly, it counts. At Need the parent completes and the tool runs once.
func TestMofn_InsideSubAgent(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	pol := &ApprovalPolicy{Need: 2, Approvers: abc}
	vf := fakeVerifiers(abc...)
	var charged int
	charge := &countingTool{name: "charge", approval: pol, calls: &charged}
	// The gate runs in the sub-agent, so the sub-agent carries the verifier resolver.
	sub := mustNew(
		&scriptModel{turns: [][]Emit{toolTurn("s1", "charge", `{}`), textTurn("sub-done")}},
		store,
		WithTools(charge),
		WithApproverVerifiers(vf),
	)
	parent := mustNew(
		&scriptModel{turns: [][]Emit{
			toolTurn("c1", "worker", `{"task":"charge it"}`),
			textTurn("parent-done"),
		}},
		store,
		WithTools(MustSubAgent("worker", "does work", sub)),
	)

	_, err := parent.Run(ctx, "root", UserText("delegate"))
	var pend *ApprovalPending
	if !errors.As(err, &pend) || pend.Quorum == nil {
		t.Fatalf("parent Run err = %v, want an m-of-n *ApprovalPending from the sub-agent", err)
	}
	subRunID := SubRunID("root", "c1")
	if pend.RunID != subRunID || pend.ToolUseID != "s1" || pend.ToolName != "charge" {
		t.Fatalf("pend = %+v, want sub-run %s charge/s1", pend, subRunID)
	}
	if complete, _ := IsComplete(ctx, store, "root"); complete {
		t.Fatal("parent marked complete while the sub-agent's gate is pending")
	}

	// Signed against the PARENT run id: the signature does not verify for the sub-run's
	// decision bytes, so it is ignored.
	wrong := pend.Subject()
	wrong.RunID = "root"
	sig := fakeSign("alice", ApprovalDecisionBytes(wrong, "alice", true, testDecidedAt))
	if err := decideAs(ctx, store, subRunID, "s1", "alice", true, sig); err != nil {
		t.Fatal(err)
	}
	_, err = parent.Run(ctx, "root", UserText("delegate"))
	if !errors.As(err, &pend) || pend.Quorum == nil || pend.Quorum.Approved != 0 {
		t.Fatalf("after a parent-run-id signature: err=%v, want still 0 approved", err)
	}

	// alice re-signs pend.Subject() correctly: her mistaken record did not take her place.
	sig = fakeSign("alice", ApprovalDecisionBytes(pend.Subject(), "alice", true, testDecidedAt))
	if err := decideAs(ctx, store, subRunID, "s1", "alice", true, sig); err != nil {
		t.Fatal(err)
	}
	approveAs(t, store, subRunID, "s1", "bob", true)
	res, err := parent.Run(ctx, "root", UserText("delegate"))
	if err != nil {
		t.Fatalf("parent re-run at quorum: %v", err)
	}
	out := res.Message
	if textOf(out) != "parent-done" || charged != 1 {
		t.Fatalf("out=%q charged=%d, want parent-done/1", textOf(out), charged)
	}
	if complete, _ := IsComplete(ctx, store, "root"); !complete {
		t.Fatal("parent not marked complete after the sub-agent's gate passed")
	}
}

// edVerifier is an ed25519 approver verifier local to this package's tests (agent cannot import
// audit, whose Ed25519Verifier is the production one).
type edVerifier struct{ pub ed25519.PublicKey }

func (edVerifier) Alg() Alg { return "ed25519" }

func (v edVerifier) Verify(message, sig []byte) bool {
	return len(v.pub) == ed25519.PublicKeySize && ed25519.Verify(v.pub, message, sig)
}

// KeyIDs derives the identity from the public key bytes, as audit.Ed25519Verifier does.
func (v edVerifier) KeyIDs() []string {
	h := sha256.Sum256(v.pub)
	return []string{"ed25519:" + hex.EncodeToString(h[:])}
}

// sharedKey is a deterministic ed25519 key pair: the key one person holds.
func sharedKey() (ed25519.PublicKey, ed25519.PrivateKey) {
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	return priv.Public().(ed25519.PublicKey), priv
}

// signAs records a decision for approverID signed with priv, over the recorded call.
func signAs(t *testing.T, store *Journal, runID, toolUseID, approverID string, priv ed25519.PrivateKey) {
	t.Helper()
	sig := ed25519.Sign(priv, ApprovalDecisionBytes(subjectOf(t, store, runID, toolUseID), approverID, true, testDecidedAt))
	if err := SubmitDecision(context.Background(), store, Decision{RunID: runID, ToolUseID: toolUseID, ApproverID: approverID, Approved: true, DecidedAt: testDecidedAt, Alg: "ed25519", Signature: sig}); err != nil {
		t.Fatalf("SubmitDecision(%s): %v", approverID, err)
	}
}

// F5 (spec/tla, findings/ap-shared-key): two approvers whose verifiers resolve to one key are
// two seats for one person. The holder of that key signs as both and meets a 2-of-2 quorum
// alone. New refuses such a policy with ErrConfig, so the tool never runs (the gate checks it
// again on every evaluation: TestMofn_SharedKeyBetweenRounds).
func TestMofn_SharedKeyIsOneSeat(t *testing.T) {
	store := memJournal()
	pub, priv := sharedKey()
	pol := &ApprovalPolicy{Need: 2, Approvers: []string{"a1", "a2"}}
	vf := func(id string) (ApproverVerifier, bool) {
		if id == "a1" || id == "a2" {
			return edVerifier{pub: pub}, true
		}
		return nil, false
	}
	var charged int

	_ = priv // the key's holder could sign as both seats
	_, err := mofnRun(store, "r1", true, pol, vf, &charged)
	if charged != 0 {
		t.Fatalf("charge ran %d times on one person's approval of a 2-of-2 quorum, want 0", charged)
	}
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want ErrConfig for a policy whose approvers share a key", err)
	}
}

// keyVerifier is a fake verifier with explicit key identities: it accepts fakeSign signatures
// by signer, and reports keys as its KeyIDs.
type keyVerifier struct {
	signer string
	keys   []string
}

func (v keyVerifier) Verify(message, sig []byte) bool {
	return bytes.Equal(sig, fakeSign(v.signer, message))
}

func (v keyVerifier) KeyIDs() []string { return v.keys }
func (keyVerifier) Alg() Alg           { return fakeAlg }

func resolverOf(m map[string]ApproverVerifier) ApproverVerifierFor {
	return func(id string) (ApproverVerifier, bool) {
		v, ok := m[id]
		return v, ok
	}
}

func TestApprovalPolicy_ValidateKeys(t *testing.T) {
	pol := ApprovalPolicy{Need: 2, Approvers: []string{"a1", "a2", "a3"}}
	own := func(id string) ApproverVerifier { return keyVerifier{signer: id, keys: []string{"k:" + id}} }
	cases := []struct {
		name string
		vf   ApproverVerifierFor
		ok   bool
	}{
		{"distinct keys", resolverOf(map[string]ApproverVerifier{"a1": own("a1"), "a2": own("a2"), "a3": own("a3")}), true},
		{"an unknown approver fills no seat and is not refused", resolverOf(map[string]ApproverVerifier{"a1": own("a1"), "a2": own("a2")}), true},
		{"a verifier listing its own key twice", resolverOf(map[string]ApproverVerifier{"a1": keyVerifier{keys: []string{"k1", "k1"}}, "a2": own("a2")}), true},
		{"nil resolver", nil, false},
		{"two approvers, one key", resolverOf(map[string]ApproverVerifier{"a1": own("a1"), "a2": keyVerifier{keys: []string{"k:a1"}}, "a3": own("a3")}), false},
		// A rotation window: a2 accepts its new key or a1's key.
		{"key sets that overlap", resolverOf(map[string]ApproverVerifier{"a1": own("a1"), "a2": keyVerifier{keys: []string{"k:a2", "k:a1"}}}), false},
		// A hybrid whose Ed25519 component is a1's key and whose ML-DSA component is its own.
		{"one shared component", resolverOf(map[string]ApproverVerifier{"a1": keyVerifier{keys: []string{"e1", "m1"}}, "a2": keyVerifier{keys: []string{"e1", "m2"}}}), false},
		{"no key identity", resolverOf(map[string]ApproverVerifier{"a1": keyVerifier{}, "a2": own("a2")}), false},
		{"an empty key identity", resolverOf(map[string]ApproverVerifier{"a1": keyVerifier{keys: []string{"k:a1", ""}}, "a2": own("a2")}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := pol.ValidateKeys(c.vf)
			if c.ok && err != nil {
				t.Fatalf("ValidateKeys = %v, want nil", err)
			}
			if !c.ok && !errors.Is(err, ErrConfig) {
				t.Fatalf("ValidateKeys = %v, want ErrConfig", err)
			}
		})
	}
	if err := (ApprovalPolicy{Need: 3, Approvers: []string{"a1"}}).ValidateKeys(fakeVerifiers("a1")); !errors.Is(err, ErrConfig) {
		t.Fatalf("ValidateKeys on an invalid policy = %v, want Validate's ErrConfig", err)
	}
}

// The counting rule holds one seat per key whatever resolver it is given: both approvers
// sharing a key are excluded, in either signing order, and the others still count.
func TestTallyApprovals_SharedKeyNeverCounts(t *testing.T) {
	s := ApprovalSubject{RunID: "r", ToolUseID: "c1", ToolName: "charge", Args: []byte(`{}`)}
	vf := resolverOf(map[string]ApproverVerifier{
		"a1": keyVerifier{signer: "h1", keys: []string{"k1"}},
		"a2": keyVerifier{signer: "h1", keys: []string{"k1"}},
		"a3": keyVerifier{signer: "h3", keys: []string{"k3"}},
		"a4": keyVerifier{signer: "h4"},
	})
	rec := func(approver, signer string) Record {
		return Record{Name: "d:" + approver, Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{Approver: approver, ApproverAlg: fakeAlg, DecidedAt: FormatTimestamp(testDecidedAt), Signature: fakeSign(signer, ApprovalDecisionBytes(s, approver, true, testDecidedAt))}}
	}
	pol := ApprovalPolicy{Need: 2, Approvers: []string{"a1", "a2", "a3", "a4"}}
	for _, order := range [][]string{{"a1", "a2", "a3", "a4"}, {"a2", "a1", "a3", "a4"}} {
		signer := map[string]string{"a1": "h1", "a2": "h1", "a3": "h3", "a4": "h4"}
		var recs []Record
		for _, a := range order {
			recs = append(recs, rec(a, signer[a]))
		}
		tally, checks := TallyApprovals(recs, s, pol, vf)
		if tally.Approved != 1 || tally.Passed() || !reflect.DeepEqual(tally.ApprovedBy, []string{"a3"}) {
			t.Fatalf("order %v: tally = %+v, want only a3 counted", order, tally)
		}
		want := map[string]string{"a1": ReasonSharedKey, "a2": ReasonSharedKey, "a3": "", "a4": ReasonNoKeyID}
		for _, c := range checks {
			if c.Reason != want[c.Approver] {
				t.Fatalf("order %v: %s reason %q, want %q", order, c.Approver, c.Reason, want[c.Approver])
			}
		}
	}
}

// A resolver that answers the gate's check with distinct keys and the count with one shared
// key does not get one person through: the count excludes the shared seats itself, and with both
// seats of a 2-of-2 excluded the quorum is unreachable, so the gate denies (fails closed) and
// journals a tally naming them as Excluded.
func TestMofn_SharedKeyAfterCheckNeverCounts(t *testing.T) {
	store := memJournal()
	pol := &ApprovalPolicy{Need: 2, Approvers: []string{"a1", "a2"}}
	calls := map[string]int{}
	vf := func(id string) (ApproverVerifier, bool) {
		calls[id]++
		if calls[id]%2 == 1 { // the gate's check, once per approver per evaluation
			return keyVerifier{signer: id, keys: []string{"k:" + id}}, true
		}
		return keyVerifier{signer: "h1", keys: []string{"k:h1"}}, true // the count
	}
	var charged int
	charge := &countingTool{name: "charge", approval: pol, calls: &charged}
	a := mustNew(&scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}, store,
		WithTools(charge), WithApproverVerifiers(vf))
	clear(calls) // New checked the keys once per approver; the gate's evaluations alternate from here
	ctx := context.Background()
	_, _ = a.Run(ctx, "r1", UserText("pay"))
	for _, a := range pol.Approvers {
		sig := fakeSign("h1", ApprovalDecisionBytes(subjectOf(t, store, "r1", "c1"), a, true, testDecidedAt))
		if err := SubmitDecision(context.Background(), store, Decision{RunID: "r1", ToolUseID: "c1", ApproverID: a, Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: sig}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := a.Run(ctx, "r1", UserText("pay"))
	if charged != 0 {
		t.Fatalf("charge ran %d times on one person's approval, want 0", charged)
	}
	if err != nil {
		t.Fatalf("err = %v, want the gate to deny and the run to finish", err)
	}
	got, err := step(context.Background(), store, "r1", ApprovalTallyStep("c1"), func(context.Context) (ApprovalTally, error) {
		return ApprovalTally{}, errors.New("no tally journaled")
	})
	if err != nil || got.Passed() || !reflect.DeepEqual(got.Excluded, []string{"a1", "a2"}) {
		t.Fatalf("journaled tally = %+v (err %v), want a denial with a1 and a2 excluded", got, err)
	}
}

// The check runs on every evaluation: a resolver that moves a second approver onto the first
// one's key between tally rounds is refused on the next round.
func TestMofn_SharedKeyBetweenRounds(t *testing.T) {
	store := memJournal()
	pol := &ApprovalPolicy{Need: 2, Approvers: []string{"a1", "a2", "a3"}}
	var charged int
	_, err := mofnRun(store, "r1", true, pol, fakeVerifiers("a1", "a2", "a3"), &charged)
	var pend *ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("first round err = %v, want a pause", err)
	}
	approveAs(t, store, "r1", "c1", "a1", true)
	shared := func(id string) (ApproverVerifier, bool) {
		if id == "a2" {
			return keyVerifier{signer: "a1", keys: []string{"fake:a1"}}, true
		}
		return fakeVerifiers("a1", "a3")(id)
	}
	sig := fakeSign("a1", ApprovalDecisionBytes(subjectOf(t, store, "r1", "c1"), "a2", true, testDecidedAt))
	if err := SubmitDecision(context.Background(), store, Decision{RunID: "r1", ToolUseID: "c1", ApproverID: "a2", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: sig}); err != nil {
		t.Fatal(err)
	}
	_, err = mofnRun(store, "r1", false, pol, shared, &charged)
	if charged != 0 || !errors.Is(err, ErrConfig) {
		t.Fatalf("charged=%d err=%v, want 0 and ErrConfig", charged, err)
	}
}

// ptrVerifier has pointer methods that read its fields, so a typed nil *ptrVerifier inside an
// ApproverVerifier panics if called.
type ptrVerifier struct{ keys []string }

func (v *ptrVerifier) Verify(message, sig []byte) bool { return len(v.keys) > 0 }
func (v *ptrVerifier) KeyIDs() []string                { return v.keys }
func (*ptrVerifier) Alg() Alg                          { return fakeAlg }

// noPanic runs f and fails the test, rather than crashing it, if f panics.
func noPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s panicked: %v", what, r)
		}
	}()
	f()
}

// A resolver that returns a typed nil verifier (a nil *T in a non-nil interface) is a
// configuration error, refused with ErrConfig everywhere it is used, never a panic.
func TestMofn_TypedNilVerifierRefused(t *testing.T) {
	pol := ApprovalPolicy{Need: 1, Approvers: []string{"a1", "a2"}}
	vf := func(id string) (ApproverVerifier, bool) {
		if id == "a1" {
			return (*ptrVerifier)(nil), true
		}
		return fakeVerifiers("a2")(id)
	}
	noPanic(t, "ValidateKeys", func() {
		if err := pol.ValidateKeys(vf); !errors.Is(err, ErrConfig) {
			t.Fatalf("ValidateKeys = %v, want ErrConfig", err)
		}
	})
	s := ApprovalSubject{RunID: "r", ToolUseID: "c1", ToolName: "charge", Args: []byte(`{}`)}
	recs := []Record{{Name: "d:a1", Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{Approver: "a1", ApproverAlg: fakeAlg, DecidedAt: FormatTimestamp(testDecidedAt), Signature: []byte("x")}}}
	noPanic(t, "TallyApprovals", func() {
		tally, checks := TallyApprovals(recs, s, pol, vf)
		if tally.Approved != 0 || len(checks) != 1 || checks[0].Counted {
			t.Fatalf("tally = %+v checks = %+v, want a1's record not counted", tally, checks)
		}
	})
	store := memJournal()
	var charged int
	noPanic(t, "the gate", func() {
		_, err := mofnRun(store, "r1", true, &pol, vf, &charged)
		if charged != 0 || !errors.Is(err, ErrConfig) {
			t.Fatalf("charged=%d err=%v, want 0 and ErrConfig", charged, err)
		}
	})
	noPanic(t, "SubmitDecision with WithDecisionCheck", func() {
		err := SubmitDecision(context.Background(), store, Decision{RunID: "r1", ToolUseID: "c1", ApproverID: "a1", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: []byte("x")}, WithDecisionCheck(vf))
		if !errors.Is(err, ErrConfig) {
			t.Fatalf("SubmitDecision = %v, want ErrConfig", err)
		}
	})
}

// Unreachable counts only approvers who can still fill a seat: an approver excluded for a shared
// key or no key identity can never approve, so a quorum that needs them is unreachable.
func TestTallyApprovals_UnreachableExcludesSharedSeats(t *testing.T) {
	s := ApprovalSubject{RunID: "r", ToolUseID: "c1", ToolName: "charge", Args: []byte(`{}`)}
	vf := resolverOf(map[string]ApproverVerifier{
		"a1": keyVerifier{signer: "h1", keys: []string{"k1"}},
		"a2": keyVerifier{signer: "h1", keys: []string{"k1"}},
		"a3": keyVerifier{signer: "h3", keys: []string{"k3"}},
	})
	tally, _ := TallyApprovals(nil, s, ApprovalPolicy{Need: 2, Approvers: []string{"a1", "a2", "a3"}}, vf)
	if !tally.Unreachable() {
		t.Fatalf("tally = %+v: two of three seats are excluded, so Need 2 is unreachable", tally)
	}
	if !reflect.DeepEqual(tally.Pending, []string{"a3"}) || !reflect.DeepEqual(tally.Excluded, []string{"a1", "a2"}) {
		t.Fatalf("Pending = %v Excluded = %v, want [a3] and [a1 a2]: an excluded approver is not waited on", tally.Pending, tally.Excluded)
	}
	// Need 1 is still reachable through a3.
	if tally, _ := TallyApprovals(nil, s, ApprovalPolicy{Need: 1, Approvers: []string{"a1", "a2", "a3"}}, vf); tally.Unreachable() {
		t.Fatalf("tally = %+v: a3 can still approve, so Need 1 is reachable", tally)
	}
}

// A terminal tally in the journal is authoritative: a resume reuses it and does not recount, even
// if the keys changed since. This is documented behaviour, and it bounds F5's fix: a passed tally
// recorded by a version without the key check, which counted two approvers on one key, stands
// after the upgrade (the CHANGELOG's upgrade note says to finish or audit such runs first). From
// the adversarial review of #109; it asserts the behaviour rather than a fix, by decision.
func TestMofn_RecordedTallyIsReusedNotRecounted(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	pol := &ApprovalPolicy{Need: 2, Approvers: []string{"a1", "a2"}}
	var charged int
	if _, err := mofnRun(store, "r1", true, pol, fakeVerifiers("a1", "a2"), &charged); err == nil {
		t.Fatal("setup: want a pause")
	}
	// Under the old deployment a1 and a2 both resolved to key h1, held by one person.
	subj := subjectOf(t, store, "r1", "c1")
	for _, a := range pol.Approvers {
		sig := fakeSign("h1", ApprovalDecisionBytes(subj, a, true, testDecidedAt))
		if err := SubmitDecision(ctx, store, Decision{RunID: "r1", ToolUseID: "c1", ApproverID: a, Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: sig}); err != nil {
			t.Fatal(err)
		}
	}
	// The old gate's terminal tally counted both (it compared no keys; emulated here with ids
	// that differ, which is what the old rule effectively saw).
	oldVF := resolverOf(map[string]ApproverVerifier{
		"a1": keyVerifier{signer: "h1", keys: []string{"old-view-1"}},
		"a2": keyVerifier{signer: "h1", keys: []string{"old-view-2"}},
	})
	recs, _ := store.History(ctx, "r1")
	oldTally, _ := TallyApprovals(recs, subj, *pol, oldVF)
	if !oldTally.Passed() {
		t.Fatalf("setup: old tally %+v did not pass", oldTally)
	}
	oldTally.DecisionFormat = DecisionFormat // an earlier gate of this decision format
	if _, err := step(ctx, store, "r1", ApprovalTallyStep("c1"), func(context.Context) (ApprovalTally, error) { return oldTally, nil }, WithSafety(Safety{ReadOnly: true})); err != nil {
		t.Fatal(err)
	}
	// After the upgrade a2 gets a key of its own. A recount would not pass; the gate does not
	// recount, and the recorded tally stands.
	fixed := resolverOf(map[string]ApproverVerifier{
		"a1": keyVerifier{signer: "h1", keys: []string{"k:h1"}},
		"a2": keyVerifier{signer: "a2", keys: []string{"k:a2"}},
	})
	if recount, _ := TallyApprovals(recs, subj, *pol, fixed); recount.Passed() {
		t.Fatalf("setup: the recount under the fixed keys passed: %+v", recount)
	}
	if _, err := mofnRun(store, "r1", false, pol, fixed, &charged); err != nil || charged != 1 {
		t.Fatalf("charged=%d err=%v, want the recorded tally reused and the tool run once", charged, err)
	}
}

// The lockout regression: a forged decision recorded under an approver's id does not take
// their place. Their real decision, recorded afterwards, counts.
func TestMofn_ForgedThenRealCounts(t *testing.T) {
	store := memJournal()
	pol := &ApprovalPolicy{Need: 2, Approvers: abc}
	vf := fakeVerifiers(abc...)
	var charged int

	_, _ = mofnRun(store, "r1", true, pol, vf, &charged)
	subj := subjectOf(t, store, "r1", "c1")
	forged := fakeSign("bob", ApprovalDecisionBytes(subj, "alice", true, testDecidedAt)) // bob signs as alice
	if err := decideAs(context.Background(), store, "r1", "c1", "alice", true, forged); err != nil {
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
	store := memJournal()
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
		sig := fakeSign("alice", ApprovalDecisionBytes(s, "alice", true, testDecidedAt))
		if err := decideAs(context.Background(), store, "r1", "c1", "alice", true, sig); err != nil {
			t.Fatal(err)
		}
		_, err := mofnRun(store, "r1", false, pol, vf, &charged)
		var pend *ApprovalPending
		if !errors.As(err, &pend) || pend.Quorum.Approved != 0 || charged != 0 {
			t.Fatalf("%s: a signature over another subject counted (err=%v, charged=%d)", name, err, charged)
		}
	}
	approveAs(t, store, "r1", "c1", "alice", true)
	if _, err := mofnRun(store, "r1", false, pol, vf, &charged); err != nil || charged != 1 {
		t.Fatalf("real approval after 4 mis-bound ones: err=%v charged=%d, want it to count", err, charged)
	}
}

// Records written straight into the journal, bypassing SubmitDecision, can neither block an
// approver nor force a denial: invalid denials do not count toward "unreachable".
func TestMofn_JunkCannotBlockOrDeny(t *testing.T) {
	store := memJournal()
	pol := &ApprovalPolicy{Need: 2, Approvers: abc}
	vf := fakeVerifiers(abc...)
	var charged int

	_, _ = mofnRun(store, "r1", true, pol, vf, &charged)
	for _, id := range abc {
		writeRaw(t, store, "r1", approvalStep("c1")+":"+id+":junk", Record{Kind: StepApproval, ToolUseID: "c1", Approved: false, ApproverSignature: &ApproverSignature{Approver: id, DecidedAt: FormatTimestamp(testDecidedAt), Signature: []byte("junk")}})
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
		return ApprovalDecisionBytes(s, approver, approved, testDecidedAt)
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
	store := memJournal()
	msg := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "refund", Args: base.Args}}}
	if _, err := store.do(context.Background(), "r1", "@llm/0", func(context.Context) (Record, error) {
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
func TestWithDecisionCheck_RejectsAtSubmission(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	pol := &ApprovalPolicy{Need: 2, Approvers: abc}
	vf := fakeVerifiers(abc...)
	var charged int
	_, _ = mofnRun(store, "r1", true, pol, vf, &charged)
	subj := subjectOf(t, store, "r1", "c1")
	check := WithDecisionCheck(vf)
	sign := func(s ApprovalSubject, signer, as string, approved bool) []byte {
		return fakeSign(signer, ApprovalDecisionBytes(s, as, approved, testDecidedAt))
	}
	records := func() int { return countDecisions(t, store, "r1", "c1", "alice") }

	if err := decideAs(ctx, store, "r1", "c1", "alice", true, sign(subj, "alice", "alice", true), check); err != nil {
		t.Fatalf("valid decision: %v", err)
	}
	if err := decideAs(ctx, store, "r1", "c1", "alice", true, sign(subj, "alice", "alice", true), check); err != nil {
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
		err := decideAs(ctx, store, "r1", tc.call, tc.as, tc.approved, tc.sig, check)
		if !errors.Is(err, tc.want) || !errors.Is(err, ErrConfig) {
			t.Fatalf("%s: err = %v, want %v (an ErrConfig)", tc.name, err, tc.want)
		}
	}
	if n := records(); n != 1 {
		t.Fatalf("%d records by alice after rejected submissions, want 1", n)
	}
	if err := decideAs(ctx, store, "r1", "c1", "alice", true, nil); !errors.Is(err, ErrConfig) {
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

// A decision counts only under the scheme it was journaled with, and that scheme must be the
// approver key's: a valid signature journaled under another scheme name, or under none, does not
// count, and SubmitDecision refuses a decision with no scheme and (with WithDecisionCheck) one
// under a scheme the approver's key is not.
func TestMofn_DecisionCountsOnlyUnderItsScheme(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	pol := &ApprovalPolicy{Need: 1, Approvers: abc}
	vf := fakeVerifiers(abc...)
	var charged int
	_, _ = mofnRun(store, "r1", true, pol, vf, &charged)
	subj := subjectOf(t, store, "r1", "c1")
	sig := fakeSign("alice", ApprovalDecisionBytes(subj, "alice", true, testDecidedAt))

	if err := SubmitDecision(ctx, store, Decision{RunID: "r1", ToolUseID: "c1", ApproverID: "alice", Approved: true, Signature: sig}); !errors.Is(err, ErrConfig) {
		t.Fatalf("no scheme: err = %v, want ErrConfig", err)
	}
	err := SubmitDecision(ctx, store, Decision{RunID: "r1", ToolUseID: "c1", ApproverID: "alice", Approved: true, DecidedAt: testDecidedAt, Alg: "ml-dsa-65", Signature: sig}, WithDecisionCheck(vf))
	if !errors.Is(err, ErrInvalidApproval) {
		t.Fatalf("another scheme under WithDecisionCheck: err = %v, want ErrInvalidApproval", err)
	}
	writeRaw(t, store, "r1", approvalStep("c1")+":alice:noalg", Record{Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{Approver: "alice", DecidedAt: FormatTimestamp(testDecidedAt), Signature: sig}})
	writeRaw(t, store, "r1", approvalStep("c1")+":alice:otheralg", Record{Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{Approver: "alice", ApproverAlg: "ml-dsa-65", DecidedAt: FormatTimestamp(testDecidedAt), Signature: sig}})
	_, err = mofnRun(store, "r1", false, pol, vf, &charged)
	wantPending(t, err, counts{Need: 1, Pending: []string{"alice", "bob", "carol"}})
	_, checks := TallyApprovals(mofnHistory(t, store, "r1"), subj, *pol, vf)
	if len(checks) != 2 || checks[0].Reason != ReasonAlg || checks[1].Reason != ReasonAlg {
		t.Fatalf("checks = %+v, want both decisions refused for their scheme", checks)
	}

	if err := decideAs(ctx, store, "r1", "c1", "alice", true, sig); err != nil {
		t.Fatal(err)
	}
	if out, err := mofnRun(store, "r1", false, pol, vf, &charged); err != nil || textOf(out) != "done" || charged != 1 {
		t.Fatalf("out=%q charged=%d err=%v, want the decision under the key's scheme to count", textOf(out), charged, err)
	}
}

// sharedKeyVerifier is one key, whatever approver id it is resolved for.
type sharedKeyVerifier struct{}

func (sharedKeyVerifier) Alg() Alg         { return fakeAlg }
func (sharedKeyVerifier) KeyIDs() []string { return []string{"fake:one key"} }
func (sharedKeyVerifier) Verify(message, sig []byte) bool {
	return bytes.Equal(sig, fakeSign("shared", message))
}

// Two approver ids that resolve to one key are one signer, so a 2-of-2 policy is not met by one key
// holder signing as both. Under the one seat-per-key rule (#109, KeyIDs) neither id counts, whoever
// signed first (ReasonSharedKey), and the gate refuses such a policy outright (ValidateKeys).
func TestMofn_OneKeyUnderTwoApproverIdsCountsOnce(t *testing.T) {
	s := ApprovalSubject{RunID: "r", ToolUseID: "c", ToolName: "wire", Args: json.RawMessage(`{}`)}
	var recs []Record
	for _, id := range []string{"alice", "bob"} {
		recs = append(recs, Record{Name: "approval:c:" + id, Kind: StepApproval, ToolUseID: "c", Approved: true, ApproverSignature: &ApproverSignature{Approver: id, ApproverAlg: fakeAlg, DecidedAt: FormatTimestamp(testDecidedAt), Signature: fakeSign("shared", ApprovalDecisionBytes(s, id, true, testDecidedAt))}})
	}
	p := ApprovalPolicy{Need: 2, Approvers: []string{"alice", "bob"}}
	got, checks := TallyApprovals(recs, s, p, func(string) (ApproverVerifier, bool) { return sharedKeyVerifier{}, true })
	if got.Passed() || got.Approved != 0 {
		t.Fatalf("tally = %+v, want no approval counted for the shared key", got)
	}
	if len(checks) != 2 || checks[0].Counted || checks[0].Reason != ReasonSharedKey || checks[1].Counted || checks[1].Reason != ReasonSharedKey {
		t.Fatalf("checks = %+v, want both decisions refused for sharing one key", checks)
	}
	if err := p.ValidateKeys(func(string) (ApproverVerifier, bool) { return sharedKeyVerifier{}, true }); !errors.Is(err, ErrConfig) {
		t.Fatalf("ValidateKeys = %v, want ErrConfig", err)
	}
}
