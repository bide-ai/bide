package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	gsm "github.com/blackwell-systems/gsm"
	agent "github.com/dayna/go-agents"
	"github.com/dayna/go-agents/audit"
	"github.com/dayna/go-agents/govern"
)

// TestVerifyConvergenceCLI exercises the verify-convergence command end to end, including the
// adversarial cross-check: with a checker whose verdict disagrees with the certificate, the command
// must FAIL, so a certificate that overstates convergence cannot pass. A fake checker script stands
// in for the real Coq oracle so the agreement/disagreement logic is tested without the toolchain.
func TestVerifyConvergenceCLI(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// A real convergent policy: an approval reverted when the case is flagged.
	r := gsm.NewRegistry("kyc-decision")
	approved := r.Bool("approved")
	flag := r.Bool("flagged")
	r.Rule("no_approve_when_flagged").
		Require(gsm.Or(gsm.Is(approved, 0), gsm.Is(flag, 0))).
		RepairWith(gsm.SetTo(approved, 0)).
		Add()
	r.On("flag").Does(gsm.SetTo(flag, 1)).Add()
	r.On("approve").Does(gsm.SetTo(approved, 1)).Add()
	_, rep, err := r.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	digest, _ := r.PolicyDigest()
	policyBytes, _ := r.PolicyBytes()

	// Anchor the policy and its convergence certificate, then sign a tree head and prove both.
	store := agent.NewMemStore()
	const runID = "run1"
	if _, err := audit.RecordPolicy(ctx, store, runID, policyBytes, digest); err != nil {
		t.Fatalf("RecordPolicy: %v", err)
	}
	certBytes, _ := govern.CertifyConvergence(rep, digest).Marshal()
	if _, err := audit.RecordConvergence(ctx, store, runID, certBytes, digest); err != nil {
		t.Fatalf("RecordConvergence: %v", err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, err := audit.NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		t.Fatalf("NewTreeHead: %v", err)
	}
	sth := audit.SignTreeHead(th, priv)

	policyBundle, err := audit.ProvePolicy(ctx, store, runID, digest, sth)
	if err != nil {
		t.Fatalf("ProvePolicy: %v", err)
	}
	certBundle, err := audit.ProveConvergence(ctx, store, runID, digest, sth)
	if err != nil {
		t.Fatalf("ProveConvergence: %v", err)
	}

	policyPath := filepath.Join(dir, "policy-bundle.json")
	certPath := filepath.Join(dir, "cert-bundle.json")
	writeJSON(t, policyPath, policyBundle)
	writeJSON(t, certPath, certBundle)
	pubHex := hex.EncodeToString(pub)

	// Build the CLI binary once.
	bin := filepath.Join(dir, "goagents-audit")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, out)
	}

	base := []string{"verify-convergence", "-cert-bundle", certPath, "-policy-bundle", policyPath, "-pubkey", pubHex}

	// 1) No checker: the cryptographic root verifies (exit 0).
	if out, err := exec.Command(bin, base...).CombinedOutput(); err != nil {
		t.Fatalf("no-checker run should pass, got err %v\n%s", err, out)
	}

	// 2) Checker agrees (exit 0 => converges): the command passes.
	agree := fakeChecker(t, dir, "agree", 0)
	if out, err := exec.Command(bin, append(base, "-checker", agree)...).CombinedOutput(); err != nil {
		t.Fatalf("agreeing checker should pass, got err %v\n%s", err, out)
	}

	// 3) Checker disagrees (exit 1 => not convergent) while the certificate claims convergent:
	// the command must FAIL, catching a certificate that overstates convergence.
	disagree := fakeChecker(t, dir, "disagree", 1)
	out, err := exec.Command(bin, append(base, "-checker", disagree)...).CombinedOutput()
	if err == nil {
		t.Fatalf("disagreeing checker must fail the command, but it exited 0\n%s", out)
	}
}

// fakeChecker writes an executable script that ignores its argument and exits with the given code,
// standing in for the external oracle (exit 0 = convergent, non-zero = not).
func fakeChecker(t *testing.T, dir, name string, code int) string {
	t.Helper()
	path := filepath.Join(dir, name+".sh")
	script := "#!/bin/sh\nexit " + map[int]string{0: "0", 1: "1"}[code] + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake checker: %v", err)
	}
	return path
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
