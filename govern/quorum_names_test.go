package govern_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/govern"
)

func fixedVoter(name, decision string) govern.Voter {
	return govern.Voter{Name: name, Decide: func(context.Context) (string, error) { return decision, nil }}
}

// Two quorums in one run, even over the same voters, are independent: each returns its own tally
// from its own votes, each recorded under its own name.
func TestQuorum_TwoQuorumsInOneRunAreIndependent(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	first, err := govern.Quorum(ctx, store, "run", "ship", 2, fixedVoter("a", "approve"), fixedVoter("b", "approve"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := govern.Quorum(ctx, store, "run", "refund", 2, fixedVoter("a", "reject"), fixedVoter("b", "reject"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Decision != "approve" || second.Decision != "reject" {
		t.Fatalf("first quorum decided %q, second %q; want approve, then reject", first.Decision, second.Decision)
	}
	for _, step := range []string{
		govern.QuorumConfigStep("ship"), govern.QuorumVoteStep("ship", "a"), govern.QuorumTallyStep("ship"),
		govern.QuorumConfigStep("refund"), govern.QuorumVoteStep("refund", "b"), govern.QuorumTallyStep("refund"),
	} {
		if !hasStep(t, store, "run", step) {
			t.Errorf("no step %q recorded", step)
		}
	}
}

func hasStep(t *testing.T, store agent.Durable, runID, name string) bool {
	t.Helper()
	recs, err := store.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Name == name {
			return true
		}
	}
	return false
}

// Reusing a quorum name in a run with a different voter set, voter order, or k is a config error,
// and it casts no vote: the recorded quorum never answers for another.
func TestQuorum_ReusedNameMustMatch(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	if _, err := govern.Quorum(ctx, store, "run", "q", 2, fixedVoter("a", "approve"), fixedVoter("b", "approve")); err != nil {
		t.Fatal(err)
	}
	var cast bool
	c := govern.Voter{Name: "c", Decide: func(context.Context) (string, error) { cast = true; return "reject", nil }}
	for what, call := range map[string]func() error{
		"another voter set": func() error {
			_, err := govern.Quorum(ctx, store, "run", "q", 2, fixedVoter("a", "approve"), c)
			return err
		},
		"another voter order": func() error {
			_, err := govern.Quorum(ctx, store, "run", "q", 2, fixedVoter("b", "approve"), fixedVoter("a", "approve"))
			return err
		},
		"another k": func() error {
			_, err := govern.Quorum(ctx, store, "run", "q", 1, fixedVoter("a", "approve"), fixedVoter("b", "approve"))
			return err
		},
	} {
		if err := call(); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("reusing name %q with %s: err = %v, want ErrConfig", "q", what, err)
		}
	}
	if cast {
		t.Fatal("a rejected reuse cast a vote")
	}
	if res, err := govern.Quorum(ctx, store, "run", "q", 2, fixedVoter("a", "approve"), fixedVoter("b", "approve")); err != nil || res.Decision != "approve" {
		t.Fatalf("the same call again = %+v, %v; want the recorded tally", res, err)
	}
}

// Names that could make two quorums' steps collide, and voter lists that could make two votes
// share a step, are rejected up front.
func TestQuorum_RejectsAmbiguousNames(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	cases := map[string]func() error{
		"empty quorum name": func() error {
			_, err := govern.Quorum(ctx, store, "run", "", 1, fixedVoter("a", "x"))
			return err
		},
		"quorum name with '/'": func() error {
			_, err := govern.Quorum(ctx, store, "run", "q/vote/a", 1, fixedVoter("a", "x"))
			return err
		},
		"no voters": func() error {
			_, err := govern.Quorum(ctx, store, "run", "q", 1)
			return err
		},
		"empty voter name": func() error {
			_, err := govern.Quorum(ctx, store, "run", "q", 1, fixedVoter("", "x"))
			return err
		},
		"duplicate voter": func() error {
			_, err := govern.Quorum(ctx, store, "run", "q", 1, fixedVoter("a", "x"), fixedVoter("a", "y"))
			return err
		},
	}
	for what, call := range cases {
		if err := call(); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%s: err = %v, want ErrConfig", what, err)
		}
	}
	if recs, _ := store.History(ctx, "run"); len(recs) != 0 {
		t.Fatalf("a rejected call recorded %d steps", len(recs))
	}
}
