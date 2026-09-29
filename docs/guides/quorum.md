# Model quorum

Some decisions are too costly to leave to one model: approving a large refund, flagging an account,
executing a trade. A quorum asks several models the same question and acts only when enough of them
agree. When they disagree, the decision goes to a person or a safe default instead of whichever
model answered first.

Every vote and the final tally are recorded durably, so a crash never re-asks a model that already
voted, and anyone can later verify, offline, exactly how the decision was reached.

## When to use it

| You want | Use |
|---|---|
| A fast answer, even when a provider is slow or down | [Hedging](reliability.md#when-to-hedge-vs-retry): the first good answer wins |
| A decision several independent models agree on | A quorum |
| A person to sign off | [Approval](approval.md); combine it with a quorum to escalate disagreements |

A quorum costs one model call per voter and waits for all of them, so reserve it for the few
decisions that are expensive or hard to undo.

## Quick start

Give each voter a name and a `Decide` function that asks one model and returns a short label.
`govern.Quorum` runs the voters in parallel, tallies their answers, and tells you whether at least
`k` of them agreed.

```go
// Each model answers with one label from a fixed set.
type verdict struct {
    Decision string `json:"decision"` // "approve", "deny", or "escalate"
}

func voter(name string, a *agent.Agent, ticket string) govern.Voter {
    return govern.Voter{
        Name: name,
        Decide: func(ctx context.Context) (string, error) {
            v, err := agent.RunTypedNative[verdict](ctx, a, "refund-1234/vote/"+name, ticket)
            return v.Decision, err
        },
    }
}

res, err := govern.Quorum(ctx, store, "refund-1234", 2,
    voter("claude", claudeAgent, ticket),
    voter("gpt", gptAgent, ticket),
    voter("gemini", geminiAgent, ticket),
)
if err != nil {
    return err // a voter failed; run again with the same run ID to finish (see below)
}
if res.Agreed {
    return apply(res.Decision) // at least 2 of the 3 chose res.Decision
}
return escalate(res) // no agreement: hand it to a person
```

[`examples/quorum`](../../examples/quorum/main.go) is a runnable, offline version with both
outcomes: agreement, where the decision commits, and a split, where it escalates.

## Reading the result

| Field | Meaning |
|---|---|
| `Decision` | The decision chosen by the most voters |
| `VotesFor` | How many voters chose `Decision` |
| `Total` | How many voters voted |
| `Agreed` | `true` when `VotesFor >= k` and no other decision has as many votes |
| `Votes` | Every vote, with the voter's name, in voter order |

A tie for the most votes is never agreement. With four voters and `k = 2`, a 2-2 split has
`Agreed == false`: both decisions reach `k`, but neither has more support than the other.

## Choosing voters and k

- **Prefer different providers.** Three different model families catch more mistakes than one
  model asked three times, because their errors are less likely to line up. Asking the same model
  repeatedly only smooths out its randomness.
- **Majority or unanimity.** `k` greater than half the voters (2 of 3, 3 of 5) is the usual choice.
  `k` equal to the number of voters is the most cautious and escalates more often. A `k` of half or
  less lets two different decisions both reach `k`; the tie rule above keeps that from counting as
  agreement.
- **Keep the answers comparable.** Votes are compared as exact strings, so each model must answer
  from a small fixed set of labels. Use structured output (`RunTypedNative`, a response format, or
  a forced tool choice), and normalize anything else before returning it. Open-ended text cannot be
  put to a quorum.

## Rules for voters

- **`Name` must be unique** within one call. It identifies the voter in the record, and it is the
  key the vote is stored under.
- **`Decide` must only decide.** It may be run again if the process stops before its vote is
  recorded, so it must not send, charge, or change anything. Act on `res.Decision` afterwards.
- **Give each voter's agent run its own run ID**, as in the example (`refund-1234/vote/claude`), so
  each model's reasoning is kept in a separate journal.

## Crashes, retries, and failed voters

Each vote and the tally are durable steps. If the process stops partway, call `govern.Quorum`
again with the same run ID: votes already recorded are replayed, not asked again, and only the
missing voters run. The tally comes out the same.

If a voter returns an error, `Quorum` returns that error together with the tally of the votes that
did arrive. The failed voter is asked again the next time you call `Quorum` with the same run ID.

## When the models disagree

Decide in advance what a failed quorum means, and make it the same every time:

- **Ask a person.** Route the decision to a tool marked `agent.Safety{RequiresApproval: true}`, so
  the run pauses until someone approves it (see [Approval](approval.md)).
- **Fall back to a safe default,** such as "deny" or "hold for review".

Either way, the split vote itself is part of the record.

## Making the gate enforceable

For decisions that must provably never commit without agreement, express the rule as governed
state (see [Governance](governance.md)). The tally is fed in as a fact, and a checked policy
refuses the commit unless the count reaches `k`:

```go
decision := r.Enum("decision", "approve", "deny", "escalate")
votesFor := r.Int("votes_for", 0, n)
committed := r.Bool("committed")

// A decision may be committed only with k votes; otherwise it becomes "escalate".
r.Rule("quorum_required").
    Require(gsm.Or(gsm.Is(committed, 0), gsm.Ge(gsm.V(votesFor), gsm.Lit(k)))).
    RepairWith(gsm.SetLabel(decision, "escalate")).
    Add()

// The commit event does nothing unless the quorum is met.
r.On("commit").OnlyIf(gsm.Ge(gsm.V(votesFor), gsm.Lit(k))).Does(gsm.SetBoolTrue(committed)).Add()
```

The policy is checked for every possible vote count, so the gate holds however the models vote.
Commit through `govern.AttestedEventTool` to record the commit bound to the policy it ran under.
`examples/quorum` shows the whole wiring.

## Verifying a decision offline

Anyone holding the published proofs can check the decision without access to your systems. Export
a proof for the tally and for every vote with `audit.ProveStep`, signed under the run's tree head,
then run:

```bash
bide-audit verify-quorum \
  -tally tally.json \
  -vote claude.json -vote gpt.json -vote gemini.json \
  -pubkey <signing key> -k 2 \
  [-commit commit.json]
```

It fails unless all of the following hold:

- every proof is authentic and from the same signed run;
- every vote is disclosed, and the tally recomputes exactly from those votes, so a forged tally is
  caught;
- a single decision has the most votes, and at least `k` of them;
- with `-commit`, the commit is recorded in the same run.

## What a quorum does not do

- **It does not make the answer correct.** The gate is enforced exactly: nothing commits without
  `k` agreeing votes. But models trained on similar data can be wrong together, so agreement lowers
  the risk of a single model's mistake without guaranteeing the decision.
- **It is for important decisions only.** Every gated decision costs one call per voter, and waits
  for the slowest voter.
- **It needs short, fixed answers.** It does not apply to free-form generation.

## How it works

A quorum is not a special kind of agent; it is built from pieces bide already has. The voters run
with `agent.Parallel`, so each vote is a durable step. The tally is plain Go, recorded as a step of
its own. Escalation uses the approval gate, and enforcement uses governed state. Because each piece
is durable and recorded, the quorum inherits crash safety and offline verification without any
machinery of its own.
