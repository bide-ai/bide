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

	// The KYC machine has a repair, so its certificate says compensation_free=false.
	if got := govern.CertifyConvergence(rep, digest).CompensationFree; got {
		t.Fatalf("KYC machine must not be compensation-free, cert says %v", got)
	}

	// 1) No checker: the cryptographic root verifies (exit 0).
	if out, err := exec.Command(bin, base...).CombinedOutput(); err != nil {
		t.Fatalf("no-checker run should pass, got err %v\n%s", err, out)
	}

	// 2) Checker agrees on convergence AND classification: the command passes.
	agree := fakeChecker(t, dir, "agree", 0, "compensation_free=false")
	if out, err := exec.Command(bin, append(base, "-checker", agree)...).CombinedOutput(); err != nil {
		t.Fatalf("agreeing checker should pass, got err %v\n%s", err, out)
	}

	// 3) Checker disagrees on convergence (exit 1) while the certificate claims convergent: FAIL.
	disagree := fakeChecker(t, dir, "disagree", 1, "compensation_free=false")
	if out, err := exec.Command(bin, append(base, "-checker", disagree)...).CombinedOutput(); err == nil {
		t.Fatalf("disagreeing checker must fail the command, but it exited 0\n%s", out)
	}

	// 4) Checker agrees on convergence but reports a DIFFERENT classification than the certificate:
	// the command must FAIL, catching a certificate that overstates the CRDT-fragment claim.
	misclass := fakeChecker(t, dir, "misclass", 0, "compensation_free=true")
	if out, err := exec.Command(bin, append(base, "-checker", misclass)...).CombinedOutput(); err == nil {
		t.Fatalf("classification mismatch must fail the command, but it exited 0\n%s", out)
	}
}

// fakeChecker writes an executable script that ignores its argument, prints the given
// classification line, and exits with the given code, standing in for the external oracle
// (exit 0 = convergent, non-zero = not; the printed line is the compensation-free verdict).
func fakeChecker(t *testing.T, dir, name string, code int, line string) string {
	t.Helper()
	path := filepath.Join(dir, name+".sh")
	script := "#!/bin/sh\necho " + line + "\nexit " + map[int]string{0: "0", 1: "1"}[code] + "\n"
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

// TestVerifyQuorumCLI exercises verify-quorum end to end: a real quorum run produces vote and tally
// bundles, and the command confirms k-of-n agreement, recomputing the tally from the disclosed
// votes. It also checks the failure paths (threshold not met, and not all votes disclosed).
func TestVerifyQuorumCLI(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := agent.NewMemStore()
	const runID = "run1"

	// Three voters, two agree on "approve"; k = 2. govern.Quorum records each vote and the tally
	// as durable, provable Steps.
	decide := func(v string) func(context.Context) (string, error) {
		return func(context.Context) (string, error) { return v, nil }
	}
	res, err := govern.Quorum(ctx, store, runID, 2,
		govern.Voter{Name: "model-A", Decide: decide("approve")},
		govern.Voter{Name: "model-B", Decide: decide("approve")},
		govern.Voter{Name: "model-C", Decide: decide("deny")})
	if err != nil {
		t.Fatalf("Quorum: %v", err)
	}
	if res.Decision != "approve" || res.VotesFor != 2 || !res.Agreed {
		t.Fatalf("unexpected tally: %+v", res)
	}

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, err := audit.NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		t.Fatalf("NewTreeHead: %v", err)
	}
	sth := audit.SignTreeHead(th, priv)

	// Produce and write the bundles a verifier would receive.
	bundlePath := func(name, file string) string {
		pb, err := audit.ProveStep(ctx, store, runID, name, sth)
		if err != nil {
			t.Fatalf("ProveStep %s: %v", name, err)
		}
		p := filepath.Join(dir, file)
		writeJSON(t, p, pb)
		return p
	}
	tallyP := bundlePath("quorum/tally", "tally.json")
	aP := bundlePath("model-A", "a.json")
	bP := bundlePath("model-B", "b.json")
	cP := bundlePath("model-C", "c.json")
	pubHex := hex.EncodeToString(pub)

	bin := filepath.Join(dir, "goagents-audit")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, out)
	}

	// 1) Quorum met (k=2), all three votes disclosed: passes.
	pass := []string{"verify-quorum", "-tally", tallyP, "-vote", aP, "-vote", bP, "-vote", cP, "-pubkey", pubHex, "-k", "2"}
	if out, err := exec.Command(bin, pass...).CombinedOutput(); err != nil {
		t.Fatalf("quorum-met run should pass, got err %v\n%s", err, out)
	}

	// 2) Threshold not met (k=3 while votes_for=2): must FAIL.
	k3 := []string{"verify-quorum", "-tally", tallyP, "-vote", aP, "-vote", bP, "-vote", cP, "-pubkey", pubHex, "-k", "3"}
	if out, err := exec.Command(bin, k3...).CombinedOutput(); err == nil {
		t.Fatalf("k=3 with votes_for=2 must fail, but exited 0\n%s", out)
	}

	// 3) Not all votes disclosed (2 of 3) so the tally cannot be recomputed: must FAIL.
	partial := []string{"verify-quorum", "-tally", tallyP, "-vote", aP, "-vote", bP, "-pubkey", pubHex, "-k", "2"}
	if out, err := exec.Command(bin, partial...).CombinedOutput(); err == nil {
		t.Fatalf("partial vote disclosure must fail, but exited 0\n%s", out)
	}
}
