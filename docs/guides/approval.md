# Approval

A tool can require a human decision before it runs. The decision is a journaled step, so it survives
a crash, and the run pauses durably until it lands. There are two forms:

- **1-of-1:** one human approves or denies (`Safety{RequiresApproval: true}` + `agent.Approve`).
- **m-of-n:** k distinct, named approvers out of a bounded set of n must sign off
  (`Safety{Approval: &agent.ApprovalPolicy{...}}` + `agent.ApproveAs`). Each decision is signed over
  the exact call (tool and arguments), and the fact that k approved it before it ran is provable
  offline, from evidence that cannot leave a decision out unnoticed.

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

When the run pauses, each approver signs the paused call's subject with their own key and records
the decision:

```go
var pend *agent.PendingApproval
if errors.As(err, &pend) {
	// Show the approver pend.ToolName and pend.Args: that is exactly what they sign.
	msg := agent.ApprovalDecisionBytes(pend.Subject(), "finance", true)
	sig, _ := financeSigner.Sign(msg) // any audit.Signer: Ed25519, ML-DSA, or hybrid
	agent.ApproveAs(ctx, store, pend.RunID, pend.ToolUseID, "finance", true, sig)
}
```

Then re-run with the same `runID`. The gate evaluates the recorded decisions:

| Outcome | When | What happens |
|---|---|---|
| **Proceed** | approvals >= `Need` | the tool runs |
| **Deny** | fewer approvers remain who have not validly denied than `Need` | the tool is skipped and the model gets the same `tool call denied by human` result as a 1-of-1 denial |
| **Pause** | otherwise | `Run` returns `*PendingApproval` with `Quorum` set to the running tally |

```go
if errors.As(err, &pend) && pend.Quorum != nil {
	q := pend.Quorum // Need, Approved, ApprovedBy, Denied, DeniedBy, Pending (who has not validly decided)
	fmt.Printf("%d of %d approved, waiting on %v\n", q.Approved, q.Need, q.Pending)
}
```

With `Agent.Stream`, the `ApprovalRequired` event carries the same tally in its `Quorum` field, so a
UI can show progress as the run pauses, before `Final` returns. It is nil for a 1-of-1 gate.

### What an approver signs

`ApprovalDecisionBytes` binds the run id, the call id, the tool name, a SHA-256 of the call's
arguments, the approver id, and the decision, under a version tag, with every field length-prefixed.
A signature therefore approves one exact call: it does not verify for another run, another call, the
same call id with different arguments, or another approver. If a UI showed an approver stale
arguments, their signature does not count for the real call.

The arguments are hashed in a canonical form (keys sorted, no insignificant whitespace, no HTML
escaping, number literals kept verbatim, empty arguments as `{}`), so the same call signs the same
whether its arguments came from a live model stream, the journal, or a re-indented evidence file.
The full layout is documented on `ApprovalDecisionBytes`, so a non-Go verifier can reproduce it.

### What counts

Each distinct decision is its own journal record. Over those records, in journal order, an
approver's decision is their **first valid** one: the approver is in the policy's `Approvers`, the
resolver returns their key, and the signature verifies for this call. So:

- **A bad record never locks an approver out.** A forged signature, a signature with the wrong key,
  or a record written straight into the journal is ignored, and the approver's real decision still
  counts when it arrives.
- **A later decision does not override an earlier valid one.** A change of mind is superseded, the
  same rule as 1-of-1. Resubmitting the identical decision is a no-op.
- **Only valid denials can deny.** Invalid records never count toward "k is unreachable", so they
  cannot force a denial either.
- An `ApproveAs` decision never satisfies a 1-of-1 gate, and `Approve` never counts toward an m-of-n
  one.

This rule is `agent.TallyApprovals`, a pure function the gate calls at run time and
`audit.VerifyApprovals` calls offline, so the two cannot count differently.

### Feedback at submission

By default `ApproveAs` records the decision without checking it; the gate checks every record
itself. To tell an approver immediately that their decision will not count, pass
`agent.WithDecisionCheck(resolver)`: it verifies the signature against the recorded call and returns
`agent.ErrInvalidApproval` (no such call, unknown approver, or a signature that does not verify) or
`agent.ErrAlreadyDecided` (their earlier valid decision already counts), and records nothing.
Eligibility is not checked there, because the policy lives on the tool.

### What the gate records

Every decision is a journaled step, so decisions survive crashes and can arrive over any span of
time. When the gate reaches a terminal outcome (proceed or deny), it journals the tally as its own
step, `approval-tally:<toolUseID>`, **before the tool runs**. That record carries the policy it
enforced (`Need`, `Approvers`), who approved and denied, and the name of every decision record it read
(`Records`). A resumed run reads that record instead of recounting, so the outcome is fixed once
decided even if keys rotate or the policy changes later. A pause writes nothing, so it stays open for
more decisions.

### Inside a sub-agent

An m-of-n tool inside a `SubAgent` pauses the whole tree: the parent's `Run` returns the sub-run's
`*PendingApproval`. Two details follow from where the gate runs:

