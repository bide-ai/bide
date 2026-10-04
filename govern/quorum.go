package govern

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/bide-ai/bide/agent"
)

// Voter is one named unit that produces a normalized decision. Name identifies who voted
// (a model, a provider/version, or any principal), so the recorded vote shows not just the
// tally but who voted how; it is part of the vote's durable step name, so it must be non-empty
// and unique across the voters in one Quorum call. Decide returns a discrete decision string;
// the caller keeps the model call and its parsing inside Decide, so Quorum stays model-agnostic
// and every vote is already a comparable, normalized value. Free-form prose cannot be quorumed:
// Decide must map to a small set of labels (an enum, a yes/no, a chosen action). Decide must have
// no side effects: a vote not yet recorded when the process dies is cast again on resume.
type Voter struct {
	Name   string                                // identifies who voted; part of the vote's step name, so non-empty and unique per Quorum call
	Decide func(context.Context) (string, error) // produces this voter's normalized decision label
}

// Vote is one voter's recorded decision. It is the provenance the audit trail commits to.
type Vote struct {
	Voter    string `json:"voter"`    // the voter that cast this decision
	Decision string `json:"decision"` // the normalized decision label the voter chose
}

// QuorumResult is the tally over the votes. Decision is the plurality value (the decision the
// most voters chose); VotesFor is how many voters chose it; Total is the number of voters that
// voted; Agreed reports whether VotesFor >= k and no other decision has as many votes (a tie for
// the most votes is never agreement). Votes carries every recorded vote in voter order.
//
// Agreed is the statistical signal, not a proof. The provable object is the gate a caller builds
// from VotesFor: seed votes_for into a gsm invariant that admits the commit only when
// votes_for >= k (see examples/govern/quorum), and the k-of-n requirement is then machine-checked over
// every possible count. Whether the agreed Decision is correct is not certified here: correlated
// model errors mean agreement is not statistical independence, so a quorum lowers single-model
// risk without certifying the answer.
type QuorumResult struct {
	Decision string `json:"decision"`  // the plurality decision; on a tie, the lexically smallest tied label (a placeholder, and Agreed is false)
	VotesFor int    `json:"votes_for"` // how many voters chose Decision
	Total    int    `json:"total"`     // number of voters that successfully voted
	Agreed   bool   `json:"agreed"`    // VotesFor >= k and no tie for the most votes (statistical signal, not a proof)
	Votes    []Vote `json:"votes"`     // every recorded vote, in voter order
}

