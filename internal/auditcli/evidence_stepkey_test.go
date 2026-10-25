package auditcli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/taintfixture"
)

// TestVerifyEvidenceStepKeyOfCall: verify-evidence of a gate's approval-pending step prints the call
// ID as the other verbs do ("c2#taint"), not its escaped form in the stored key ("c2%23taint"). The
// package itself, which the seal covers, still holds the escaped key.
func TestVerifyEvidenceStepKeyOfCall(t *testing.T) {
	ctx := context.Background()
	f := taintfixture.New(t, nil)
	if err := f.Drive(); err == nil {
		t.Fatal("the run did not pause at the escalation gate")
	}
	logSigner := audit.Ed25519Signer{Priv: f.LogPriv}
	store := f.Journal
	const runID = taintfixture.Run
	key := agent.ApprovalPendingStep("c2#taint")
	if key != "approval-pending:c2%23taint" {
		t.Fatalf("the pending step key is %q", key)
	}
	pkg, err := audit.Evidence(ctx, store, runID, logSigner, time.Now().Add(-time.Minute).UnixNano(), audit.WithStep(key))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "evidence.json")
	writeJSON(t, path, pkg)
	code, stdout, stderr := runCLI("verify-evidence", "-evidence", path, "-pubkey", keyText(logSigner))
	if code != 0 {
		t.Fatalf("verify-evidence: exit %d\n%s%s", code, stdout, stderr)
	}
	if want := `PASS  step "approval-pending" of call "c2#taint": step included in the signed log`; !strings.Contains(stdout, want) {
		t.Errorf("verify-evidence output lacks %q:\n%s", want, stdout)
	}
	if strings.Contains(stdout, "%23") {
		t.Errorf("verify-evidence output shows the escaped key:\n%s", stdout)
	}
	if pkg.Actions[0].Ref != key {
		t.Errorf("the package action is %q, want the stored key %q", pkg.Actions[0].Ref, key)
	}
}

// TestStepSubjectEveryCallKey: each key the engine builds from a call ID and a journal can hold in
// evidence is named by the ID; the notification keys that share the "approval:" prefix hold a run ID
// before the call ID and are never named as a call.
func TestStepSubjectEveryCallKey(t *testing.T) {
	const id = "c2#taint"
	for name, key := range map[string]string{
		"approval":         "approval:c2%23taint",
		"after request":    "approval:c2%23taint:after-request",
		"pending":          agent.ApprovalPendingStep(id),
		"request":          agent.ApprovalRequestStep(id),
		"deadline":         agent.ApprovalDeadlineStep(id),
		"tally":            agent.ApprovalTallyStep(id),
		"retally":          agent.ApprovalRetallyStep(id),
		"sod retally":      agent.ApprovalSoDRetallyStep(id),
		"ident tally":      agent.ApprovalIdentTallyStep(id, 0),
		"ident reask tall": agent.ApprovalIdentTallyStep(id, 2),
		"reask":            agent.ApprovalReaskStep(id, false),
		"sod reask":        agent.ApprovalReaskStep(id, true),
		"ident reask":      agent.ApprovalIdentReaskStep(id, 1),
		"exec hold":        agent.ApprovalExecHoldStep(id, 1),
		"identity":         agent.ApprovalIdentityStep(id, 1),
		"identity digest":  agent.ApprovalIdentityDigestStep(id, 1, strings.Repeat("ab", 16)),
		"pause":            agent.ApprovalPauseStep(id, 3),
		"decision":         agent.DecisionStep(agent.Decision{ToolUseID: id, ApproverID: "alice", Approved: true, Alg: "ed25519", DecidedAt: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}),
	} {
		if !strings.Contains(key, "%23") {
			t.Errorf("%s: key %q does not hold the escaped ID; the test is not testing it", name, key)
		}
		if got := stepSubject(key); !strings.HasSuffix(got, ` of call "c2#taint"`) || strings.Contains(got, "%23") {
			t.Errorf("%s: %q is shown as %s", name, key, got)
		}
	}
	for name, key := range map[string]string{
		"first":       agent.ApprovalKey("run#x", id, 1),
		"reminder":    agent.ApprovalKey("run#x", id, 0),
		"reask":       agent.ApprovalReaskKey("run#x", id, true),
		"ident reask": agent.ApprovalIdentReaskKey("run#x", id, 2),
	} {
		if got, want := stepSubject(key), fmt.Sprintf("step %q", key); got != want {
			t.Errorf("notification key %s: shown as %s, want the stored key %s", name, got, want)
		}
	}
}

// TestDecisionLabelUnsigned: a decision recorded without a signature is labelled by its step, named
// as a step of a call is.
func TestDecisionLabelUnsigned(t *testing.T) {
	step := "approval:c2%23taint:alice:" + strings.Repeat("0a", 32)
	got := decisionLabel(audit.DecisionCoverage{Step: step})
	if want := `(no approver signature) step "approval:alice:` + strings.Repeat("0a", 32) + `" of call "c2#taint"`; got != want {
		t.Errorf("decisionLabel = %s, want %s", got, want)
	}
}
