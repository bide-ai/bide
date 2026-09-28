package govern_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/govern"
)

// fixed builds a voter that always returns the same decision, counting how many times it is
// actually invoked, so a test can prove durable memoization (a re-run must not re-invoke it).
func fixed(name, decision string, calls *int32) govern.Voter {
	return govern.Voter{
		Name: name,
		Decide: func(context.Context) (string, error) {
			atomic.AddInt32(calls, 1)
			return decision, nil
		},
	}
}

// TestQuorumMet: enough voters agree, so Agreed is true and the plurality is the agreed value.
func TestQuorumMet(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()

	res, err := govern.Quorum(ctx, store, "run/met", 2,
		govern.Voter{Name: "a", Decide: func(context.Context) (string, error) { return "approve", nil }},
		govern.Voter{Name: "b", Decide: func(context.Context) (string, error) { return "approve", nil }},
		govern.Voter{Name: "c", Decide: func(context.Context) (string, error) { return "deny", nil }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Agreed {
		t.Fatalf("expected quorum met, got %+v", res)
	}
	if res.Decision != "approve" {
		t.Fatalf("expected plurality approve, got %q", res.Decision)
	}
	if res.VotesFor != 2 || res.Total != 3 {
		t.Fatalf("expected 2 of 3, got votes_for=%d total=%d", res.VotesFor, res.Total)
	}
}

// TestQuorumNotMet: no decision reaches k, so Agreed is false. A three-way split with k=2 fails.
func TestQuorumNotMet(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()

	res, err := govern.Quorum(ctx, store, "run/split", 2,
		govern.Voter{Name: "a", Decide: func(context.Context) (string, error) { return "approve", nil }},
		govern.Voter{Name: "b", Decide: func(context.Context) (string, error) { return "deny", nil }},
		govern.Voter{Name: "c", Decide: func(context.Context) (string, error) { return "escalate", nil }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if res.Agreed {
		t.Fatalf("expected quorum NOT met on a three-way split, got %+v", res)
	}
	if res.VotesFor != 1 {
		t.Fatalf("expected plurality of 1, got %d", res.VotesFor)
	}
}

// TestQuorumPlurality: the winning value is the one the most voters chose, even without a majority.
func TestQuorumPlurality(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()

	// deny gets 2, approve 1, escalate 1: deny is the plurality though it is not a majority of 4.
	res, err := govern.Quorum(ctx, store, "run/plurality", 2,
		govern.Voter{Name: "a", Decide: func(context.Context) (string, error) { return "deny", nil }},
		govern.Voter{Name: "b", Decide: func(context.Context) (string, error) { return "deny", nil }},
		govern.Voter{Name: "c", Decide: func(context.Context) (string, error) { return "approve", nil }},
		govern.Voter{Name: "d", Decide: func(context.Context) (string, error) { return "escalate", nil }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != "deny" {
		t.Fatalf("expected plurality deny, got %q", res.Decision)
	}
	if res.VotesFor != 2 || res.Total != 4 {
		t.Fatalf("expected 2 of 4, got votes_for=%d total=%d", res.VotesFor, res.Total)
	}
	if !res.Agreed {
		t.Fatalf("expected k=2 met by the plurality, got %+v", res)
	}
}

// TestQuorumDurable: re-running Quorum with the same runID returns the memoized votes and tally
// and does NOT re-invoke any Voter.Decide. This is at-most-once fan-out, proven by a call counter.
func TestQuorumDurable(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	var callsA, callsB, callsC int32

	first, err := govern.Quorum(ctx, store, "run/dur", 2,
		fixed("a", "approve", &callsA),
		fixed("b", "approve", &callsB),
		fixed("c", "deny", &callsC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := callsA + callsB + callsC; got != 3 {
		t.Fatalf("expected 3 voter invocations on first run, got %d", got)
	}

	second, err := govern.Quorum(ctx, store, "run/dur", 2,
		fixed("a", "approve", &callsA),
		fixed("b", "approve", &callsB),
		fixed("c", "deny", &callsC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := callsA + callsB + callsC; got != 3 {
		t.Fatalf("re-run re-invoked Decide: expected still 3 invocations, got %d", got)
	}
	if second.Decision != first.Decision || second.VotesFor != first.VotesFor || second.Total != first.Total {
		t.Fatalf("re-run tally differed: first=%+v second=%+v", first, second)
	}
}

// TestQuorumProvenance: each vote is a durable step recording who voted how, and is independently
// provable against a signed tree head. The tally is likewise provable.
func TestQuorumProvenance(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	const runID = "run/prov"

	res, err := govern.Quorum(ctx, store, runID, 2,
		govern.Voter{Name: "gpt", Decide: func(context.Context) (string, error) { return "approve", nil }},
		govern.Voter{Name: "claude", Decide: func(context.Context) (string, error) { return "approve", nil }},
		govern.Voter{Name: "gemini", Decide: func(context.Context) (string, error) { return "deny", nil }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Votes) != 3 {
		t.Fatalf("expected 3 recorded votes, got %d", len(res.Votes))
	}

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, err := audit.NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	sth := audit.SignTreeHead(th, priv)

	// Each named vote and the tally are provable on their own, without disclosing the others.
	for _, name := range []string{"gpt", "claude", "gemini", "quorum/tally"} {
		pb, err := audit.ProveStep(ctx, store, runID, name, sth)
		if err != nil {
			t.Fatalf("prove %q: %v", name, err)
		}
		ok, err := pb.Verify(pub)
		if err != nil {
			t.Fatalf("verify %q: %v", name, err)
		}
		if !ok {
			t.Fatalf("inclusion proof for %q did not verify", name)
		}
	}
}

// TestQuorumVoterError: a voter that errors is not recorded; the joined error surfaces and the
// partial tally reflects only the votes that succeeded.
func TestQuorumVoterError(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	boom := errors.New("model unavailable")

	res, err := govern.Quorum(ctx, store, "run/err", 2,
		govern.Voter{Name: "a", Decide: func(context.Context) (string, error) { return "approve", nil }},
		govern.Voter{Name: "b", Decide: func(context.Context) (string, error) { return "", boom }},
		govern.Voter{Name: "c", Decide: func(context.Context) (string, error) { return "approve", nil }},
	)
	if err == nil {
		t.Fatal("expected the voter error to surface")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected joined error to wrap the voter error, got %v", err)
	}
	// The two successful votes are still tallied; the failed voter is absent.
	if res.Total != 2 {
		t.Fatalf("expected 2 successful votes tallied, got %d", res.Total)
	}
	if res.Decision != "approve" || res.VotesFor != 2 {
		t.Fatalf("expected approve x2 from the votes that succeeded, got %+v", res)
	}
}
