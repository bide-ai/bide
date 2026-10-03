// Command quorum shows a governed model quorum built as a composition of existing seams, not a new
// agent type (see docs/guides/quorum.md). A high-stakes decision is put to N voters; the commit is admitted
// only under k-of-n agreement, and the whole vote is in the audit trail.
//
// The pieces are all seams the SDK already has:
//   - fan-out: govern.Quorum runs the voters with agent.Parallel, so each vote is a durable Step;
//   - tally: plain Go, counting agreement on the normalized decision;
//   - govern: a gsm invariant makes the k-of-n gate provable (verified over every possible count);
//   - commit: govern.EventTool with a PolicyDigest journals the commit, binding it to the policy and state;
//   - escalate: a failed quorum forces the decision to "escalate" and pauses under an approval gate (agent.WithApproval);
//   - audit: each vote, the tally, and the commit are provable offline against a signed tree head.
//
// There is no LLM or network here: the voters are stubs returning fixed decisions, so the example
// runs as-is and exercises the durability + governance + audit machinery. In a real deployment each
// Voter.Decide would call a different model (diversity over count) and parse its output to a label;
// the guarantees shown are identical.
//
// The boundary, stated plainly: the k-of-n gate is machine-checked; the agreement is statistical.
// Correlated model errors mean agreement is not independence, so a quorum lowers single-model risk
// without certifying the answer. This demo proves the gate, not the correctness of the decision.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/govern"
	gsm "github.com/blackwell-systems/gsm"
)

// k of n: three voters, majority agreement (k = 2) required to commit.
const (
	n = 3
	k = 2
)

// quorumName names the quorum within its run: its votes and tally are recorded under
// quorum/<name>/vote/<voter> and quorum/<name>/tally, so one run can hold several quorums.
const quorumName = "release"

func main() {
	ctx := context.Background()

	// One policy. votes_for is a STATE variable, so the gate "committed => votes_for >= k" is
	// verified exhaustively over every possible count: one proof for all vote tallies.
	m, votesFor, committed, policyDigest := buildPolicy()

	// (a) Three voters agree: quorum met, the decision commits under the attested tool.
	fmt.Println("== case A: voters agree ==")
	runAgree(ctx, m, votesFor, committed, policyDigest)

	// (b) Three voters disagree: quorum not met, the gate forces escalate and the commit is a
	// no-op; the escalate path pauses for human approval.
	fmt.Println("\n== case B: voters disagree ==")
	runDisagree(ctx, m, votesFor, committed, policyDigest)

	fmt.Println("\nOne machine-checked policy gated both runs on k-of-n agreement: the agreed case")
	fmt.Println("committed and is provable to the policy and the votes; the split case escalated and")
	fmt.Println("did not commit. The gate is proven; the agreement is statistical, not a guarantee.")
}

// buildPolicy expresses the k-of-n requirement as a gsm invariant. The decision may be committed
// only with quorum, and the commit event is guarded so it is a no-op below k. Build succeeding is
// gsm's certificate that the policy converges (WFC + CC) over every (decision, votes_for,
// committed) state.
func buildPolicy() (*gsm.Machine, gsm.Var, gsm.Var, string) {
	r := gsm.NewRegistry("model-quorum")
	decision := r.Enum("decision", "approve", "deny", "escalate")
	votesFor := r.Int("votes_for", 0, n) // how many voters chose the committed decision
	committed := r.Bool("committed")

	// The gate: a decision may be committed only under quorum. committed => votes_for >= k,
	// verified over every votes_for value. On no quorum the repair reverts the commit (which
	// restores the invariant, so compensation terminates) and forces the decision to escalate. The
	// two assigns compose into one repair: Transform is a slice of assigns.
	revertAndEscalate := append(gsm.Do(gsm.Set(committed, gsm.Lit(0))), gsm.SetLabel(decision, "escalate")...)
	r.Rule("quorum_required").
		Require(gsm.Or(gsm.Is(committed, 0), gsm.Ge(gsm.V(votesFor), gsm.Lit(k)))).
		RepairWith(revertAndEscalate).
		Add()

	// Pre-commit guard: the commit event does nothing unless quorum is met. votes_for is seeded as
	// initial state from the (verified) tally before commit is applied, exactly as authority seeds
	// a desk's limit; commit is the only event, so there is no order to depend on.
	r.On("commit").OnlyIf(gsm.Ge(gsm.V(votesFor), gsm.Lit(k))).Does(gsm.SetTo(committed, 1)).Add()

	machine, rep, err := r.Build()
	if err != nil {
		panic(fmt.Sprintf("policy did not verify: %v\n%s", err, rep))
	}
	digest, _ := r.PolicyDigest()
	fmt.Printf("one policy, proven convergent over all %d states; k=%d of n=%d; digest %s\n\n",
		rep.StateCount, k, n, digest[:16]+"...")
	return machine, votesFor, committed, digest
}

