package govern_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/govern"
)

// A threshold above the number of voters can never be met, so the quorum could never agree
// whatever they vote. That is a misconfiguration, refused with ErrConfig before any vote is cast,
// rather than a quorum that silently reports no agreement forever.
func TestQuorum_KAboveTheVoterCountIsAConfigError(t *testing.T) {
	store := agent.NewMemStore()
	res, err := govern.Quorum(context.Background(), store, "run", "q", 3, fixedVoter("a", "approve"), fixedVoter("b", "approve"))
	if !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Quorum with k=3 over 2 voters = %+v, %v; want ErrConfig", res, err)
	}
	if hasStep(t, store, "run", govern.QuorumConfigStep("q")) {
		t.Fatal("a refused quorum recorded its config")
	}
	if res, err := govern.Quorum(context.Background(), store, "run", "q2", 2, fixedVoter("a", "approve"), fixedVoter("b", "approve")); err != nil || !res.Agreed {
		t.Fatalf("Quorum with k equal to the voter count = %+v, %v; want agreement", res, err)
	}
}

// A vote read back from the journal is the vote of the voter whose step holds it. A record in
// bob's slot that names another voter (a corrupted or tampered journal) is ErrProtocol: tallied,
// it would show alice voting twice and bob not at all.
func TestQuorum_VoteMustNameItsSlotsVoter(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	if _, err := agent.Step(ctx, store, "run", govern.QuorumVoteStep("q", "bob"), func(context.Context) (govern.Vote, error) {
		return govern.Vote{Voter: "alice", Decision: "approve"}, nil
	}, agent.StepSafety(agent.Safety{ReadOnly: true})); err != nil {
		t.Fatal(err)
	}
	res, err := govern.Quorum(ctx, store, "run", "q", 2, fixedVoter("alice", "approve"), fixedVoter("bob", "reject"))
	if !errors.Is(err, agent.ErrProtocol) {
		t.Fatalf("Quorum over a vote naming another voter = %+v, %v; want ErrProtocol", res, err)
	}
}

// The tally is derived from the votes. A recorded tally that disagrees with the recorded votes (a
// corrupted or tampered journal) is ErrProtocol, not returned as the quorum's result.
func TestQuorum_RecordedTallyMustMatchTheVotes(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	if _, err := agent.Step(ctx, store, "run", govern.QuorumTallyStep("q"), func(context.Context) (govern.QuorumResult, error) {
		return govern.QuorumResult{Decision: "approve", VotesFor: 2, Total: 2, Agreed: true,
			Votes: []govern.Vote{{Voter: "a", Decision: "approve"}, {Voter: "b", Decision: "approve"}}}, nil
	}, agent.StepSafety(agent.Safety{ReadOnly: true})); err != nil {
		t.Fatal(err)
	}
	res, err := govern.Quorum(ctx, store, "run", "q", 2, fixedVoter("a", "approve"), fixedVoter("b", "reject"))
	if !errors.Is(err, agent.ErrProtocol) {
		t.Fatalf("Quorum with a recorded tally the votes do not give = %+v, %v; want ErrProtocol", res, err)
	}
}
