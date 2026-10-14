package agent

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"
)

type randVerifier struct{ id string }

func (randVerifier) Alg() Alg            { return fakeAlg }
func (v randVerifier) PublicKey() []byte { return []byte(v.id) }
func (v randVerifier) KeyIDs() []string  { return []string{"rand:" + v.id} }
func (v randVerifier) Verify(m, sig []byte) bool {
	return len(sig) > 1 && bytes.Equal(sig[1:], fakeSign(v.id, m))
}

// A randomized scheme (ECDSA, as KMS/HSM keys sign) gives one decision different signature bytes.
// Concurrent submissions of that one decision (a client retry racing its first attempt) must
// still report exactly one write, and record one decision.
func TestRev282_RandomizedConcurrentReportWritten(t *testing.T) {
	ctx := context.Background()
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	vf := func(id string) (ApproverVerifier, bool) { return randVerifier{id}, id == "a" }
	store := NewMemStore()
	j := mustJournal(store)
	var charged int
	if _, err := mofnRun(j, "r", true, pol, vf, &charged); err == nil {
		t.Fatal("run did not pause")
	}
	base := fakeSign("a", ApprovalDecisionBytes(subjectOf(t, j, "r", "c1"), "a", true, testDecidedAt))
	const n = 8
	wrote := make([]bool, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			d := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg,
				Signature: append([]byte{byte(i)}, base...)}
			<-start
			if err := SubmitDecision(ctx, mustJournal(store), d, WithDecisionCheck(vf), ReportWritten(&wrote[i])); err != nil {
				t.Logf("call %d: %v", i, err)
			}
		})
	}
	close(start)
	wg.Wait()
	got := 0
	for _, w := range wrote {
		if w {
			got++
		}
	}
	recs, _ := j.History(ctx, "r")
	nrec := 0
	for _, r := range recs {
		if r.Kind == StepApproval && r.Approver() == "a" {
			nrec++
		}
	}
	t.Logf("wrote=%v records=%d", wrote, nrec)
	if got != 1 || nrec != 1 {
		t.Fatalf("one decision, re-signed by a randomized scheme: %d submissions report a write and %d records are journaled; want 1 and 1", got, nrec)
	}
}

func pausedOne(t *testing.T) (*Journal, ApproverVerifierFor, []byte) {
	t.Helper()
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	vf := func(id string) (ApproverVerifier, bool) { return randVerifier{id}, id == "a" }
	j := mustJournal(NewMemStore())
	var charged int
	if _, err := mofnRun(j, "r", true, pol, vf, &charged); err == nil {
		t.Fatal("run did not pause")
	}
	return j, vf, fakeSign("a", ApprovalDecisionBytes(subjectOf(t, j, "r", "c1"), "a", true, testDecidedAt))
}

func decisionRecords(t *testing.T, j *Journal) []Record {
	t.Helper()
	recs, _ := j.History(context.Background(), "r")
	var out []Record
	for _, r := range recs {
		if r.Kind == StepApproval && r.Approver() == "a" {
			out = append(out, r)
		}
	}
	return out
}

// The step name is the decision's identity, never its signature bytes: two re-signings of one
// decision share it, and any other approver, verdict, scheme, time or call has another.
func TestDecisionStepIsTheDecisionsIdentity(t *testing.T) {
	d := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: []byte("one")}
	e := d
	e.Signature = []byte("two, a randomized re-signing")
	if DecisionStep(d) != DecisionStep(e) {
		t.Fatal("a re-signing of one decision has another name")
	}
	for name, f := range map[string]func(*Decision){
		"approver": func(x *Decision) { x.ApproverID = "b" }, "verdict": func(x *Decision) { x.Approved = false },
		"alg": func(x *Decision) { x.Alg = "other" }, "time": func(x *Decision) { x.DecidedAt = x.DecidedAt.Add(time.Second) },
		"call": func(x *Decision) { x.ToolUseID = "c2" },
	} {
		x := d
		f(&x)
		if DecisionStep(x) == DecisionStep(d) {
			t.Errorf("%s: same name for a different decision", name)
		}
	}
}

// A decision journaled under the previous, signature-derived name still counts, and resubmitting it
// (even re-signed) adds nothing.
func TestLegacyNamedDecisionStillCountsAndResubmitsAsNoop(t *testing.T) {
	ctx := context.Background()
	j, vf, base := pausedOne(t)
	d := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: append([]byte{1}, base...)}
	legacy := approvalDecisionStep("c1", "a", true, fakeAlg, FormatTimestamp(testDecidedAt), d.Signature)
	if _, err := j.do(ctx, "r", legacy, func(context.Context) (Record, error) {
		return Record{Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{
			Approver: "a", ApproverAlg: fakeAlg, Signature: d.Signature, DecidedAt: FormatTimestamp(testDecidedAt), RecordedAt: FormatTimestamp(testDecidedAt)}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	var charged int
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	if _, err := mofnRun(j, "r", false, pol, vf, &charged); err != nil || charged != 1 {
		t.Fatalf("the legacy-named decision does not count: %v charged %d", err, charged)
	}
	d2 := d
	d2.Signature = append([]byte{2}, base...)
	var wrote bool
	if err := SubmitDecision(ctx, j, d2, WithDecisionCheck(vf), ReportWritten(&wrote)); err != nil || wrote {
		t.Fatalf("resubmitting a legacy-named decision: err %v wrote %v", err, wrote)
	}
	if n := len(decisionRecords(t, j)); n != 1 {
		t.Fatalf("%d records", n)
	}
}

// A record under the decision's identity name that is not the approver's valid decision (a junk
// signature written without a check) must not take the name from the real decision: the real one is
// recorded under the signature-derived name, and counts.
func TestJunkAtTheIdentityNameDoesNotLockOutTheRealDecision(t *testing.T) {
	ctx := context.Background()
	j, vf, base := pausedOne(t)
	junk := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: []byte{9, 9, 9}}
	if err := SubmitDecision(ctx, j, junk); err != nil {
		t.Fatal(err)
	}
	real := junk
	real.Signature = append([]byte{1}, base...)
	var wrote bool
	if err := SubmitDecision(ctx, j, real, WithDecisionCheck(vf), ReportWritten(&wrote)); err != nil || !wrote {
		t.Fatalf("real decision: err %v wrote %v", err, wrote)
	}
	var charged int
	if _, err := mofnRun(j, "r", false, &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}, vf, &charged); err != nil || charged != 1 {
		t.Fatalf("the real decision does not count behind the junk: %v charged %d", err, charged)
	}
}
