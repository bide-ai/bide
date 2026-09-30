package bideaudit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/govern"
)

// buildCLI builds the bide-audit binary into dir and returns its path.
func buildCLI(t *testing.T, dir string) string {
	t.Helper()
	bin := auditBin(dir)
	if out, err := exec.Command("go", "build", "-o", bin, cliPkg).CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, out)
	}
	return bin
}

// exitCode runs the command and returns its exit code and combined output.
func exitCode(t *testing.T, bin string, args ...string) (int, string) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("run %v: %v", args, err)
	return -1, ""
}

// verify-quorum must reject a vote from another run, even one whose journal is byte-identical (so
// its tree has the same root): the signed head names the run.
func TestVerifyQuorumCLI_RejectsVoteFromAnotherRun(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := agent.NewMemStore()
	decide := func(v string) func(context.Context) (string, error) {
		return func(context.Context) (string, error) { return v, nil }
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	bundles := map[string]string{}
	for _, runID := range []string{"run1", "run2"} {
		if _, err := govern.Quorum(ctx, store, runID, "refund", 2,
			govern.Voter{Name: "model-A", Decide: decide("approve")},
			govern.Voter{Name: "model-B", Decide: decide("approve")}); err != nil {
			t.Fatal(err)
		}
		th, err := audit.NewTreeHead(ctx, store, runID, 1)
		if err != nil {
			t.Fatal(err)
		}
		sth := audit.SignTreeHead(th, priv)
		for _, name := range []string{"quorum/refund/tally", "quorum/refund/vote/model-A", "quorum/refund/vote/model-B"} {
			pb, err := audit.ProveStep(ctx, store, runID, name, sth)
			if err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(dir, runID+"-"+filepath.Base(name)+".json")
			writeJSON(t, p, pb)
			bundles[runID+"/"+filepath.Base(name)] = p
		}
	}
	bin := buildCLI(t, dir)
	pubHex := hex.EncodeToString(pub)
	args := func(voteB string) []string {
		return []string{"verify-quorum", "-name", "refund", "-tally", bundles["run1/tally"], "-vote", bundles["run1/model-A"], "-vote", voteB, "-pubkey", pubHex, "-k", "2"}
	}
	if code, out := exitCode(t, bin, args(bundles["run1/model-B"])...); code != 0 {
		t.Fatalf("the genuine quorum failed: exit %d\n%s", code, out)
	}
	if code, out := exitCode(t, bin, args(bundles["run2/model-B"])...); code == 0 {
		t.Fatalf("verify-quorum counted a vote from run2 toward run1's tally:\n%s", out)
	}
}
