package bideaudit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/govern"
	gsm "github.com/blackwell-systems/gsm"
)

// checkerVerbs anchors a convergent policy, its convergence certificate, and a governed action
// under it, and returns, for each verb that runs -checker, a command line that verifies (exit 0)
// with an agreeing checker appended.
func checkerVerbs(t *testing.T, dir string) map[string][]string {
	t.Helper()
	ctx := context.Background()
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
		t.Fatal(err)
	}
	digest, _ := r.PolicyDigest()
	policyBytes, _ := r.PolicyBytes()
	certBytes, _ := govern.CertifyConvergence(rep, digest).Marshal()

	store := agent.NewMemStore()
	const runID = "run1"
	if _, err := audit.RecordPolicy(ctx, store, runID, policyBytes, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.RecordConvergence(ctx, store, runID, certBytes, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Do(ctx, runID, "action", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "call_1",
			Result: []byte(`{"event":"approve","applied":true,"policy_digest":"` + digest + `","state_digest":"abc"}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, err := audit.NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	sth := audit.SignTreeHead(th, priv)
	policyBundle, err := audit.ProvePolicy(ctx, store, runID, digest, sth)
	if err != nil {
		t.Fatal(err)
	}
	certBundle, err := audit.ProveConvergence(ctx, store, runID, digest, sth)
	if err != nil {
		t.Fatal(err)
	}
	actionBundle, err := audit.ProveToolCall(ctx, store, runID, "call_1", sth)
	if err != nil {
		t.Fatal(err)
	}
	runCert, err := audit.CertifyRun(ctx, store, runID, sth, audit.RunCertSpec{ApprovedPolicies: []string{digest}}, priv, 2)
	if err != nil {
		t.Fatal(err)
	}
	path := func(name string, v any) string {
		p := filepath.Join(dir, name)
		writeJSON(t, p, v)
		return p
	}
	policyFile := filepath.Join(dir, "policy.machine")
	if err := os.WriteFile(policyFile, policyBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(pub)
	policyP := path("policy-bundle.json", policyBundle)
	return map[string][]string{
		"verify-governance":      {"verify-governance", "-policy", policyFile, "-digest", digest},
		"verify-governed-action": {"verify-governed-action", "-action", path("action.json", actionBundle), "-policy-bundle", policyP, "-pubkey", pubHex},
		"verify-convergence":     {"verify-convergence", "-cert-bundle", path("cert-bundle.json", certBundle), "-policy-bundle", policyP, "-pubkey", pubHex},
		"verify-run":             {"verify-run", "-cert", path("runcert.json", runCert), "-pubkey", pubHex, "-approved", digest},
	}
}

// A checker that gives no verdict is not a verdict that the policy does not converge. The
// astchecker exits 0 for convergent, 1 for not convergent, and 2 for a usage or parse error (as
// does any uncaught OCaml exception). So a checker that cannot be started, exits with any other
// status, is killed by a signal, or prints a compensation_free line that is not exactly true or
// false (or both) is an error: the verb exits 3 and prints an ERROR line, never a FAIL verdict
// about convergence and never a claim that the oracle agrees.
func TestCLI_CheckerWithoutVerdictIsAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stand-in checkers are POSIX shell scripts")
	}
	dir := t.TempDir()
	bin := buildCLI(t, dir)
	verbs := checkerVerbs(t, dir)
	script := func(name, body string) string {
		p := filepath.Join(dir, name+".sh")
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	agree := script("agree", "echo compensation_free=false; exit 0")
	for verb, args := range verbs {
		if code, out := exitCode(t, bin, append(args, "-checker", agree)...); code != 0 {
			t.Fatalf("%s with an agreeing checker exited %d, want 0:\n%s", verb, code, out)
		}
	}
	broken := map[string]string{
		"a checker that cannot be started": filepath.Join(dir, "no-such-checker"),
		"a parse error (exit 2)":           script("parse-error", "echo 'parse error: line 1' >&2; exit 2"),
		"an unknown exit status":           script("exit-7", "echo compensation_free=false; exit 7"),
		"a checker killed by a signal":     script("killed", "kill -9 $$"),
		"an unparsable classification":     script("maybe", "echo compensation_free=maybe; exit 0"),
		"contradictory classifications":    script("both", "echo compensation_free=true; echo compensation_free=false; exit 0"),
	}
	for verb, args := range verbs {
		for name, checker := range broken {
			code, out := exitCode(t, bin, append(args, "-checker", checker)...)
			if code != 3 || !strings.Contains(out, "ERROR:") {
				t.Errorf("%s, %s: exited %d, want 3 with an ERROR line:\n%s", verb, name, code, out)
			}
			for _, verdict := range []string{"FAIL", "agree", "converges=false", "did not certify"} {
				if strings.Contains(out, verdict) {
					t.Errorf("%s, %s: output reads as a verdict (%q):\n%s", verb, name, verdict, out)
				}
			}
		}
	}
}
