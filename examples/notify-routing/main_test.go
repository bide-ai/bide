package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

func TestHealthyStoryPassesAndTamperingFails(t *testing.T) {
	var out bytes.Buffer
	o, err := story(&out, false)
	if err != nil {
		t.Fatal(err)
	}
	if o.err != nil || len(o.report.Problems) != 0 {
		t.Fatalf("the healthy story: %v, %v", o.err, o.report.Problems)
	}
	// Each approver's channel got the approval; on-call got the halt and then its all-clear; the
	// lead was never needed.
	for _, want := range []string{"risk     received: approval_pending", "finance  received: approval_pending", "oncall   received: outcome_unknown, resolved", "lead     received: \n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	rep, err := audit.VerifyNotifications(withoutFirstRecord(o.in, agent.NotifyTrailName(approvalRun)))
	if !errors.Is(err, audit.ErrNotVerified) || len(rep.Problems) == 0 {
		t.Fatalf("a trail with a record removed verified: %v, %v", err, rep.Problems)
	}
}

func TestDownReceiverEscalatesAndTheAuditSaysWhenOnCallWasLate(t *testing.T) {
	var out bytes.Buffer
	o, err := story(&out, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "lead     received: escalated, resolved") {
		t.Errorf("the lead was not told:\n%s", out.String())
	}
	if o.err == nil || !strings.Contains(strings.Join(o.report.Problems, "\n"), "acked_late") {
		t.Fatalf("on-call's late acknowledgement was not reported: %v, %v", o.err, o.report.Problems)
	}
	if len(o.report.Problems) != 1 {
		t.Errorf("problems = %v, want only the late acknowledgement", o.report.Problems)
	}
}

func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(&out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"audit: PASS, 4 events", "removed: FAIL", "audit: FAIL, 1 problem(s), the first: outcome_unknown"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
