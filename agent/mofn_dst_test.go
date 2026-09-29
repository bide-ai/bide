package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// Deterministic Simulation Testing of the m-of-n approval gate. The guarantees under test:
// each approver's decision counts at most once (their first valid decision counts; later ones
// are superseded, and invalid ones never take their place), the gate's
// outcome is a pure function of the journal so any number of resumes reach the same result,
// decisions that land between resumes are tallied exactly, and a crash at any write after
// quorum never fires the gated side effect twice.

var opsFinRisk = []string{"ops", "finance", "risk"}

func mofnDSTPolicy() *ApprovalPolicy {
	return &ApprovalPolicy{Need: 2, Approvers: opsFinRisk}
}

// mofnHistory returns the full journal for runID.
func mofnHistory(t *testing.T, store Durable, runID string) []Record {
	t.Helper()
	recs, err := store.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// countSteps reports how many journal records for runID are named name.
func countSteps(t *testing.T, store Durable, runID, name string) int {
	t.Helper()
	n := 0
	for _, r := range mofnHistory(t, store, runID) {
		if r.Name == name {
			n++
		}
	}
	return n
}

// countDecisions reports how many decision records approver has on toolUseID.
func countDecisions(t *testing.T, store Durable, runID, toolUseID, approver string) int {
	t.Helper()
	n := 0
	for _, r := range mofnHistory(t, store, runID) {
		if IsApprovalDecision(r, toolUseID) && r.Approver == approver {
			n++
		}
	}
	return n
}

// journaledTally reads the final approval-tally step back from the journal. Step returns the
// recorded value without running fn, so a missing record fails the test.
func journaledTally(t *testing.T, store Durable, runID, toolUseID string) ApprovalTally {
	t.Helper()
	got, err := Step(context.Background(), store, runID, "approval-tally:"+toolUseID, func(context.Context) (ApprovalTally, error) {
		return ApprovalTally{}, errors.New("approval tally not journaled")
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The same approver deciding twice (with opposite verdicts) is counted once, and the first
// valid decision wins: in the pure tally, at the pause, and at the terminal gate. Each
// distinct decision is its own record; an identical resubmission is not.
func TestMofnDST_AtMostOncePerApprover(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	pol := mofnDSTPolicy()
	vf := fakeVerifiers(opsFinRisk...)
	var charged int

	_, err := mofnRun(store, "r1", true, pol, vf, &charged)
	wantPending(t, err, counts{Need: 2, Pending: []string{"ops", "finance", "risk"}})

	// ops approves, then tries to flip to a (validly signed) denial.
	approveAs(t, store, "r1", "c1", "ops", true)
	approveAs(t, store, "r1", "c1", "ops", false)

	if n := countDecisions(t, store, "r1", "c1", "ops"); n != 2 {
		t.Fatalf("journal holds %d decision records by ops, want 2 (one per distinct decision)", n)
	}

	want := counts{Need: 2, Approved: 1, ApprovedBy: []string{"ops"}, Pending: []string{"finance", "risk"}}
	got, checks := TallyApprovals(mofnHistory(t, store, "r1"), subjectOf(t, store, "r1", "c1"), *pol, vf)
	if c := countsOf(got); !reflect.DeepEqual(c, want) {
		t.Fatalf("TallyApprovals = %+v, want %+v", c, want)
	}
	if len(checks) != 2 || !checks[0].Counted || !checks[0].Approved || checks[1].Reason != ReasonSuperseded {
		t.Fatalf("checks = %+v, want ops' approval counted and the flip superseded", checks)
	}
	_, err = mofnRun(store, "r1", false, pol, vf, &charged)
	wantPending(t, err, want)
	if charged != 0 {
		t.Fatalf("charge ran %d times below quorum, want 0", charged)
	}

	// A second, independent approver reaches Need only because ops' first decision stood.
	approveAs(t, store, "r1", "c1", "finance", true)
	out, err := mofnRun(store, "r1", false, pol, vf, &charged)
	if err != nil {
		t.Fatalf("run at quorum: %v", err)
	}
	if textOf(out) != "done" || charged != 1 {
		t.Fatalf("out=%q charged=%d, want done/1", textOf(out), charged)
	}
	wantFinal := counts{Need: 2, Approved: 2, ApprovedBy: []string{"ops", "finance"}, Pending: []string{"risk"}}
	if got := countsOf(journaledTally(t, store, "r1", "c1")); !reflect.DeepEqual(got, wantFinal) {
		t.Fatalf("journaled tally = %+v, want %+v", got, wantFinal)
	}

	// Resubmitting ops' identical denial after the gate resolved maps to the same record: no-op.
	sig := fakeSign("ops", ApprovalDecisionBytes(subjectOf(t, store, "r1", "c1"), "ops", false))
	if err := ApproveAs(ctx, store, "r1", "c1", "ops", false, sig); err != nil {
		t.Fatal(err)
	}
	if n := countDecisions(t, store, "r1", "c1", "ops"); n != 2 {
		t.Fatalf("journal holds %d decision records by ops after an identical resubmission, want 2", n)
	}
}

// Once the gate proceeds, further resumes with the same runID replay it: the tool does not
// re-run, the tally step is not doubled, and the journal is unchanged. The
// journaled tally is authoritative, so the outcome holds even if the verifier set later
// changes and would no longer verify the recorded signatures.
func TestMofnDST_DeterministicAcrossResume(t *testing.T) {
	store := NewMemStore()
	pol := mofnDSTPolicy()
	vf := fakeVerifiers(opsFinRisk...)
	var charged int

	_, err := mofnRun(store, "r1", true, pol, vf, &charged)
	wantPending(t, err, counts{Need: 2, Pending: []string{"ops", "finance", "risk"}})
	approveAs(t, store, "r1", "c1", "ops", true)
	approveAs(t, store, "r1", "c1", "risk", true)

	first, err := mofnRun(store, "r1", false, pol, vf, &charged)
	if err != nil {
		t.Fatalf("resume at quorum: %v", err)
	}
	if textOf(first) != "done" || charged != 1 {
		t.Fatalf("out=%q charged=%d, want done/1", textOf(first), charged)
	}
	wantTally := counts{Need: 2, Approved: 2, ApprovedBy: []string{"ops", "risk"}, Pending: []string{"finance"}}
	if got := countsOf(journaledTally(t, store, "r1", "c1")); !reflect.DeepEqual(got, wantTally) {
		t.Fatalf("journaled tally = %+v, want %+v", got, wantTally)
	}
	result, ok := hasStep(t, store, "r1", "c1")
	if !ok || result.Kind != StepToolResult || result.IsError {
		t.Fatalf("tool result c1 = %+v (found %v), want a successful StepToolResult", result, ok)
	}
	before := mofnHistory(t, store, "r1")

	resumes := []struct {
		name string
		vf   ApproverVerifierFor
	}{
		{"same-verifiers", vf},
		{"same-verifiers-again", vf},
		// No approver resolves: a re-tally would count zero, so this proves the replay reuses
		// the journaled decision rather than recomputing it.
		{"verifiers-revoked", fakeVerifiers()},
	}
	for _, r := range resumes {
		if ok, err := IsComplete(context.Background(), store, "r1"); err != nil || !ok {
			t.Fatalf("%s: IsComplete = %v, %v; want true before the redundant resume", r.name, ok, err)
		}
		out, err := mofnRun(store, "r1", false, pol, r.vf, &charged)
		if err != nil {
			t.Fatalf("%s: redundant resume: %v", r.name, err)
		}
		if textOf(out) != textOf(first) {
			t.Fatalf("%s: final answer = %q, want %q", r.name, textOf(out), textOf(first))
		}
		if charged != 1 {
			t.Fatalf("%s: charge ran %d times across resumes, want 1", r.name, charged)
		}
		// A finished run is final: re-driving it returns the recorded answer and appends
		// nothing, not even a model turn.
		if after := mofnHistory(t, store, "r1"); !reflect.DeepEqual(after, before) {
			t.Fatalf("%s: a redundant resume changed the journal:\nbefore=%+v\nafter =%+v", r.name, before, after)
		}
		for _, name := range []string{"approval-tally:c1", "c1", runCompleteStep} {
			if n := countSteps(t, store, "r1", name); n != 1 {
				t.Fatalf("%s: %d %q records, want exactly 1", r.name, n, name)
			}
		}
		if got := countsOf(journaledTally(t, store, "r1", "c1")); !reflect.DeepEqual(got, wantTally) {
			t.Fatalf("%s: journaled tally = %+v, want %+v", r.name, got, wantTally)
		}
	}
}

// A decision landing between two resumes: the intermediate pause reports the exact running
// tally and journals nothing final; the next decision tips the gate and the run proceeds.
func TestMofnDST_InterleavedDecisions(t *testing.T) {
	store := NewMemStore()
	pol := mofnDSTPolicy()
	vf := fakeVerifiers(opsFinRisk...)
	var charged int

	_, err := mofnRun(store, "r1", true, pol, vf, &charged)
	wantPending(t, err, counts{Need: 2, Pending: []string{"ops", "finance", "risk"}})

	approveAs(t, store, "r1", "c1", "finance", true)
	_, err = mofnRun(store, "r1", false, pol, vf, &charged)
	wantPending(t, err, counts{Need: 2, Approved: 1, ApprovedBy: []string{"finance"}, Pending: []string{"ops", "risk"}})
	// Resuming again with no new decision is stable: same pause, same tally.
	_, err = mofnRun(store, "r1", false, pol, vf, &charged)
	wantPending(t, err, counts{Need: 2, Approved: 1, ApprovedBy: []string{"finance"}, Pending: []string{"ops", "risk"}})
	if charged != 0 {
		t.Fatalf("charge ran %d times below quorum, want 0", charged)
	}
	if _, ok := hasStep(t, store, "r1", "approval-tally:c1"); ok {
		t.Fatal("a pending pause journaled an approval tally; want only the terminal tally")
	}
	if _, ok := hasStep(t, store, "r1", "c1"); ok {
		t.Fatal("a pending pause recorded a tool result")
	}

	// A denial lands next: still reachable (3 - 1 >= 2), so the gate stays paused.
	approveAs(t, store, "r1", "c1", "ops", false)
	_, err = mofnRun(store, "r1", false, pol, vf, &charged)
	wantPending(t, err, counts{Need: 2, Approved: 1, Denied: 1, ApprovedBy: []string{"finance"}, DeniedBy: []string{"ops"}, Pending: []string{"risk"}})

	approveAs(t, store, "r1", "c1", "risk", true)
	out, err := mofnRun(store, "r1", false, pol, vf, &charged)
	if err != nil {
		t.Fatalf("resume at quorum: %v", err)
	}
	if textOf(out) != "done" || charged != 1 {
		t.Fatalf("out=%q charged=%d, want done/1", textOf(out), charged)
	}
	want := counts{Need: 2, Approved: 2, Denied: 1, ApprovedBy: []string{"finance", "risk"}, DeniedBy: []string{"ops"}}
	if got := countsOf(journaledTally(t, store, "r1", "c1")); !reflect.DeepEqual(got, want) {
		t.Fatalf("journaled tally = %+v, want %+v", got, want)
	}
}

// Crash at every write the resume performs once quorum is met. The gated tool is
// non-idempotent, so it must fire at most once under every crash schedule, the run must end
// completed or in ResumeHalt, and the final tally must be journaled exactly once with the
// same value regardless of where the crash landed.
func TestMofnDST_CrashSweepAfterQuorum(t *testing.T) {
	pol := mofnDSTPolicy()
	vf := fakeVerifiers(opsFinRisk...)
	wantTally := counts{Need: 2, Approved: 2, ApprovedBy: []string{"ops", "finance"}, Pending: []string{"risk"}}
	resume := func(store Durable, charged *int) error {
		charge := &countingTool{name: "charge", safety: Safety{Approval: pol}, calls: charged}
		a := New(&scriptModel{turns: [][]Emit{textTurn("done")}}, store, charge).WithApproverVerifiers(vf)
		_, err := a.Run(context.Background(), "r1", "pay")
		return err
	}

	haltSeen := false
	for crashAt := 1; crashAt <= 32; crashAt++ {
		mem := NewMemStore()
		var charged int
		_, err := mofnRun(mem, "r1", true, pol, vf, &charged)
		wantPending(t, err, counts{Need: 2, Pending: []string{"ops", "finance", "risk"}})
		approveAs(t, mem, "r1", "c1", "ops", true)
		approveAs(t, mem, "r1", "c1", "finance", true)

		err = resume(&crashStore{inner: mem, crashAt: crashAt}, &charged)
		crashed := errors.Is(err, errCrash)
		for errors.Is(err, errCrash) {
			err = resume(mem, &charged)
		}

		if charged > 1 {
			t.Fatalf("crashAt=%d: charge fired %d times, want at most 1", crashAt, charged)
		}
		var halt *ResumeHalt
		switch {
		case err == nil:
			if charged != 1 {
				t.Fatalf("crashAt=%d: completed run charged %d times, want 1", crashAt, charged)
			}
		case errors.As(err, &halt):
			haltSeen = true // the effect fired but its result was lost: halted, not retried
		default:
			t.Fatalf("crashAt=%d: unexpected terminal error: %v", crashAt, err)
		}
		if n := countSteps(t, mem, "r1", "approval-tally:c1"); n != 1 {
			t.Fatalf("crashAt=%d: %d approval-tally:c1 records, want exactly 1", crashAt, n)
		}
		if got := countsOf(journaledTally(t, mem, "r1", "c1")); !reflect.DeepEqual(got, wantTally) {
			t.Fatalf("crashAt=%d: journaled tally = %+v, want %+v", crashAt, got, wantTally)
		}
		for _, id := range []string{"ops", "finance"} {
			if n := countDecisions(t, mem, "r1", "c1", id); n != 1 {
				t.Fatalf("crashAt=%d: %d decision records by %s, want 1", crashAt, n, id)
			}
		}

		if !crashed { // crashAt exceeded the resume's write count: sweep complete
			break
		}
	}
	if !haltSeen {
		t.Fatal("no crash point exercised ResumeHalt; the halt path was never tested")
	}
}
