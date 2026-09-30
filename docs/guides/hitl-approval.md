# Human approval (human-in-the-loop)

A tool can require a human decision before it runs. The decision is a journaled step, so it survives
a crash, and the run pauses durably until it lands. There are two forms:

- **1-of-1:** one human approves or denies (`Safety{RequiresApproval: true}` + `agent.Approve`).
- **m-of-n:** k distinct, named approvers out of a bounded set of n must sign off
  (`Safety{Approval: &agent.ApprovalPolicy{...}}` + `agent.SubmitDecision`). Each decision is signed over
  the exact call (tool and arguments), and the fact that k approved it before it ran is provable
  offline, from evidence that cannot leave a decision out unnoticed.

For a typed value rather than a yes/no, use `Interrupt`/`AnswerInterrupt` instead (see the
[README](../../README.md#human-in-the-loop)).

## 1-of-1

<!-- docsnip: setup ctx context.Context; a *agent.Agent; store agent.Durable; runID string; input string; type RefundArgs struct{}; doRefund func(context.Context, RefundArgs) (string, error) -->
```go
refund := agent.Func("refund", "refund the order", agent.Safety{RequiresApproval: true}, doRefund)

_, err := a.Run(ctx, runID, input)
if pend, ok := errors.AsType[*agent.ApprovalPending](err); ok {
	// ... get a human decision ...
	agent.Approve(ctx, store, pend.RunID, pend.ToolUseID, true)
	out, _ := a.Run(ctx, pend.RootRunID, input) // resumes past the pause
}
```

The first decision recorded for a tool call wins. A denial is fed back to the model as the tool
result `tool call denied by human`, so the model can react rather than the run failing.

A recorded denial is final even if the tool's gate is removed or loosened before the run is driven
again: the call is denied, not run. An approval is not carried over the same way. While a gate is
configured, the call's decision is taken under the gate's current policy, so a gate tightened (from
1-of-1 to m-of-n, say) before the call ran asks again.

## m-of-n

Declare the policy on the tool, and tell the agent how to resolve an approver id to the key that
verifies that approver's signature:

<!-- docsnip: setup model agent.Model; store agent.Durable; type RefundArgs struct{}; doRefund func(context.Context, RefundArgs) (string, error); approverKeys map[string]ed25519.PublicKey -->
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

Each approver needs a key of their own. A seat is a key, not an id: if two approvers' verifiers
resolve to one key, whoever holds it can sign as both, so the gate refuses that policy with
`ErrConfig` (see [Configuration errors](#configuration-errors)). The gate tells keys apart with
`ApproverVerifier.KeyIDs`, which the `audit` verifiers derive from the public key's bytes; a
verifier of your own must implement it the same way (see [Key identity](#key-identity)).

When the run pauses, each approver signs the paused call's subject with their own key and records
the decision:

<!-- docsnip: setup ctx context.Context; store agent.Durable; err error; financeSigner audit.Signer -->
```go
if pend, ok := errors.AsType[*agent.ApprovalPending](err); ok {
	// Show the approver pend.ToolName and pend.Args: that is exactly what they sign.
	msg := agent.ApprovalDecisionBytes(pend.Subject(), "finance", true)
	sig, _ := financeSigner.Sign(msg) // any audit.Signer: Ed25519, ML-DSA, or hybrid
	agent.SubmitDecision(ctx, store, agent.Decision{RunID: pend.RunID, ToolUseID: pend.ToolUseID,
		ApproverID: "finance", Approved: true, Signature: sig})
}
```

Then re-run `pend.RootRunID`. The gate evaluates the recorded decisions:

| Outcome | When | What happens |
|---|---|---|
| **Proceed** | approvals >= `Need` | the tool runs |
| **Deny** | fewer approvers remain who have not validly denied than `Need` | the tool is skipped and the model gets the same `tool call denied by human` result as a 1-of-1 denial |
| **Pause** | otherwise | `Run` returns `*ApprovalPending` with `Quorum` set to the running tally |

<!-- docsnip: setup err error; pend *agent.ApprovalPending -->
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
resolver returns their key, no other approver's verifier reports that key, and the signature verifies
for this call. So:

- **A bad record never locks an approver out.** A forged signature, a signature with the wrong key,
  or a record written straight into the journal is ignored, and the approver's real decision still
  counts when it arrives.
- **A later decision does not override an earlier valid one.** A change of mind is superseded, the
  same rule as 1-of-1. Resubmitting the identical decision is a no-op.
- **Only valid denials can deny.** Invalid records never count toward "k is unreachable", so they
  cannot force a denial either.
- **One key is one seat.** An approver whose verifier shares a key identity with another approver's,
  or reports none, never counts (`agent.ReasonSharedKey`, `agent.ReasonNoKeyID`), and neither does the
  other approver on that key, whichever signed first. The gate refuses such a policy before it
  counts; the counting rule excludes those seats as well, so a resolver that answers differently
  between the check and the count still cannot seat one person twice.
- A `SubmitDecision` decision never satisfies a 1-of-1 gate, and `Approve` never counts toward an m-of-n
  one.

This rule is `agent.TallyApprovals`, a pure function the gate calls at run time and
`audit.VerifyApprovals` calls offline, so the two cannot count differently.

### Feedback at submission

By default `SubmitDecision` records the decision without checking it; the gate checks every record
itself. To tell an approver immediately that their decision will not count, pass
`agent.WithDecisionCheck(resolver)`: it verifies the signature against the recorded call and returns
`agent.ErrInvalidApproval` (no such call, unknown approver, or a signature that does not verify) or
`agent.ErrAlreadyDecided` (their earlier valid decision already counts), and records nothing.
Eligibility is not checked there, because the policy lives on the tool.

### What the gate records

Every decision is a journaled step, so decisions survive crashes and can arrive over any span of
time. When the gate reaches a terminal outcome (proceed or deny), it journals the tally as its own
step, `approval-tally:<encoded toolUseID>` (`agent.ApprovalTallyStep`), **before the tool runs**. That record carries the policy it
enforced (`Need`, `Approvers`), who approved and denied, and the name of every decision record it read
(`Records`). A resumed run reads that record instead of recounting, so the outcome is fixed once
decided even if keys rotate or the policy changes later. A pause writes nothing, so it stays open for
more decisions.

### Inside a sub-agent

An m-of-n tool inside a `SubAgent` pauses the whole tree: the parent's `Run` returns the sub-run's
`*ApprovalPending`. Two details follow from where the gate runs:

- set `WithApproverVerifiers` on the **sub**-agent, since that is the agent whose tool is gated;
- approvers sign `pend.Subject()` and record against `pend.RunID`, the sub-run's id
  (`agent.SubRunID(parentRunID, toolUseID)`, `<parentRunID>><encoded toolUseID>`). A decision signed against the parent's run id does not verify, and
  does not lock the approver out: re-signed correctly, it counts.

### Configuration errors

The gate fails with `ErrConfig` (rather than counting zero decisions) when a tool has an `Approval`
policy but no `WithApproverVerifiers` resolver is set, or when the policy is malformed: no approvers,
an empty or duplicate approver id, an id that is not valid UTF-8, or `Need` outside
`1..len(Approvers)` (see `ApprovalPolicy.Validate`).

Approver ids are compared as exact bytes, but a policy may not list two ids that differ only by
case or Unicode normalization (`alice` and `Alice`, an NFC and an NFD `café`, a fullwidth and an
ASCII spelling). They are compared under NFKC case folding, and such a policy is refused as
ambiguous with `ErrConfig` naming both ids: a reader of the policy, or a key lookup that folds
case, would take them for one approver, who could then fill two seats.

Distinct ids are not enough: the gate also refuses, with `ErrConfig` naming both approvers and the
key, a policy two of whose approvers' verifiers report a common key identity, and a policy with an
approver whose verifier reports no key identity (an empty `KeyIDs`, or an empty entry in it). The
holder of a shared key could otherwise sign as each approver it serves and meet the quorum alone.
An approver the resolver does not know is not refused: their decisions cannot verify, so they fill
no seat. The gate runs this check on every evaluation, before it reads a recorded tally, so a
resolver changed between one resume and the next is checked again. The check guards the tallies
this version counts and records. A terminal tally already in the journal is reused, not recounted
(see [What the gate records](#what-the-gate-records)), so a tally recorded by an earlier version,
which did not compare keys, stands as recorded even if it counted two approvers on one key. Finish
or audit such runs before relying on this check for them (see the CHANGELOG's upgrade note).
`ApprovalPolicy.ValidateKeys(resolver)` runs the same check (with `Validate`), so a deployment can
refuse a bad pairing of policy and keys at startup, before any call pauses.

### Key identity

`ApproverVerifier` has two methods: `Verify`, and `KeyIDs() []string`, the identities of the keys
behind `Verify`. An identity names a key, not an approver: derive it from the public key's bytes,
never from a configured name, so that two verifiers built over one key report the same identity. The
`audit` verifiers report the scheme and the hex SHA-256 of the public key's encoding
(`audit.KeyID`, for example `ed25519:3b6a27bc...`):

- `audit.Ed25519Verifier` and `audit.MLDSAVerifier` report one identity. The ML-DSA label names the
  key's parameter set (`ml-dsa-44`, `ml-dsa-65` or `ml-dsa-87`). A key that verifies nothing (an
  Ed25519 key of the wrong length, a nil ML-DSA key) reports none, so the gate refuses it rather
  than seating it.
- `audit.HybridVerifier` reports both component keys. A hybrid signature is meant to hold while either
  scheme holds, so if one scheme breaks, the other component's key alone signs: two approvers sharing
  either component are one seat.
- A verifier of your own that accepts a signature by any of several keys (a rotation window) reports
  every one of them, so that two approvers whose key sets overlap are refused.

Two identities are the same key only when the strings are equal. The gate cannot tell that one person
holds two different keys; see [Scope](#scope).

**Trust boundary.** A verifier is trusted code, like the resolver that returns it. The gate takes
`KeyIDs` on faith: it cannot check that the identities name the keys `Verify` really accepts, so a
verifier that under-reports its keys, or reports identities not derived from them, defeats the check.
Use the `audit` verifiers, or derive identities the same way.

**It fails closed.** Enrolling one approver's public key under a second approver (a copy-paste in
the key directory, say) makes the gate refuse the whole policy with `ErrConfig`, not just the second
approver: no call under that policy runs, and paused calls stay paused, until the keys are corrected.
A resolver that returns a typed nil verifier (a nil pointer in a non-nil interface) is refused the
same way, and so is a key a verifier cannot use (it reports no identity). If `TallyApprovals` is
handed such a resolver directly, the approvers it cannot seat are listed in the tally's `Excluded`,
never count, and are not waited on: when too few seats remain to reach `Need`, the gate denies.

## Proving the gate held

`audit.ApprovalEvidence` builds the complete evidence for a gated call under one signed tree head, in
journal order: the model turn that requested the call (its tool and arguments), every decision
record the gate read, valid or not, the gate's recorded tally, and the call's result.

<!-- docsnip: setup ctx context.Context; store agent.Durable; runID string; toolUseID string; logPriv ed25519.PrivateKey -->
```go
th, _ := audit.NewTreeHead(ctx, store, runID, time.Now().UnixNano())
sth := audit.SignTreeHead(th, logPriv)
actions, _ := audit.ApprovalEvidence(ctx, store, runID, toolUseID, sth)
```

The result appends onto an `EvidencePackage`'s `Actions`; drop the trailing result entry if the
package already carries the call.

An auditor checks it offline with `audit.VerifyApprovals`, holding only the evidence, the policy they
expect, the approvers' public keys (the same resolver shape the gate uses), and the log key:

<!-- docsnip: setup pkg audit.EvidencePackage; toolUseID string; policy agent.ApprovalPolicy; approverVerifiers agent.ApproverVerifierFor; logPub ed25519.PublicKey -->
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
returns an error rather than a verdict; so does a resolver under which two of the policy's approvers
share a key. A denied gate yields consistent evidence whose verdict is not
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
every problem. It exits 0 only when the gate held: 1 when it did not, 4 when an input cannot be read
or used (including a key file that gives two of the policy's approvers one key), and 2 on a usage error (see the audit guide's
[exit status](audit.md#exit-status); only 0 means verified). `bide-audit verify-evidence` checks the proofs only,
so it cannot detect an omitted decision; use `verify-approvals` for the approval claim.

Evidence files commit to the exact recorded bytes. Keep them byte-exact: a tool that re-orders JSON
object keys inside a recorded message breaks that record's proof.

## Scope

- The core does not authenticate who holds a key. Binding an approver id to a person is your identity
  provider's job; the core verifies signatures against whatever keys your resolver returns. It refuses
  two approvers on one key, but it cannot see that one person holds two different keys: issuing each
  person one key is the identity provider's job too.
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
