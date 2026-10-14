package agent

import (
	"context"
	"testing"
)

// F1: a decision journaled by a release that names by signature, resubmitted without
// WithDecisionCheck (a retry after a lost response, or a rolling deploy where an old and a new
// binary both take submissions), used to be a no-op (same signature, same name). It now writes a
// second record of the same decision and ReportWritten says true, so a service acting once per
// recorded decision acts twice.
func TestUncheckedResubmitOfLegacyNamedDecision(t *testing.T) {
	ctx := context.Background()
	j, _, base := pausedOne(t)
	d := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: append([]byte{1}, base...)}
	legacy := approvalDecisionStep("c1", "a", true, fakeAlg, FormatTimestamp(testDecidedAt), d.Signature)
	if _, err := j.do(ctx, "r", legacy, func(context.Context) (Record, error) {
		return Record{Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{
			Approver: "a", ApproverAlg: fakeAlg, Signature: d.Signature, DecidedAt: FormatTimestamp(testDecidedAt), RecordedAt: FormatTimestamp(testDecidedAt)}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	var wrote bool
	if err := SubmitDecision(ctx, j, d, ReportWritten(&wrote)); err != nil {
		t.Fatal(err)
	}
	if wrote || len(decisionRecords(t, j)) != 1 {
		t.Fatalf("identical resubmission of a legacy-named decision: wrote=%v, %d records; want false and 1", wrote, len(decisionRecords(t, j)))
	}
}

// DecisionStep is the name SubmitDecision gives a decision first; it is not a lookup key. With a bad
// record at that name the real decision lives under another name, and HeldDecisionStep finds it.
func TestHeldDecisionStepFindsTheRecordWhereverItIs(t *testing.T) {
	ctx := context.Background()
	j, vf, base := pausedOne(t)
	junk := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: []byte{9, 9, 9}}
	real := junk
	real.Signature = append([]byte{1}, base...)
	if _, ok, err := HeldDecisionStep(ctx, j, real); err != nil || ok {
		t.Fatalf("before anything: %v %v", ok, err)
	}
	if err := SubmitDecision(ctx, j, junk); err != nil {
		t.Fatal(err)
	}
	if name, ok, err := HeldDecisionStep(ctx, j, real); err != nil || ok {
		t.Fatalf("a junk record at the identity name is not the real decision: %q %v %v", name, ok, err)
	}
	if name, ok, err := HeldDecisionStep(ctx, j, junk); err != nil || !ok || name != DecisionStep(junk) {
		t.Fatalf("junk: %q %v %v", name, ok, err)
	}
	if err := SubmitDecision(ctx, j, real, WithDecisionCheck(vf)); err != nil {
		t.Fatal(err)
	}
	name, ok, err := HeldDecisionStep(ctx, j, real)
	if err != nil || !ok || name == DecisionStep(real) {
		t.Fatalf("fallback: %q %v %v", name, ok, err)
	}
	if r, _, _ := j.Get(ctx, "r", name); string(r.Signature()) != string(real.Signature) {
		t.Fatalf("the named record is not the real decision: %v", r.Signature())
	}
	// A decision under the legacy name is found too.
	l := real
	l.DecidedAt = testDecidedAt.Add(1e9)
	legacy := approvalDecisionStep("c1", "a", true, fakeAlg, FormatTimestamp(l.DecidedAt), l.Signature)
	if _, err := j.do(ctx, "r", legacy, func(context.Context) (Record, error) {
		return Record{Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{
			Approver: "a", ApproverAlg: fakeAlg, Signature: l.Signature, DecidedAt: FormatTimestamp(l.DecidedAt)}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if name, ok, err := HeldDecisionStep(ctx, j, l); err != nil || !ok || name != legacy {
		t.Fatalf("legacy: %q %v %v", name, ok, err)
	}
}