// runAgree: three voters, two agree on approve. Quorum is met, so the commit event fires under the
// attested tool and the decision is committed. The vote, the tally, and the commit are all provable.
func runAgree(ctx context.Context, m *gsm.Machine, votesFor, committed gsm.Var, policyDigest string) {
	store, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		log.Fatal(err)
	}
	const runID = "quorum/agree"

	voters := []govern.Voter{
		{Name: "model-A", Decide: decide("approve")},
		{Name: "model-B", Decide: decide("approve")},
		{Name: "model-C", Decide: decide("deny")},
	}
	res, err := govern.Quorum(ctx, store, runID, quorumName, k, voters...)
	if err != nil {
		panic(err)
	}
	printTally(res)

	// Seed the (verified) tally into governed state, then attempt the commit. The governor caps
	// the outcome to what the policy admits: with votes_for >= k the guard lets commit through.
	gov := govern.New(m, m.NewState().SetInt(votesFor, res.VotesFor))
	commit := govern.EventTool(gov, govern.EventToolConfig{Name: "commit", Description: "commit the quorum decision", Event: "commit", PolicyDigest: policyDigest})

	// The agent commits through the attested tool in the same run, so the commit leaf is journaled
	// next to the votes as the run's provable action. Its model is scripted (no LLM): it calls
	// commit, then answers.
	model := agent.NewScriptedModel(agent.ToolTurn("commit/leaf", "commit", `{}`), agent.TextTurn("committed"))
	a, err := agent.New(model, store, agent.WithTools(commit))
	if err != nil {
		log.Fatal(err)
	}
	if _, err := a.Run(ctx, runID, agent.UserText("commit the quorum decision")); err != nil {
		panic(err)
	}
	leaf := toolResult(ctx, store, runID, "commit/leaf")
	var m2 map[string]any
	_ = json.Unmarshal(leaf, &m2)
	fmt.Printf("committed=%v (quorum met); leaf binds policy=%v state=%v\n",
		gov.State().GetBool(committed), m2["policy_digest"].(string)[:12]+"...", m2["state_digest"].(string)[:12]+"...")

	proveRun(ctx, store, runID, quorumSteps(voters), "commit/leaf")
}

