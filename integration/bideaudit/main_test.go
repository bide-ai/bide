package bideaudit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/govern"
	gsm "github.com/blackwell-systems/gsm"
)

// cliPkg is the CLI these tests build and run: the core module's bide-audit, unchanged.
const cliPkg = "github.com/bide-ai/bide/cmd/bide-audit"

// auditBin is the path to build the bide-audit test binary to. On Windows, go
// build writes (and exec expects) a .exe suffix, so add it there.
func auditBin(dir string) string {
	bin := filepath.Join(dir, "bide-audit")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	return bin
}

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
	sth := must(audit.SignTreeHead(th, audit.Ed25519Signer{Priv: priv}))

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
	bin := auditBin(dir)
	if out, err := exec.Command("go", "build", "-o", bin, cliPkg).CombinedOutput(); err != nil {
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
// (exit 0 = convergent, 1 = not, as the astchecker does; the printed line is the compensation-free
// verdict).
func fakeChecker(t *testing.T, dir, name string, code int, line string) string {
	t.Helper()
	// The stand-in oracle is a POSIX shell script, which Windows cannot exec
	// directly (a real Windows user would supply a .exe astchecker). The CLI's
	// checker-invocation path is platform-neutral and covered on Linux/macOS;
	// the cryptographic assertions before this point still run on Windows.
	if runtime.GOOS == "windows" {
		t.Skip("external-oracle cross-check uses a POSIX shell script; skipped on Windows")
	}
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

// TestVerifyRunCLI exercises the verify-run command end to end: a run with a governed action under
// an approved, convergence-certified policy produces a RunCertificate that the command verifies,
// including the oracle cross-check. It covers the failure paths: a policy not in the auditor's
// allowlist FAILS only-approved-policies, and an oracle whose verdict disagrees with a policy's
// certificate FAILS. A fake checker stands in for the real Coq oracle.
func TestVerifyRunCLI(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := agent.NewMemStore()
	const runID = "run1"

	// A real convergent policy, anchored with its certificate, exercised by one governed action.
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
	certBytes, _ := govern.CertifyConvergence(rep, digest).Marshal()

	if _, err := audit.RecordPolicy(ctx, store, runID, policyBytes, digest); err != nil {
		t.Fatalf("RecordPolicy: %v", err)
	}
	if _, err := audit.RecordConvergence(ctx, store, runID, certBytes, digest); err != nil {
		t.Fatalf("RecordConvergence: %v", err)
	}
	// A governed-action leaf: a completed tool call whose result embeds the policy digest, as
	// an attested govern.EventTool journals.
	if _, err := store.Do(ctx, runID, "action", func(context.Context) (agent.Record, error) {
		return agent.Record{
			Kind:      agent.StepToolResult,
			ToolUseID: "call_1",
			Result:    []byte(`{"event":"approve","applied":true,"policy_digest":"` + digest + `","state_digest":"abc"}`),
		}, nil
	}); err != nil {
		t.Fatalf("record action: %v", err)
	}

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, err := audit.NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		t.Fatalf("NewTreeHead: %v", err)
	}
	sth := must(audit.SignTreeHead(th, audit.Ed25519Signer{Priv: priv}))

	cert, err := audit.CertifyRun(ctx, store, runID, sth, audit.RunCertSpec{ApprovedPolicies: []string{digest}, Signer: audit.Ed25519Signer{Priv: priv}, TimestampNanos: 2})
	if err != nil {
		t.Fatalf("CertifyRun: %v", err)
	}
	certPath := filepath.Join(dir, "runcert.json")
	writeJSON(t, certPath, cert)
	pubHex := hex.EncodeToString(pub)

	bin := auditBin(dir)
	if out, err := exec.Command("go", "build", "-o", bin, cliPkg).CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, out)
	}

	base := []string{"verify-run", "-cert", certPath, "-pubkey", pubHex, "-approved", digest}

	// 1) No checker, policy approved: the cryptographic root verifies (exit 0).
	if out, err := exec.Command(bin, base...).CombinedOutput(); err != nil {
		t.Fatalf("no-checker approved run should pass, got err %v\n%s", err, out)
	}

	// 2) Checker agrees (exit 0, KYC machine is compensation-bearing so compensation_free=false): pass.
	agree := fakeChecker(t, dir, "agree", 0, "compensation_free=false")
	if out, err := exec.Command(bin, append(base, "-checker", agree)...).CombinedOutput(); err != nil {
		t.Fatalf("agreeing checker should pass, got err %v\n%s", err, out)
	}

	// 3) Oracle disagrees on convergence (exit 1) while the certificate claims convergent: FAIL.
	disagree := fakeChecker(t, dir, "disagree", 1, "compensation_free=false")
	if out, err := exec.Command(bin, append(base, "-checker", disagree)...).CombinedOutput(); err == nil {
		t.Fatalf("disagreeing oracle must fail the command, but it exited 0\n%s", out)
	}

	// 4) The used policy is NOT in the auditor's allowlist: only-approved-policies FAILS.
	notApproved := []string{"verify-run", "-cert", certPath, "-pubkey", pubHex, "-approved", "some-other-digest"}
	if out, err := exec.Command(bin, notApproved...).CombinedOutput(); err == nil {
		t.Fatalf("a used policy outside the allowlist must fail, but it exited 0\n%s", out)
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
	res, err := govern.Quorum(ctx, store, runID, "q", 2,
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
	sth := must(audit.SignTreeHead(th, audit.Ed25519Signer{Priv: priv}))

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
	tallyP := bundlePath(govern.QuorumTallyStep("q"), "tally.json")
	aP := bundlePath(govern.QuorumVoteStep("q", "model-A"), "a.json")
	bP := bundlePath(govern.QuorumVoteStep("q", "model-B"), "b.json")
	cP := bundlePath(govern.QuorumVoteStep("q", "model-C"), "c.json")
	pubHex := hex.EncodeToString(pub)

	bin := auditBin(dir)
	if out, err := exec.Command("go", "build", "-o", bin, cliPkg).CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, out)
	}

	// 1) Quorum met (k=2), all three votes disclosed: passes.
	pass := []string{"verify-quorum", "-name", "q", "-tally", tallyP, "-vote", aP, "-vote", bP, "-vote", cP, "-pubkey", pubHex, "-k", "2"}
	if out, err := exec.Command(bin, pass...).CombinedOutput(); err != nil {
		t.Fatalf("quorum-met run should pass, got err %v\n%s", err, out)
	}

	// 2) Threshold not met (k=3 while votes_for=2): must FAIL.
	k3 := []string{"verify-quorum", "-name", "q", "-tally", tallyP, "-vote", aP, "-vote", bP, "-vote", cP, "-pubkey", pubHex, "-k", "3"}
	if out, err := exec.Command(bin, k3...).CombinedOutput(); err == nil {
		t.Fatalf("k=3 with votes_for=2 must fail, but exited 0\n%s", out)
	}

	// 3) Not all votes disclosed (2 of 3) so the tally cannot be recomputed: must FAIL.
	partial := []string{"verify-quorum", "-name", "q", "-tally", tallyP, "-vote", aP, "-vote", bP, "-pubkey", pubHex, "-k", "2"}
	if out, err := exec.Command(bin, partial...).CombinedOutput(); err == nil {
		t.Fatalf("partial vote disclosure must fail, but exited 0\n%s", out)
	}

	// 4) One vote disclosed twice in place of another (A, A, C hides B): must FAIL.
	dup := []string{"verify-quorum", "-name", "q", "-tally", tallyP, "-vote", aP, "-vote", aP, "-vote", cP, "-pubkey", pubHex, "-k", "2"}
	if out, err := exec.Command(bin, dup...).CombinedOutput(); err == nil {
		t.Fatalf("a vote disclosed twice in place of another must fail, but exited 0\n%s", out)
	}
}

