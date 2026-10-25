package auditcli

import (
	"context"
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