// runDisagree: three voters split three ways. Quorum is NOT met, so the guard makes commit a no-op
// and the invariant forces the decision to escalate. The escalate path routes to human approval
// (agent.WithApproval), which pauses the run durably; nothing commits.
func runDisagree(ctx context.Context, m *gsm.Machine, votesFor, committed gsm.Var, policyDigest string) {
	store, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		log.Fatal(err)
	}
	const runID = "quorum/disagree"

	voters := []govern.Voter{
		{Name: "model-A", Decide: decide("approve")},
		{Name: "model-B", Decide: decide("deny")},
		{Name: "model-C", Decide: decide("escalate")},
	}
	res, err := govern.Quorum(ctx, store, runID, quorumName, k, voters...)
	if err != nil {
		panic(err)
	}
	printTally(res)

	// No quorum: seed the (sub-k) count. The commit guard is false, so commit is a no-op, and the
	// invariant forces the decision to escalate.
	gov := govern.New(m, m.NewState().SetInt(votesFor, res.VotesFor))
	commit := govern.EventTool(gov, govern.EventToolConfig{Name: "commit", Description: "commit the quorum decision", Event: "commit", PolicyDigest: policyDigest})

	// The disagreement policy is explicit: escalate to a human under dual control. The escalate
	// tool has an approval gate, so calling it through the agent loop pauses the run durably rather
	// than acting.
	escalate := agent.Func("escalate", "route the ungoverned decision to a human",
		agent.Safety{},
		func(context.Context, struct{}) (map[string]any, error) {
			return map[string]any{"escalated": true}, nil
		}, agent.WithApproval(agent.SingleApproval()))

	// The agent's (scripted, LLM-free) model tries the commit, then escalates. The commit is
	// attested and journaled either way, so the audit trail records that the action was gated out,
	// not silently dropped; the escalate call has no recorded approval, so the run pauses.
	model := agent.NewScriptedModel(
		agent.ToolTurn("commit/1", "commit", `{}`),
		agent.ToolTurn("escalate/1", "escalate", `{}`),
		agent.TextTurn("escalated"),
	)
	a, err := agent.New(model, store, agent.WithTools(commit, escalate))
	if err != nil {
		log.Fatal(err)
	}
	_, runErr := a.Run(ctx, runID, agent.UserText("commit the quorum decision"))

	didCommit := gov.State().GetBool(committed)
	fmt.Printf("committed=%v (quorum NOT met); the guard made commit a no-op\n", didCommit)

	// A paused run returns *agent.ApprovalPending; it resumes only after agent.Approve records a
	// decision.
	var pending *agent.ApprovalPending
	if !errors.As(runErr, &pending) {
		panic(fmt.Sprintf("expected the escalate call to pause for approval, got %v", runErr))
	}
	fmt.Printf("escalate path: paused for human approval under dual control: %s\n", pending.Error())

	proveRun(ctx, store, runID, quorumSteps(voters), "")
}

// quorumSteps lists the steps to prove for the quorum: each voter's vote, then the tally.
func quorumSteps(voters []govern.Voter) []string {
	steps := make([]string, 0, len(voters)+1)
	for _, v := range voters {
		steps = append(steps, govern.QuorumVoteStep(quorumName, v.Name))
	}
	return append(steps, govern.QuorumTallyStep(quorumName))
}

// decide returns a Voter.Decide that yields a fixed normalized decision (a stand-in for a model
// call plus its output parsing).
func decide(v string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return v, nil }
}

func printTally(res govern.QuorumResult) {
	fmt.Println("votes (parallel, durable, each independently provable):")
	for _, v := range res.Votes {
		fmt.Printf("  %-10s -> %s\n", v.Voter, v.Decision)
	}
	fmt.Printf("tally: decision=%q votes_for=%d of %d, agreed(k=%d)=%v\n",
		res.Decision, res.VotesFor, res.Total, k, res.Agreed)
}

// proveRun signs a tree head and proves each step (and the optional commit leaf) offline, so a
// third party confirms with the public key alone that these votes and this outcome are committed.
func proveRun(ctx context.Context, store *agent.Journal, runID string, steps []string, toolUseID string) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, err := audit.NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		panic(err)
	}
	sth, err := audit.SignTreeHead(th, audit.Ed25519Signer{Priv: priv})
	if err != nil {
		panic(err)
	}

	fmt.Println("offline proofs (verify with the public key alone):")
	for _, name := range steps {
		b, err := audit.ProveStep(ctx, store, runID, name, sth)
		if err != nil {
			panic(err)
		}
		if err := b.Verify(audit.Ed25519Verifier{Pub: pub}); err != nil {
			panic(err)
		}
		fmt.Printf("  %-30s inclusion proof verified: %v\n", name, true)
	}
	if toolUseID != "" {
		pb, err := audit.ProveToolCall(ctx, store, runID, toolUseID, sth)
		if err != nil {
			panic(err)
		}
		if err := pb.Verify(audit.Ed25519Verifier{Pub: pub}); err != nil {
			panic(err)
		}
		fmt.Printf("  %-30s inclusion proof verified: %v\n", toolUseID, true)
	}
}

// toolResult reads the result the run journaled for the tool call toolUseID.
func toolResult(ctx context.Context, store *agent.Journal, runID, toolUseID string) json.RawMessage {
	for rec, err := range store.Records(ctx, runID) {
		if err != nil {
			log.Fatal(err)
		}
		if rec.Kind == agent.StepToolResult && rec.ToolUseID == toolUseID {
			return rec.Result
		}
	}
	log.Fatalf("run %s has no result for tool call %s", runID, toolUseID)
	return nil
}
