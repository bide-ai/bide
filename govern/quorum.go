package govern

import (
	"context"
	"sort"

	"github.com/blackwell-systems/bide/agent"
)

// Voter is one named unit that produces a normalized decision. Name identifies who voted
// (a model, a provider/version, or any principal), so the recorded vote shows not just the
// tally but who voted how; it doubles as the durable step key, so it must be unique across
// the voters in one Quorum call. Decide returns a discrete decision string; the caller keeps
// the model call and its parsing inside Decide, so Quorum stays model-agnostic and every vote
// is already a comparable, normalized value. Free-form prose cannot be quorumed: Decide must
// map to a small set of labels (an enum, a yes/no, a chosen action).
type Voter struct {
	Name   string                                // identifies who voted; also the durable step key, so it must be unique per Quorum call
	Decide func(context.Context) (string, error) // produces this voter's normalized decision label
}

// Vote is one voter's recorded decision. It is the provenance the audit trail commits to.
type Vote struct {
	Voter    string `json:"voter"`    // the voter that cast this decision
	Decision string `json:"decision"` // the normalized decision label the voter chose
}

// QuorumResult is the tally over the votes. Decision is the plurality value (the decision the
// most voters chose); VotesFor is how many voters chose it; Total is the number of voters that
// voted; Agreed reports whether VotesFor >= k. Votes carries every recorded vote in voter order.
//
// Agreed is the statistical signal, not a proof. The provable object is the gate a caller builds
// from VotesFor: seed votes_for into a gsm invariant that admits the commit only when
// votes_for >= k (see examples/quorum), and the k-of-n requirement is then machine-checked over
// every possible count. Whether the agreed Decision is correct is not certified here: correlated
// model errors mean agreement is not statistical independence, so a quorum lowers single-model
// risk without certifying the answer.
type QuorumResult struct {
	Decision string `json:"decision"`  // the plurality decision (the label the most voters chose)
	VotesFor int    `json:"votes_for"` // how many voters chose Decision
	Total    int    `json:"total"`     // number of voters that successfully voted
	Agreed   bool   `json:"agreed"`    // whether VotesFor >= k (statistical signal, not a proof)
	Votes    []Vote `json:"votes"`     // every recorded vote, in voter order
}

// Quorum fans the voters out concurrently and durably, tallies their normalized decisions, and
// returns the plurality decision with its count. It is a composition helper over existing seams,
// not a new agent type: the fan-out is agent.Parallel (each vote is a journaled Step, so it is
// at-most-once and replayable), each vote and the final tally are recorded as durable Step values
// (provable one by one via audit.ProveStep), and the k-of-n gate itself is left to the caller to
// express as a gsm invariant over VotesFor (see examples/quorum). Keeping the model call inside
// each Voter.Decide keeps this helper model-agnostic and the votes normalized.
//
// Durability: the votes and the tally are memoized under runID by name. Re-running Quorum with
// the same runID returns the recorded votes without re-invoking any Voter.Decide, so a resumed
// run neither re-queries the models nor re-tallies. Voter names must therefore be unique within
// one call. The tally is recorded under the step name "quorum/tally".
//
// Tally: votes are grouped by decision value; the plurality value wins (ties broken by decision
// string, deterministically, so the recorded tally is stable across replays). Agreed is
// VotesFor >= k. A voter that errors is not recorded, so on a later resume it re-runs while the
// voters that already voted are memoized; the joined error is returned and the tally reflects
// only the votes that succeeded (Total is the count of successful votes). k <= 0 is treated as
// no gate (Agreed is true whenever at least one voter voted).
//
// The boundary, stated plainly: this returns a tally. The tally makes the k-of-n gate provable
// once a caller wires VotesFor into an invariant; the agreement itself is statistical and never a
// guarantee of correctness. Do not blur the two.
func Quorum(ctx context.Context, store agent.Durable, runID string, k int, voters ...Voter) (QuorumResult, error) {
	tasks := make([]agent.Task[Vote], len(voters))
	for i, v := range voters {
		v := v // capture per iteration
		tasks[i] = agent.Task[Vote]{
			Name: v.Name,
			Fn: func(ctx context.Context) (Vote, error) {
				decision, err := v.Decide(ctx)
				if err != nil {
					return Vote{}, err
				}
				return Vote{Voter: v.Name, Decision: decision}, nil
			},
		}
	}

	// Fan out durably: each vote is a Step (at-most-once, replayable, independently provable).
	votes, err := agent.Parallel(ctx, store, runID, 0, tasks...)
	if err != nil {
		// Some voter failed. Tally only the votes that were recorded (Voter set), so the caller
		// sees the partial tally alongside the error; the failed voters re-run on resume.
		recorded := recordedVotes(votes)
		result := tally(recorded, k)
		return result, err
	}

	// Record the tally as its own durable step so the tally itself is provable, not just the
	// individual votes, and so a resumed run returns the same tally without recomputing it.
	result, err := agent.Step(ctx, store, runID, "quorum/tally", func(context.Context) (QuorumResult, error) {
		return tally(votes, k), nil
	})
	if err != nil {
		return QuorumResult{}, err
	}
	return result, nil
}

// recordedVotes drops the zero votes left by voters that errored (their Step was not journaled),
// keeping only the votes that actually completed.
func recordedVotes(votes []Vote) []Vote {
	out := votes[:0:0]
	for _, v := range votes {
		if v.Voter != "" {
			out = append(out, v)
		}
	}
	return out
}

// tally counts agreement on the normalized decision and returns the plurality with its count.
// Ties on count are broken by decision string so the winner is deterministic across replays.
func tally(votes []Vote, k int) QuorumResult {
	counts := map[string]int{}
	for _, v := range votes {
		counts[v.Decision]++
	}

	// Deterministic plurality: highest count wins; the lexically smallest decision breaks ties.
	decisions := make([]string, 0, len(counts))
	for d := range counts {
		decisions = append(decisions, d)
	}
	sort.Strings(decisions)
	winner, best := "", 0
	for _, d := range decisions {
		if counts[d] > best {
			winner, best = d, counts[d]
		}
	}

	agreed := best >= k
	if k <= 0 {
		agreed = len(votes) > 0
	}
	return QuorumResult{
		Decision: winner,
		VotesFor: best,
		Total:    len(votes),
		Agreed:   agreed,
		Votes:    votes,
	}
}