// Quorum fans the voters out concurrently and durably, tallies their normalized decisions, and
// returns the plurality decision with its count. It is a composition helper over existing seams,
// not a new agent type: the fan-out is agent.Journal.Parallel (each vote is a journaled Step, so it is
// at-most-once and replayable), each vote and the final tally are recorded as durable Step values
// (provable one by one via audit.ProveStep), and the k-of-n gate itself is left to the caller to
// express as a gsm invariant over VotesFor (see examples/govern/quorum). Keeping the model call inside
// each Voter.Decide keeps this helper model-agnostic and the votes normalized.
//
// Naming: name identifies this quorum within the run, so one run can hold several quorums, even
// over the same voters. It must be non-empty and contain no '/'. The quorum's steps are
// QuorumConfigStep(name) (its k and voter names, recorded first), QuorumVoteStep(name, voter) for
// each vote, and QuorumTallyStep(name) for the tally.
//
// Durability: the votes and the tally are memoized under runID by step name. Re-running Quorum
// with the same runID and name returns the recorded votes without re-invoking any Voter.Decide, so
// a resumed run neither re-queries the models nor re-tallies. A re-run must pass the same k and the
// same voter names in the same order: any other call under a recorded name is an agent.ErrConfig
// error, and nothing is cast or tallied, so one quorum's record never answers for another's.
//
// Tally: votes are grouped by decision value; the plurality value wins (ties broken by decision
// string, deterministically, so the recorded tally is stable across replays). Agreed is
// VotesFor >= k with no tie for the most votes: a tie is never agreement. A voter that errors is not recorded, so on a later resume it re-runs while the
// voters that already voted are memoized; the joined error is returned and the tally reflects
// only the votes that succeeded (Total is the count of successful votes). k <= 0 is treated as
// no gate (Agreed is true whenever at least one voter voted and the top count is not tied). A k
// above the number of voters could never be met and is an agent.ErrConfig error.
//
// Records read back: each recorded vote must name the voter whose step holds it, and a recorded
// tally must equal the tally of the recorded votes. A journal that breaks either (a corrupted or
// edited record) is an agent.ErrProtocol error with no tally, so a vote is never counted as
// another voter's and a tally the votes do not give is never returned.
//
// The boundary, stated plainly: this returns a tally. The tally makes the k-of-n gate provable
// once a caller wires VotesFor into an invariant; the agreement itself is statistical and never a
// guarantee of correctness. Do not blur the two.
func Quorum(ctx context.Context, store *agent.Journal, runID, name string, k int, voters ...Voter) (QuorumResult, error) {
	cfg := quorumConfig{K: k, Voters: make([]string, len(voters))}
	if name == "" || strings.Contains(name, "/") {
		return QuorumResult{}, fmt.Errorf("govern: quorum name %q must be non-empty and contain no '/': %w", name, agent.ErrConfig)
	}
	if len(voters) == 0 {
		return QuorumResult{}, fmt.Errorf("govern: quorum %q has no voters: %w", name, agent.ErrConfig)
	}
	seen := map[string]bool{}
	for i, v := range voters {
		if v.Name == "" || seen[v.Name] {
			return QuorumResult{}, fmt.Errorf("govern: quorum %q voter name %q must be non-empty and unique: %w", name, v.Name, agent.ErrConfig)
		}
		seen[v.Name] = true
		cfg.Voters[i] = v.Name
	}
	if k > len(voters) {
		return QuorumResult{}, fmt.Errorf("govern: quorum %q needs k=%d votes but has %d voters, so it could never agree: %w", name, k, len(voters), agent.ErrConfig)
	}

	// Record this quorum's k and voters before any vote, and hold every later call under the same
	// name to them: a reused name with another voter set would otherwise mix two quorums' votes, and
	// a changed k would be answered by a tally computed against the old one.
	recorded, err := store.Step(ctx, runID, QuorumConfigStep(name), func(context.Context) (quorumConfig, error) {
		return cfg, nil
	}, agent.WithSafety(agent.Safety{ReadOnly: true}))
	if err != nil {
		return QuorumResult{}, err
	}
	if recorded.K != cfg.K || !slices.Equal(recorded.Voters, cfg.Voters) {
		return QuorumResult{}, fmt.Errorf("govern: quorum %q in run %q was started with k=%d and voters %q, not k=%d and voters %q: %w",
			name, runID, recorded.K, recorded.Voters, cfg.K, cfg.Voters, agent.ErrConfig)
	}

	tasks := make([]agent.Task[Vote], len(voters))
	for i, v := range voters {
		tasks[i] = agent.Task[Vote]{
			Name:   QuorumVoteStep(name, v.Name),
			Safety: agent.Safety{ReadOnly: true}, // a vote is a decision, not an effect (see Voter)
			Fn: func(ctx context.Context) (Vote, error) {
				decision, err := v.Decide(ctx)
				if err != nil {
					return Vote{}, err
				}
				return Vote{Voter: v.Name, Decision: decision}, nil
			},
		}
	}

	// Fan out durably: each vote is a Step (recorded once, replayable, independently provable).
	votes, err := store.Parallel(ctx, runID, tasks)
	if verr := checkVoters(name, votes, cfg.Voters, err == nil); verr != nil {
		return QuorumResult{}, verr
	}
	if err != nil {
		// Some voter failed. Tally only the votes that were recorded (Voter set), so the caller
		// sees the partial tally alongside the error; the failed voters re-run on resume.
		recorded := recordedVotes(votes)
		result := tally(recorded, k)
		return result, err
	}

	// Record the tally as its own durable step so the tally itself is provable, not just the
	// individual votes, and so a resumed run returns the same tally without recomputing it.
	want := tally(votes, k)
	result, err := store.Step(ctx, runID, QuorumTallyStep(name), func(context.Context) (QuorumResult, error) {
		return want, nil
	}, agent.WithSafety(agent.Safety{ReadOnly: true}))
	if err != nil {
		return QuorumResult{}, err
	}
	// The tally is derived from the votes: a recorded one that the recorded votes do not give (a
	// corrupted or edited journal) is not this quorum's result.
	if !sameTally(result, want) {
		return QuorumResult{}, fmt.Errorf("govern: quorum %q in run %q has a recorded tally %+v that its votes do not give (%+v): %w", name, runID, result, want, agent.ErrProtocol)
	}
	return result, nil
}

// checkVoters refuses votes read back from the journal that do not belong to the voter whose step
// holds them: votes[i] is voters[i]'s vote. A record naming another voter (a corrupted or edited
// journal) would be tallied as that voter's, so one voter would count twice. With all set, every
// voter must have voted; otherwise a voter that failed left a zero Vote, which is skipped.
func checkVoters(name string, votes []Vote, voters []string, all bool) error {
	for i, v := range votes {
		if v.Voter == voters[i] || (!all && v == Vote{}) {
			continue
		}
		return fmt.Errorf("govern: quorum %q has a vote recorded for voter %q that names voter %q: %w", name, voters[i], v.Voter, agent.ErrProtocol)
	}
	return nil
}

// sameTally reports whether two tallies are equal, votes included.
func sameTally(a, b QuorumResult) bool {
	return a.Decision == b.Decision && a.VotesFor == b.VotesFor && a.Total == b.Total && a.Agreed == b.Agreed && slices.Equal(a.Votes, b.Votes)
}

// quorumConfig is what a quorum records before it votes: its threshold and its voters, in order.
type quorumConfig struct {
	K      int      `json:"k"`
	Voters []string `json:"voters"`
}

// QuorumConfigStep is the step name under which the quorum named name records its k and voters.
func QuorumConfigStep(name string) string { return "quorum/" + name + "/config" }

// QuorumVoteStep is the step name of voter's vote in the quorum named name. Because a quorum name
// contains no '/', the name and voter are recoverable from it: everything after
// "quorum/<name>/vote/" is the voter.
func QuorumVoteStep(name, voter string) string { return "quorum/" + name + "/vote/" + voter }

// QuorumTallyStep is the step name of the tally of the quorum named name.
func QuorumTallyStep(name string) string { return "quorum/" + name + "/tally" }

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

	// A tie for the most votes is not agreement: no decision has more support than another, and
	// Decision (the lexically smallest of the tied labels) is only a deterministic placeholder.
	tied := false
	for _, d := range decisions {
		if d != winner && counts[d] == best {
			tied = true
		}
	}
	agreed := best >= k && !tied
	if k <= 0 {
		agreed = len(votes) > 0 && !tied
	}
	return QuorumResult{
		Decision: winner,
		VotesFor: best,
		Total:    len(votes),
		Agreed:   agreed,
		Votes:    votes,
	}
}
