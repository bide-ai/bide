package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/govern"
)

// quorumBundles signs a tree head over runID and writes a ProofBundle for each named step, and
// runs a bide-audit binary built for the test.
type quorumBundles struct {
	t      *testing.T
	store  agent.Durable
	runID  string
	dir    string
	bin    string
	pubHex string
	sth    audit.SignedTreeHead
}

func newQuorumBundles(t *testing.T, store agent.Durable, runID string) *quorumBundles {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, err := audit.NewTreeHead(context.Background(), store, runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := auditBin(dir)
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, out)
	}
	return &quorumBundles{t: t, store: store, runID: runID, dir: dir, bin: bin, pubHex: hex.EncodeToString(pub), sth: audit.SignTreeHead(th, priv)}
}

func (b *quorumBundles) path(step string) string {
	b.t.Helper()
	pb, err := audit.ProveStep(context.Background(), b.store, b.runID, step, b.sth)
	if err != nil {
		b.t.Fatalf("ProveStep %s: %v", step, err)
	}
	p := filepath.Join(b.dir, strings.ReplaceAll(step, "/", "_")+".json")
	writeJSON(b.t, p, pb)
	return p
}

// verify runs verify-quorum for the quorum name with the given tally and vote steps, and returns
// its output and whether it passed.
func (b *quorumBundles) verify(name, tallyStep string, voteSteps ...string) (string, bool) {
	b.t.Helper()
	args := []string{"verify-quorum", "-name", name, "-tally", b.path(tallyStep), "-pubkey", b.pubHex, "-k", "2"}
	for _, s := range voteSteps {
		args = append(args, "-vote", b.path(s))
	}
	out, err := exec.Command(b.bin, args...).CombinedOutput()
	return string(out), err == nil
}

// Two quorums in one run over the same voters: each verifies from its own votes, and neither
// quorum's votes or tally stand in for the other's.
func TestVerifyQuorumCLI_OtherQuorumsVotesDoNotCount(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	voters := func(d string) []govern.Voter {
		return []govern.Voter{fixedCLIVoter("model-A", d), fixedCLIVoter("model-B", d), fixedCLIVoter("model-C", "abstain")}
	}
	if _, err := govern.Quorum(ctx, store, "run", "ship", 2, voters("approve")...); err != nil {
		t.Fatal(err)
	}
	if _, err := govern.Quorum(ctx, store, "run", "refund", 2, voters("deny")...); err != nil {
		t.Fatal(err)
	}
	b := newQuorumBundles(t, store, "run")
	votes := func(q string) []string {
		return []string{govern.QuorumVoteStep(q, "model-A"), govern.QuorumVoteStep(q, "model-B"), govern.QuorumVoteStep(q, "model-C")}
	}
	for _, q := range []string{"ship", "refund"} {
		if out, ok := b.verify(q, govern.QuorumTallyStep(q), votes(q)...); !ok {
			t.Fatalf("quorum %q with its own votes should verify:\n%s", q, out)
		}
	}
	if out, ok := b.verify("ship", govern.QuorumTallyStep("ship"), votes("refund")...); ok || !strings.Contains(out, "not \"model-A\"'s vote in quorum \"ship\"") {
		t.Fatalf("ship's tally verified against refund's votes, or failed for another reason:\n%s", out)
	}
	if out, ok := b.verify("refund", govern.QuorumTallyStep("ship"), votes("refund")...); ok || !strings.Contains(out, "not the tally of quorum \"refund\"") {
		t.Fatalf("ship's tally verified as refund's, or failed for another reason:\n%s", out)
	}
}

