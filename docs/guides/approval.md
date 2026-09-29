# Approval

A tool can require a human decision before it runs. The decision is a journaled step, so it survives
a crash, and the run pauses durably until it lands. There are two forms:

- **1-of-1:** one human approves or denies (`Safety{RequiresApproval: true}` + `agent.Approve`).
- **m-of-n:** k distinct, named approvers out of a bounded set of n must sign off
  (`Safety{Approval: &agent.ApprovalPolicy{...}}` + `agent.ApproveAs`). Each decision is signed,
  and the fact that k approved before the action ran is provable offline.

For a typed value rather than a yes/no, use `Interrupt`/`Resume` instead (see the
[README](../../README.md#human-in-the-loop)).

## 1-of-1

```go
refund := agent.Func("refund", "refund the order", agent.Safety{RequiresApproval: true}, doRefund)

_, err := a.Run(ctx, runID, input)
var pend *agent.PendingApproval
if errors.As(err, &pend) {
	// ... get a human decision ...
	agent.Approve(ctx, store, runID, pend.ToolUseID, true)
	out, _ := a.Run(ctx, runID, input) // resumes past the pause
}
```

The first decision recorded for a tool call wins. A denial is fed back to the model as the tool
result `tool call denied by human`, so the model can react rather than the run failing.

## m-of-n

Declare the policy on the tool, and tell the agent how to resolve an approver id to the key that
verifies that approver's signature:

```go
refund := agent.Func("refund", "refund the order",
	agent.Safety{Approval: &agent.ApprovalPolicy{
		Need:      2,
		Approvers: []string{"ops", "finance", "risk"}, // n = 3
	}},
	doRefund)

a := agent.New(model, store, refund).
	WithApproverVerifiers(func(id string) (agent.ApproverVerifier, bool) {
		pub, ok := approverKeys[id] // your PKI: id -> public key
		if !ok {
			return nil, false
		}
		return audit.Ed25519Verifier{Pub: pub}, true
	})
```

Each approver signs the canonical decision bytes with their own key and records the decision:

```go
msg := agent.ApprovalDecisionBytes(runID, toolUseID, "finance", true)
sig, _ := financeSigner.Sign(msg) // any audit.Signer: Ed25519, ML-DSA, or hybrid
agent.ApproveAs(ctx, store, runID, toolUseID, "finance", true, sig)
```

Then re-run with the same `runID`. The gate evaluates the recorded decisions:

| Outcome | When | What happens |
|---|---|---|
| **Proceed** | approvals >= `Need` | the tool runs |
| **Deny** | `n - denials < Need` (k is no longer reachable) | the tool is skipped and the model gets the same `tool call denied by human` result as a 1-of-1 denial |
| **Pause** | otherwise | `Run` returns `*PendingApproval` with `Quorum` set to the running tally |

```go
var pend *agent.PendingApproval
if errors.As(err, &pend) && pend.Quorum != nil {
	q := pend.Quorum // Need, Approved, Denied, Pending (who has not decided, in policy order)
	fmt.Printf("%d of %d approved, waiting on %v\n", q.Approved, q.Need, q.Pending)
}
```

### What counts

A decision counts toward the tally only if all of these hold:

- the approver is in the policy's `Approvers` set (a validly signed decision from anyone else is
  ignored);
- the resolver returns a verifier for that approver, and the signature verifies over
  `ApprovalDecisionBytes(runID, toolUseID, approverID, approved)`;
- it is that approver's first decision. `ApproveAs` is idempotent per approver, so a later change of
  mind is ignored, the same rule as 1-of-1.

The signed bytes carry a version tag and length-prefixed fields, so a signature cannot be replayed
onto a different run, tool call, or approver. An `ApproveAs` decision never satisfies a 1-of-1 gate,
and `Approve` never counts toward an m-of-n one.

### Durability

Every decision is a journaled step, so decisions survive crashes and can arrive over any span of
time. When the gate reaches a terminal outcome (proceed or deny), it journals the final tally as its
own step, `approval-tally:<toolUseID>`; a resumed run reads that record instead of recounting, so the
outcome is fixed once decided even if keys rotate later. A pause writes nothing, so it stays open for
more decisions.

### Configuration errors

The gate fails with `ErrConfig` (rather than counting zero decisions) when a tool has an `Approval`
policy but no `WithApproverVerifiers` resolver is set, or when `Need` is outside
`1..len(Approvers)`.

## Proving the gate held

For a gated action, `audit.ApprovalEvidence` builds the proofs an auditor needs under one signed tree
head: one per approver who decided before the action, then the action itself.

```go
th, _ := audit.NewTreeHead(ctx, store, runID, time.Now().Unix())
sth := audit.SignTreeHead(th, logPriv)
actions, _ := audit.ApprovalEvidence(ctx, store, runID, toolUseID, policy.Approvers, sth)
```

Decisions come first (Kind `"approval"`, in the order of `approvers`), and the action comes last
(Kind `"tool"`). The result appends straight onto an `EvidencePackage`'s `Actions`. An offline
verifier holding the log key and the approvers' public keys can then check, without the store:

- each proof verifies and is bound to the same signed tree head;
- each disclosed decision's signature verifies under that approver's key;
- each decision sits at a lower journal index than the action, so it was recorded before the action
  ran;
- at least `Need` distinct eligible approvers approved.

That certifies exactly one claim: **these named approvers approved, and the action did not execute
until k of them had.** It does not certify that their judgment was right. `audit.ProveApproval`
proves a single approver's decision on its own.

## Scope

- The core does not authenticate who holds a key. Binding an approver id to a person is your identity
  provider's job; the core verifies signatures against whatever keys your resolver returns.
- The approver set is bounded and fixed per policy. There are no weighted votes, role predicates
  ("at least one from risk"), delegated approval, or deadline to resolve a gate that never reaches k.
- A declarative `plan` config can carry an `approval` block (see [Flows](flows.md#node-approval)),
  but the `plan` runtime does not enforce it yet; the gate is enforced on agent tools.

The design and its tradeoffs are in [docs/design/design-mofn-approval.md](../design/design-mofn-approval.md).
