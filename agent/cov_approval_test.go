package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// SubmitDecision refuses, with ErrConfig and before recording anything, a decision that lacks a
// field every signed decision carries, or whose approver or time no reader could accept.
func TestSubmitDecision_RefusesMalformedDecisions(t *testing.T) {
	ctx := context.Background()
	ok := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: []byte("s")}
	with := func(f func(*Decision)) Decision { d := ok; f(&d); return d }
	for name, d := range map[string]Decision{
		"no run":              with(func(d *Decision) { d.RunID = "" }),
		"no tool use":         with(func(d *Decision) { d.ToolUseID = "" }),
		"no approver":         with(func(d *Decision) { d.ApproverID = "" }),
		"approver with space": with(func(d *Decision) { d.ApproverID = "a b" }),
		"approver with ctrl":  with(func(d *Decision) { d.ApproverID = "a\nb" }),
		"approver non-ASCII":  with(func(d *Decision) { d.ApproverID = "é" }),
		"no scheme":           with(func(d *Decision) { d.Alg = "" }),
		"no signature":        with(func(d *Decision) { d.Signature = nil }),
		"no decision time":    with(func(d *Decision) { d.DecidedAt = time.Time{} }),
		"time beyond year 9999": with(func(d *Decision) {
			d.DecidedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		}),
	} {
		j := memJournal()
		if err := SubmitDecision(ctx, j, d); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: SubmitDecision = %v; want ErrConfig", name, err)
		}
		if recs, _ := j.History(ctx, "r"); len(recs) != 0 {
			t.Errorf("%s: a refused decision recorded %d records", name, len(recs))
		}
	}
	// A nil option is skipped.
	j := memJournal()
	if err := SubmitDecision(ctx, j, ok, nil); err != nil {
		t.Errorf("SubmitDecision with a nil option = %v", err)
	}
}

// WithDecisionCheck tells an approver at submission that a decision will not count, and records
// nothing: an unknown call, an approver with no key (or a nil verifier), a scheme other than the
// key's, a bad signature, a store that cannot be read. The same decision without the check is
// recorded, and the gate ignores it.
func TestSubmitDecision_DecisionCheckRefusals(t *testing.T) {
	ctx := context.Background()
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	vf := fakeVerifiers("a")
	newPaused := func(t *testing.T) *Journal {
		t.Helper()
		j := memJournal()
		var charged int
		if _, err := mofnRun(j, "r", true, pol, vf, &charged); err == nil {
			t.Fatal("run did not pause")
		}
		return j
	}
	sign := func(t *testing.T, j *Journal, approver string) Decision {
		t.Helper()
		sig := fakeSign(approver, ApprovalDecisionBytes(subjectOf(t, j, "r", "c1"), approver, true, testDecidedAt))
		return Decision{RunID: "r", ToolUseID: "c1", ApproverID: approver, Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: sig}
	}
	nilVerifier := func(string) (ApproverVerifier, bool) { return (*fakeVerifier)(nil), true }

	cases := []struct {
		name  string
		check ApproverVerifierFor
		edit  func(*Decision)
		want  error
	}{
		{"unknown call", vf, func(d *Decision) { d.ToolUseID = "c9" }, ErrInvalidApproval},
		{"approver with no key", fakeVerifiers("other"), nil, ErrInvalidApproval},
		{"nil verifier", nilVerifier, nil, ErrConfig},
		{"other scheme", vf, func(d *Decision) { d.Alg = "other" }, ErrInvalidApproval},
		{"bad signature", vf, func(d *Decision) { d.Signature = []byte("forged") }, ErrInvalidApproval},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			j := newPaused(t)
			d := sign(t, j, "a")
			if c.edit != nil {
				c.edit(&d)
			}
			before, _ := j.History(ctx, "r")
			if err := SubmitDecision(ctx, j, d, WithDecisionCheck(c.check)); !errors.Is(err, c.want) {
				t.Fatalf("SubmitDecision = %v; want %v", err, c.want)
			}
			after, _ := j.History(ctx, "r")
			if len(after) != len(before) {
				t.Fatalf("a refused decision recorded %d records", len(after)-len(before))
			}
		})
	}
	t.Run("history unreadable", func(t *testing.T) {
		d := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: []byte("s")}
		j := mustJournal(loadFails{NewMemStore()})
		if err := SubmitDecision(ctx, j, d, WithDecisionCheck(vf)); !errors.Is(err, ErrStorage) {
			t.Fatalf("SubmitDecision = %v; want ErrStorage", err)
		}
	})
	t.Run("without the check a forged decision is recorded and ignored", func(t *testing.T) {
		j := newPaused(t)
		d := sign(t, j, "a")
		d.Signature = []byte("forged")
		if err := SubmitDecision(ctx, j, d); err != nil {
			t.Fatalf("SubmitDecision = %v", err)
		}
		var charged int
		_, err := mofnRun(j, "r", false, pol, vf, &charged)
		wantPending(t, err, counts{Need: 1, Pending: []string{"a"}})
		if charged != 0 {
			t.Fatal("the tool ran on a forged decision")
		}
	})
	t.Run("an approver's second decision is refused", func(t *testing.T) {
		j := newPaused(t)
		first := sign(t, j, "a")
		if err := SubmitDecision(ctx, j, first, WithDecisionCheck(vf)); err != nil {
			t.Fatal(err)
		}
		if err := SubmitDecision(ctx, j, first, WithDecisionCheck(vf)); err != nil {
			t.Fatalf("resubmitting the identical decision = %v; want a no-op", err)
		}
		later := first
		later.Approved = false
		later.Signature = fakeSign("a", ApprovalDecisionBytes(subjectOf(t, j, "r", "c1"), "a", false, testDecidedAt))
		if err := SubmitDecision(ctx, j, later, WithDecisionCheck(vf)); !errors.Is(err, ErrAlreadyDecided) {
			t.Fatalf("a changed decision = %v; want ErrAlreadyDecided", err)
		}
	})
}