- set `WithApproverVerifiers` on the **sub**-agent, since that is the agent whose tool is gated;
- approvers sign `pend.Subject()` and record against `pend.RunID`, the sub-run's id
  (`<parentRunID>/<toolUseID>`). A decision signed against the parent's run id does not verify, and
  does not lock the approver out: re-signed correctly, it counts.

### Configuration errors

The gate fails with `ErrConfig` (rather than counting zero decisions) when a tool has an `Approval`
policy but no `WithApproverVerifiers` resolver is set, or when the policy is malformed: no approvers,
an empty or duplicate approver id, or `Need` outside `1..len(Approvers)` (see
`ApprovalPolicy.Validate`).

## Proving the gate held

`audit.ApprovalEvidence` builds the complete evidence for a gated call under one signed tree head, in
journal order: the model turn that requested the call (its tool and arguments), every decision
record the gate read, valid or not, the gate's recorded tally, and the call's result.

```go
th, _ := audit.NewTreeHead(ctx, store, runID, time.Now().Unix())
sth := audit.SignTreeHead(th, logPriv)
actions, _ := audit.ApprovalEvidence(ctx, store, runID, toolUseID, sth)
```

The result appends onto an `EvidencePackage`'s `Actions`; drop the trailing result entry if the
package already carries the call.

An auditor checks it offline with `audit.VerifyApprovals`, holding only the evidence, the policy they
expect, the approvers' public keys (the same resolver shape the gate uses), and the log key:

```go
v, err := audit.VerifyApprovals(pkg.Actions, toolUseID, policy, approverVerifiers, logPub)
// v.OK: the evidence is consistent and at least Need approved.
// v.ToolName, v.Args: the exact call that was approved.
// v.Counted, v.DeniedBy: who. v.Ignored: every other decision, with the reason.
// v.Problems: every inconsistency found (any problem fails the verdict).
```

It verifies the request, tally, and result proofs under one tree head and in that order, then
recounts the disclosed decisions with `agent.TallyApprovals` against the proven call. It reports a
problem when:

- the evidence **omits** a decision the gate read (the tally names every one), so an earlier valid
  denial cannot be hidden to make a later approval count;
- the recount **disagrees** with the gate's recorded tally, for example because the auditor's keys do
  not reproduce the gate's count;
- the gate **enforced a different policy** than the auditor expects, for example `Need: 1` where
  `Need: 2` was required;
- the request, decisions, tally, and result are out of order.

Tampering with the request, the tally, or the result breaks their proofs, and `VerifyApprovals`
returns an error rather than a verdict. A denied gate yields consistent evidence whose verdict is not
OK and names who denied.

That certifies exactly one claim: **these named approvers approved this exact call, and it did not
execute until k of them had, under the expected policy, from evidence that leaves nothing out.** It
does not certify that their judgment was right. `audit.ProveApproval` proves a single decision by its
record name.

### From the command line

For an auditor who does not write Go, `bide-audit verify-approvals` runs the same check from the files
alone, with approver keys as a JSON object of id to ed25519 public key hex:

```
bide-audit verify-approvals -evidence evidence.json -pubkey <log key hex> -call refund-1 \
    -need 2 -approvers ops,finance,risk -approver-keys approver-keys.json
```

It prints the approved call, each approver who counted, every ignored decision with its reason, and
every problem, and exits 1 unless the gate held. `bide-audit verify-evidence` checks the proofs only,
so it cannot detect an omitted decision; use `verify-approvals` for the approval claim.

Evidence files commit to the exact recorded bytes. Keep them byte-exact: a tool that re-orders JSON
object keys inside a recorded message breaks that record's proof.

## Scope

- The core does not authenticate who holds a key. Binding an approver id to a person is your identity
  provider's job; the core verifies signatures against whatever keys your resolver returns.
- The approver set is bounded and fixed per policy. There are no weighted votes, role predicates
  ("at least one from risk"), delegated approval, or deadline to resolve a gate that never reaches k.
- The tally does not record which key verified each approver. An auditor needs the keys that were
  valid when the gate decided; after a rotation, keep the old public keys to verify old evidence.
- A declarative `plan` config can carry an `approval` block (see [Flows](flows.md#node-approval)),
  but the `plan` runtime does not enforce an approval gate yet, so building such a flow fails with
  `ErrConfig` naming the node (as does a `Tool` node wrapping an agent tool that requires approval)
  rather than letting the node run unapproved. Put the gate on an agent tool.

## Runnable example

`examples/approval` runs the whole flow across separate processes on a SQLite journal: the run pauses
and exits, approvers record signed decisions from their own processes, an ineligible approver and a
forged signature are ignored without locking anyone out, `-check` refuses a bad decision at
submission, the refund runs exactly once at 2 of 3, and an auditor verifies the exported evidence
with public keys only (`cd examples/approval && go run .`). Its test drives every actor as a separate
process and checks the evidence with `bide-audit verify-evidence` and `verify-approvals`, including a
tampered file and one with a decision left out.

The design and its tradeoffs are in [docs/design/design-mofn-approval.md](../design/design-mofn-approval.md).
