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

// The tally is derived from the votes. A recorded tally that disagrees with the recorded votes in
// any field (a corrupted or tampered journal) is ErrProtocol, not returned as the quorum's result;
// one that agrees is returned as recorded.
func TestQuorum_RecordedTallyMustMatchTheVotes(t *testing.T) {
	ctx := context.Background()
	votes := []govern.Vote{{Voter: "a", Decision: "approve"}, {Voter: "b", Decision: "reject"}}
	right := govern.QuorumResult{Decision: "approve", VotesFor: 1, Total: 2, Agreed: false, Votes: votes}
	for name, tc := range map[string]struct {
		edit func(*govern.QuorumResult)
		ok   bool
	}{
		"as the votes give": {func(*govern.QuorumResult) {}, true},
		"decision":          {func(r *govern.QuorumResult) { r.Decision = "reject" }, false},
		"votes for":         {func(r *govern.QuorumResult) { r.VotesFor = 2 }, false},
		"total":             {func(r *govern.QuorumResult) { r.Total = 3 }, false},
		"agreed":            {func(r *govern.QuorumResult) { r.Agreed = true }, false},
		"votes":             {func(r *govern.QuorumResult) { r.Votes = []govern.Vote{votes[0], {Voter: "b", Decision: "approve"}} }, false},
	} {
		store := agent.NewMemStore()
		recorded := right
		recorded.Votes = append([]govern.Vote(nil), votes...)
		tc.edit(&recorded)
		if _, err := agent.Step(ctx, store, "run", govern.QuorumTallyStep("q"), func(context.Context) (govern.QuorumResult, error) {
			return recorded, nil
		}, agent.StepSafety(agent.Safety{ReadOnly: true})); err != nil {
			t.Fatal(err)
		}
		res, err := govern.Quorum(ctx, store, "run", "q", 1, fixedVoter("a", "approve"), fixedVoter("b", "reject"))
		if tc.ok && err != nil {
			t.Errorf("%s: Quorum = %+v, %v; want the recorded tally", name, res, err)
		}
		if !tc.ok && !errors.Is(err, agent.ErrProtocol) {
			t.Errorf("%s: Quorum with a recorded tally the votes do not give = %+v, %v; want ErrProtocol", name, res, err)
		}
	}
}

// A recorded vote that names no voter at all, in the slot of a voter that voted, is as foreign as
// one naming another voter.
func TestQuorum_EmptyRecordedVoteIsRefused(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	if _, err := agent.Step(ctx, store, "run", govern.QuorumVoteStep("q", "bob"), func(context.Context) (govern.Vote, error) {
		return govern.Vote{}, nil
	}, agent.StepSafety(agent.Safety{ReadOnly: true})); err != nil {
		t.Fatal(err)
	}
	res, err := govern.Quorum(ctx, store, "run", "q", 1, fixedVoter("alice", "approve"), fixedVoter("bob", "reject"))
	if !errors.Is(err, agent.ErrProtocol) {
		t.Fatalf("Quorum over an empty recorded vote = %+v, %v; want ErrProtocol", res, err)
	}
}

// The check holds on a partial tally too: when a voter fails, the votes that were recorded are
// still each their own voter's.
func TestQuorum_VoteMustNameItsSlotsVoterWhenAVoterFails(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	if _, err := agent.Step(ctx, store, "run", govern.QuorumVoteStep("q", "bob"), func(context.Context) (govern.Vote, error) {
		return govern.Vote{Voter: "alice", Decision: "approve"}, nil
	}, agent.StepSafety(agent.Safety{ReadOnly: true})); err != nil {
		t.Fatal(err)
	}
	failing := govern.Voter{Name: "carol", Decide: func(context.Context) (string, error) { return "", errors.New("provider down") }}
	res, err := govern.Quorum(ctx, store, "run", "q", 2, fixedVoter("alice", "approve"), fixedVoter("bob", "reject"), failing)
	if !errors.Is(err, agent.ErrProtocol) {
		t.Fatalf("Quorum over a foreign vote with a failed voter = %+v, %v; want ErrProtocol", res, err)
	}
}