// The disclosed votes must be exactly the votes the tally records: a vote step in the run that the
// tally does not record, or a disclosed decision that differs from the tally's, fails.
func TestVerifyQuorumCLI_DisclosedVotesMustBeTheRecordedOnes(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	record := func(step string, v any) {
		t.Helper()
		if _, err := agent.Step(ctx, store, "run", step, func(context.Context) (any, error) { return v, nil }); err != nil {
			t.Fatal(err)
		}
	}
	// A journal written by hand: the tally records a and b approving, while the run also holds a
	// vote step for x, and one for c whose decision the tally reports differently.
	record(govern.QuorumVoteStep("q", "a"), govern.Vote{Voter: "a", Decision: "approve"})
	record(govern.QuorumVoteStep("q", "b"), govern.Vote{Voter: "b", Decision: "approve"})
	record(govern.QuorumVoteStep("q", "x"), govern.Vote{Voter: "x", Decision: "approve"})
	record(govern.QuorumVoteStep("q", "c"), govern.Vote{Voter: "c", Decision: "deny"})
	record(govern.QuorumTallyStep("q"), govern.QuorumResult{Decision: "approve", VotesFor: 2, Total: 2, Agreed: true,
		Votes: []govern.Vote{{Voter: "a", Decision: "approve"}, {Voter: "b", Decision: "approve"}}})
	record("quorum/r/tally", govern.QuorumResult{Decision: "approve", VotesFor: 2, Total: 2, Agreed: true,
		Votes: []govern.Vote{{Voter: "a", Decision: "approve"}, {Voter: "c", Decision: "approve"}}})
	record(govern.QuorumVoteStep("r", "a"), govern.Vote{Voter: "a", Decision: "approve"})
	record(govern.QuorumVoteStep("r", "c"), govern.Vote{Voter: "c", Decision: "deny"})
	// Tallies whose vote list disagrees with their total.
	record("quorum/s/tally", govern.QuorumResult{Decision: "approve", VotesFor: 2, Total: 2, Agreed: true,
		Votes: []govern.Vote{{Voter: "a", Decision: "approve"}}})
	record(govern.QuorumVoteStep("s", "a"), govern.Vote{Voter: "a", Decision: "approve"})
	record(govern.QuorumVoteStep("s", "x"), govern.Vote{Voter: "x", Decision: "approve"})
	record("quorum/u/tally", govern.QuorumResult{Decision: "approve", VotesFor: 2, Total: 3, Agreed: true,
		Votes: []govern.Vote{{Voter: "a", Decision: "approve"}, {Voter: "b", Decision: "approve"}}})
	record(govern.QuorumVoteStep("u", "a"), govern.Vote{Voter: "a", Decision: "approve"})
	record(govern.QuorumVoteStep("u", "b"), govern.Vote{Voter: "b", Decision: "approve"})
	b := newQuorumBundles(t, store, "run")
	a, bv, x := govern.QuorumVoteStep("q", "a"), govern.QuorumVoteStep("q", "b"), govern.QuorumVoteStep("q", "x")

	if out, ok := b.verify("q", govern.QuorumTallyStep("q"), a, bv); !ok {
		t.Fatalf("the recorded votes should verify:\n%s", out)
	}
	if out, ok := b.verify("q", govern.QuorumTallyStep("q"), a, bv, x); ok || !strings.Contains(out, "disclose exactly the votes the tally records") {
		t.Fatalf("a vote the tally does not record was accepted, or failed for another reason:\n%s", out)
	}
	if out, ok := b.verify("q", govern.QuorumTallyStep("q"), a, x); ok || !strings.Contains(out, "vote by \"b\" that is not disclosed") {
		t.Fatalf("a vote standing in for a recorded one was accepted, or failed for another reason:\n%s", out)
	}
	if out, ok := b.verify("r", "quorum/r/tally", govern.QuorumVoteStep("r", "a"), govern.QuorumVoteStep("r", "c")); ok || !strings.Contains(out, "the tally records \"c\" voting \"approve\", but the disclosed vote is \"deny\"") {
		t.Fatalf("a disclosed vote that contradicts the tally was accepted, or failed for another reason:\n%s", out)
	}
	if out, ok := b.verify("s", "quorum/s/tally", govern.QuorumVoteStep("s", "a"), govern.QuorumVoteStep("s", "x")); ok || !strings.Contains(out, "disclose exactly the votes the tally records") {
		t.Fatalf("a vote missing from the tally's list was accepted, or failed for another reason:\n%s", out)
	}
	if out, ok := b.verify("u", "quorum/u/tally", govern.QuorumVoteStep("u", "a"), govern.QuorumVoteStep("u", "b")); ok || !strings.Contains(out, "disclose exactly the votes the tally records") {
		t.Fatalf("a tally whose total disagrees with its votes was accepted, or failed for another reason:\n%s", out)
	}
}

func fixedCLIVoter(name, decision string) govern.Voter {
	return govern.Voter{Name: name, Decide: func(context.Context) (string, error) { return decision, nil }}
}