// A 2-2 split meets k = 2 for both decisions, but no decision has the most votes, so the quorum
// is not met. verify-quorum must not report agreement on whichever label sorts first.
func TestVerifyQuorumCLI_TieIsNotAgreement(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := agent.NewMemStore()
	const runID = "run1"
	decide := func(v string) func(context.Context) (string, error) {
		return func(context.Context) (string, error) { return v, nil }
	}
	_, _ = govern.Quorum(ctx, store, runID, "q", 2,
		govern.Voter{Name: "model-A", Decide: decide("approve")},
		govern.Voter{Name: "model-B", Decide: decide("approve")},
		govern.Voter{Name: "model-C", Decide: decide("deny")},
		govern.Voter{Name: "model-D", Decide: decide("deny")})
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, err := audit.NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	sth := must(audit.SignTreeHead(th, audit.Ed25519Signer{Priv: priv}))
	args := []string{"verify-quorum", "-name", "q", "-pubkey", hex.EncodeToString(pub), "-k", "2"}
	for _, name := range []string{govern.QuorumTallyStep("q"), govern.QuorumVoteStep("q", "model-A"), govern.QuorumVoteStep("q", "model-B"), govern.QuorumVoteStep("q", "model-C"), govern.QuorumVoteStep("q", "model-D")} {
		pb, err := audit.ProveStep(ctx, store, runID, name, sth)
		if err != nil {
			t.Fatalf("ProveStep %s: %v", name, err)
		}
		p := filepath.Join(dir, strings.ReplaceAll(name, "/", "_")+".json")
		writeJSON(t, p, pb)
		flag := "-vote"
		if name == govern.QuorumTallyStep("q") {
			flag = "-tally"
		}
		args = append(args, flag, p)
	}
	bin := auditBin(dir)
	if out, err := exec.Command("go", "build", "-o", bin, cliPkg).CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, out)
	}
	if out, err := exec.Command(bin, args...).CombinedOutput(); err == nil {
		t.Fatalf("a 2-2 split verified as a quorum:\n%s", out)
	}
}

// must returns v, panicking on err: for producers the tests call with known-good inputs.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
