# Human approval (human-in-the-loop)

A tool can require a human decision before it runs. The decision is a journaled step, so it survives
a crash, and the run pauses durably until it lands. There are two forms:

- **1-of-1:** one human approves or denies (`agent.WithApproval(agent.SingleApproval())` + `agent.Approve`).
- **m-of-n:** k distinct, named approvers out of a bounded set of n must sign off
  (`agent.WithApproval(&agent.ApprovalPolicy{...})` + `agent.SubmitDecision`). Each decision is signed over
  the exact call (tool and arguments), and the fact that k approved it before it ran is provable
  offline, from evidence that cannot leave a decision out unnoticed.

The gate is part of the tool's spec (`ToolSpec.Approval`), not of its `Safety`, which only says how
a call may be retried. A `SubAgent` takes `WithApproval` too, so a parent can require approval before
it delegates. Each call's result journals the policy it ran under (`Record.Approval`).

Approvers can decide from a terminal with [`bide approve`](#deciding-from-the-terminal).

For a typed value rather than a yes/no, use `Interrupt`/`AnswerInterrupt` instead (see the
[tour](../tour.md#human-in-the-loop)).

## 1-of-1

<!-- docsnip: setup ctx context.Context; a *agent.Agent; journal *agent.Journal; runID string; input string; type RefundArgs struct{}; doRefund func(context.Context, RefundArgs) (string, error) -->
```go
refund := agent.MustFunc("refund", "refund the order", doRefund, agent.WithApproval(agent.SingleApproval()))

_, err := a.Run(ctx, runID, agent.UserText(input))
if pend, ok := errors.AsType[*agent.ApprovalPending](err); ok {
	// ... get a human decision ...
	agent.Approve(ctx, journal, pend.RunID, pend.ToolUseID, true)
	res, _ := a.Run(ctx, pend.RootRunID, agent.UserText(input))
	var out agent.Message
	if res != nil {
		out = res.Message
	} // resumes past the pause
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

<!-- docsnip: setup model agent.Model; journal *agent.Journal; type RefundArgs struct{}; doRefund func(context.Context, RefundArgs) (string, error); approverKeys map[string]ed25519.PublicKey -->
```go
refund := agent.MustFunc("refund", "refund the order", doRefund,
	agent.WithApproval(&agent.ApprovalPolicy{
		Need:      2,
		Approvers: []string{"ops", "finance", "risk"}, // n = 3
	}))

a, err := agent.New(
	model,
	journal,
	agent.WithTools(refund),
	agent.WithApproverVerifiers(func(id string) (agent.ApproverVerifier, bool) {
			pub, ok := approverKeys[id] // your PKI: id -> public key
			if !ok {
				return nil, false
			}
			return audit.Ed25519Verifier{Pub: pub}, true
		}),
)
if err != nil {
	panic(err)
}
```

Each approver needs a key of their own. A seat is a key, not an id: if two approvers' verifiers
resolve to one key, whoever holds it can sign as both, so the gate refuses that policy with
`ErrConfig` (see [Configuration errors](#configuration-errors)). The gate tells keys apart with
`ApproverVerifier.KeyIDs`, which the `audit` verifiers derive from the public key's bytes; a
verifier of your own must implement it the same way (see [Key identity](#key-identity)).

When the run pauses, each approver signs the paused call's subject with their own key and records
the decision:

<!-- docsnip: setup ctx context.Context; journal *agent.Journal; err error; financeSigner audit.Signer -->
```go
if pend, ok := errors.AsType[*agent.ApprovalPending](err); ok {
	// Show the approver pend.ToolName and pend.Args: that is exactly what they sign.
	decidedAt := time.Now() // when the approver decided, by their clock; signed with the decision
	msg := agent.ApprovalDecisionBytes(pend.Subject(), "finance", true, decidedAt)
	sig, _ := financeSigner.Sign(msg) // any audit.Signer: Ed25519, ML-DSA, or hybrid
	agent.SubmitDecision(ctx, journal, agent.Decision{RunID: pend.RunID, ToolUseID: pend.ToolUseID,
		ApproverID: "finance", Approved: true, DecidedAt: decidedAt, Alg: financeSigner.Alg(), Signature: sig})
}
```

`Decision.DecidedAt` is when the approver decided, by their own clock, and is required: pass the
same time to `ApprovalDecisionBytes` and to `Decision`, since the signature covers it (see [When a
decision was made](#when-a-decision-was-made)). `Decision.Alg` names the scheme the signature is
under and is required too (`SubmitDecision` refuses an empty one, or a zero `DecidedAt`, with
`agent.ErrConfig`, and so an `ApproverID` that is not printable ASCII, which no policy can name). It is journaled with the decision as `Record.ApproverAlg`, and the
gate counts the decision only if it equals the `Alg()` of the verifier your resolver returns for
that approver: a valid signature journaled under another scheme name, or none, is ignored with
`agent.ReasonAlg`. One key fills one seat (see [Key identity](#key-identity)), and `bide audit
verify-approvals` also refuses an approver-key file that gives two ids one key, compared by key
identity, whether or not both ids are in the policy (exit 4).

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
arguments, the approver id, the decision, and the time the approver decided, under a version tag
(`bide.approval.v3`), with every field length-prefixed.
A signature therefore approves one exact call: it does not verify for another run, another call, the
same call id with different arguments, or another approver. If a UI showed an approver stale
arguments, their signature does not count for the real call.

The arguments are hashed in a canonical form (keys sorted, no insignificant whitespace, no HTML
escaping, number literals kept verbatim, empty arguments as `{}`), so the same call signs the same
whether its arguments came from a live model stream, the journal, or a re-indented evidence file.
The full layout is documented on `ApprovalDecisionBytes`, so a non-Go verifier can reproduce it:

```
"bide.approval.v3\n"
len32(run id)       run id
len32(call id)      call id
len32(tool name)    tool name
SHA-256(canonical arguments), 32 bytes
len32(approver id)  approver id
1 byte              1 approved, 0 denied
len32(decided_at)   decided_at, 24 bytes, e.g. "2026-10-13T12:14:03.123Z"
```

`len32` is a 4-byte big-endian length. The version tag is part of the signed bytes, so a decision
signed under an earlier version (`bide.approval.v2`, which had no decision time) does not verify:
the gate and `VerifyApprovals` ignore it with `agent.ReasonNoDecidedAt`. Re-sign such a decision.

The decision time is signed in one form only: RFC 3339 in UTC, to the millisecond, always ending in
`Z` (`agent.FormatTimestamp`, the same form as a run's `started_at`). A verifier parses the
recorded text, formats it again, and refuses the decision (`agent.ReasonDecidedAt`) unless the two
are byte for byte the same, so `+00:00`, another offset, one or nine fractional digits, no fraction,
a lowercase `t` or `z`, and a leap second (`23:59:60`) are all refused. One instant therefore has one
signed spelling. An approver who sends a decision over the wire sends this text, and
`agent.ParseTimestamp` reads it back.

### When a decision was made

Every decision carries two times, kept apart on purpose:

| Time | Whose clock | How it is protected |
|---|---|---|
| `decided_at` (`Record.DecidedAt`) | the approver's, when they signed | inside the signature: it cannot be changed after signing, but it is the approver's claim, and their clock may be wrong |
| `recorded_at` (`Record.RecordedAt`) | the submitter's: the process that called `SubmitDecision`, when it journaled the decision (its `agent.WithClock`, else the run's clock on its context, else its wall clock); an identical resubmission keeps the first time | not signed: it is the submitter's claim. Anchored by the journal's Merkle trail, with the record, once a signed tree head covers it; the audit flags one later than that head (`audit.FlagRecordedAfterHead`) or out of journal order (`audit.FlagRecordedOutOfOrder`) |

Both are for people and auditors. **Neither affects the gate**: `TallyApprovals` checks
`decided_at` only as signed bytes (present, canonical, covered by the signature), never reads its
value or `recorded_at`, and orders decisions by their place in the journal alone. The same
decisions count the same way whatever their times say.

`audit.VerifyApprovals` reports both times for every counted decision (`ApprovalVerdict.Times`),
and when the run asked for approval (`AskedAt`: on a plan flow node, the `recorded_at` of the
gate's request, which the ordering rule counts from; otherwise, or when the request has none, that
of the gate's `approval-pending` record). It flags, without failing the verdict or uncounting
anything:

- `decided_at` more than a tolerance after `recorded_at` (`audit.FlagDecidedAfterRecorded`; 5
  minutes by default, `audit.DefaultDecisionTimeTolerance`, or any other with
  `audit.CheckDecisionTimes`): a decision cannot be recorded before it is made, so the approver's
  clock runs ahead;
- `decided_at` before the run asked (`audit.FlagDecidedBeforeAsked`): the approver's clock runs
  behind, or the decision was signed in advance. The gate never counts a decision by its time: it
  reads only decisions on this exact call, and on a plan flow node only those recorded after the
  gate's request (`SubmitDecision` refuses one recorded before it). So a flagged decision was still
  recorded after the request; the flag says its signed time disagrees;
- no ask time in the evidence (`audit.FlagAskedUnknown`: a run an earlier version recorded, or a
  pause record lost to a crash), so the check above was not made;
- `recorded_at` missing or not a canonical time (`audit.FlagRecordedAtInvalid`): every report
  prints it quoted, as text;
- `recorded_at` more than the tolerance before an earlier decision's, or before the run asked
  (`audit.FlagRecordedOutOfOrder`), or after the signed tree head that covers the record
  (`ApprovalVerdict.HeadAt`, `audit.FlagRecordedAfterHead`): the journal's order, or the head,
  says the submitter's time is wrong.

Times run from the year 0001 to 9999; a decision time outside them has no canonical form.

The gap between the two times is shown, not judged. `bide audit verify-approvals` prints all of it,
with `-decision-time-tolerance` to change the tolerance, and `bide view` shows it on each approver's
card.

A later version may add an RFC 3161 trusted timestamp to each decision, a third time vouched for by
a timestamp authority rather than by the approver or by the submitter. It is not part of this
version.

### What counts

Each distinct decision is its own journal record. Over those records, in journal order, an
approver's decision is their **first valid** one: the approver is in the policy's `Approvers`, the
resolver returns their key, no other approver's verifier reports that key, the record carries a
signed decision time in canonical form, and the signature verifies for this call. So:

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
`agent.ErrInvalidApproval` (no such call, unknown approver, a scheme that is not the approver key's,
or a signature that does not verify) or
`agent.ErrAlreadyDecided` (their earlier valid decision already counts), and records nothing.
Eligibility is not checked there, because the policy lives on the tool.

### What the gate records

Every decision is a journaled step, so decisions survive crashes and can arrive over any span of
time. When the gate reaches a terminal outcome (proceed or deny), it journals the tally as its own
step, `approval-tally:<encoded toolUseID>` (`agent.ApprovalTallyStep`), **before the tool runs**. That record carries the policy it
enforced (`Need`, `Approvers`), who approved and denied, and the name of every decision record it read
(`Records`). A resumed run reads that record instead of recounting, so the outcome is fixed once
decided even if keys rotate or the policy changes later. A pause records no decision and no tally, so
it stays open for more decisions. It does record that the run waits:
`approval-pending:<encoded toolUseID>` (`agent.ApprovalPendingStep`), an `agent.ApprovalWait` with the
call's ID and tool, the gate's `Need` and `Approvers` (or `Single`), and when the run paused
(`RecordedAt`, by the run's clock), so a reader such as `bide view` can list the gate before anyone
decides, and an auditor can see when approvers were asked. The run writes it once, in the drive whose model turn made
the call, and a resume writes nothing. It is informational only: the gate, a resume and recovery never
read it, and a failed write of it is dropped.

### A run approved under an earlier decision format

A run that an earlier version paused at an m-of-n gate, approved under `bide.approval.v2` (decisions
with no signed time), and stopped before the call ran, journaled a tally that passed on decisions
this version does not accept. Resumed now, the gate does not run the call on that tally: it asks
again. `Run` returns `*ApprovalPending` with `Reason` set to `agent.PendingReapproval` and a fresh
count (`Quorum`, whose `Supersedes` names the old tally), and `bide view` shows the gate as waiting
for approval again. Approvers sign again under `bide.approval.v3` with `SubmitDecision`; once the
gate passes it records the new outcome under `approval-tally:<encoded toolUseID>:v3`
(`agent.ApprovalRetallyStep`), which every later drive, `audit.ApprovalEvidence` and
`audit.VerifyApprovals` read in place of the old tally, and the call runs. A tally an earlier
version recorded as a denial stays final, since it runs nothing, and a call that already ran under
the earlier version is not asked again.

### Inside a sub-agent

An m-of-n tool inside a `SubAgent` pauses the whole tree: the parent's `Run` returns the sub-run's
`*ApprovalPending`. Two details follow from where the gate runs:

- set `WithApproverVerifiers` on the **sub**-agent, since that is the agent whose tool is gated;
- approvers sign `pend.Subject()` and record against `pend.RunID`, the sub-run's id
  (`agent.SubRunID(parentRunID, toolUseID)`, `<parentRunID>><encoded toolUseID>`). A decision signed against the parent's run id does not verify, and
  does not lock the approver out: re-signed correctly, it counts.

### Configuration errors

A malformed policy is refused when the tool is built: `agent.WithApproval` (on `Func`,
`CompensatedFunc`, `SubAgent` and the other constructors that take tool options) panics with
`ErrConfig`, and `New` refuses one a tool's own `Spec` returns (every run fails with `ErrConfig`
before any model call). A policy is malformed when it has no approvers (only
`agent.SingleApproval()` asks for the one-decision gate; a policy literal such as `{Need: 1}` with an
empty approver list is refused, never taken for it), an empty or duplicate approver id, an id that
is not printable ASCII, or `Need` outside `1..len(Approvers)` (see `ApprovalPolicy.Validate`), or when it
is a `SingleApproval()` whose fields were changed.

Verifiers are needed only for an m-of-n gate: the gate fails with `ErrConfig` (rather than counting
zero decisions) when a tool has an m-of-n policy but no `WithApproverVerifiers` resolver is set, or
when an eligible approver's verifier has an empty `PublicKey()` (see above). A `SingleApproval()`
gate needs none.

Approver ids are printable ASCII: bytes 0x21 to 0x7E, so letters, digits and punctuation (an email
address such as `ops@example.com` or a URI is a valid id), and no space, control character or
character outside ASCII. An id outside that set could look like another one, a Cyrillic `о` in
place of a Latin `o`, a fullwidth or an accented spelling, and let one approver fill two seats, so
it is refused with `ErrConfig` naming it. Ids are compared as exact bytes, but a policy may not list
two that differ only by case (`alice` and `Alice`): a reader of the policy, or a key lookup that
folds case, would take them for one approver, so such a policy is refused naming both ids. Map
names in other scripts to ASCII ids in your directory (an employee number, an email address).

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
  key's parameter set (`ml-dsa-44`, `ml-dsa-65` or `ml-dsa-87`). A key that verifies nothing (a nil
  ML-DSA key, or a weak Ed25519 key) reports none, so the gate refuses it rather than seating it.
- A weak Ed25519 key is one `audit.CheckEd25519PublicKey` refuses: the wrong length, not the
  canonical encoding of a curve point, or outside the prime-order subgroup. `crypto/ed25519` accepts
  all of them. Under a small-order key anyone can forge a signature; a mixed-order key `A + T` is a
  second public key for `A`'s secret, and a non-canonical encoding is a second spelling of a key, so
  either would give one secret two identities and two seats. `Ed25519Verifier` (and so a hybrid's
  Ed25519 component) verifies nothing under a weak key and reports no identity for it. The subgroup
  check costs 1 to 4 ms of CPU per key (against about 45 microseconds for a signature check); results
  are kept in a small LRU cache, so a resolver that returns the same few keys pays it once per key.
  If your resolver takes keys from untrusted input, each new key that decodes to a curve point costs
  a check: bound or rate-limit those lookups.
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

### On a plan flow node

A [plan flow](plan-flows.md#approval-gates-on-nodes) node takes the same gate: `plan.Approval(policy)` in
Go, a config `approval: {need, approvers}` block, or the gate of the agent tool the node wraps, with
the approvers' verifiers bound by `Builder.WithApproverVerifiers` (or `plan.WithLoadedApproverVerifiers`).
The flow pauses before the node with the same `*agent.ApprovalPending`; its `ToolUseID` is the node's
key (`node:<name>`, or `node:iter:<n>:<name>` in a loop body, where each iteration is approved on its
own) and its `Args` the node's input, so approvers sign `pend.Subject()` and record decisions with
`SubmitDecision` (or `Approve` for `SingleApproval`) exactly as above. The gate records the node and
its input under `approval-request:<node key>` before the first pause, in place of the model turn
that requests a tool call, and its terminal tally before the node runs. Only decisions recorded
after that request count, and `Approve` and `SubmitDecision` refuse a node key that has none yet
(`ErrConfig`), since node keys, unlike tool-use IDs, can be guessed before the run gets there. A
decision recorded before the request anyway (written directly) never counts, and the approver's
identical decision afterwards returns `ErrConfig`: see [Flows](flows.md#approval-gates-on-nodes) for
what that means for the gate. A denial stops the flow with
`plan.ErrApprovalDenied`. Evidence and `bide audit verify-approvals` take the node's key as the call.

## Deciding from the terminal

An approver who does not write Go decides with `bide approve`, on a journal the gated agent runs
against (a SQLite file, or Postgres by the name of the environment variable that holds its DSN):

```
bide keygen -out ops.key -as ops               # once: a key pair; prints the public key to enrol
bide approve -db run.db -list                  # the calls that wait, each with its command
bide approve -db run.db -run refund-1049 -call c2 -as ops -key ops.key \
    -approver-keys approver-keys.json
```

`bide approve` reads the call from the journal, never from a flag or a notification, and shows what it
is about to sign: the run, the tool, the call's arguments (indented JSON; long ones are cut, and
`-full` shows all), the gate (k of n, who has decided, who is pending) and the SHA-256 of the arguments
the signature binds. That digest is read out of the very bytes that are signed
(`agent.ApprovalDecisionBytes`), so it is the digest of the call as the journal holds it. Text in the
arguments that could repaint a terminal or reorder what you read (escape sequences, bidirectional
overrides) is shown escaped. It then asks you to type your approver id (or `y`); `-yes` skips the
question for scripts. The decision is signed with your Ed25519 key, checked against the call by
`agent.WithDecisionCheck` with the public key derived from your private key, and recorded with
`agent.SubmitDecision`, stamped with your clock in the canonical form. A single-approver gate
(`agent.Approve`) has no approver id and no signature: `bide approve` records it as `agent.Approve`
does, takes neither `-as` nor `-key`, and says the decision is unsigned.

`-reject` records a rejection: the same checks and the same signature, over a denial. It is a flag of
`bide approve`, not a second command, so the checks live in one place and the prompt says `REJECT`.

It records nothing, and says why, when the call has no gate, the run is not waiting on it (it ended, or
the call has a result), the gate already has its approvals or can no longer pass, you are not one of its
approvers, you already decided (it names what you decided and when), or your key file can be read by
others: on Unix a private key with any group or other permission bit is refused, as `ssh` refuses one.

A key file is one line of hex: the Ed25519 seed (64 characters, the form `bide demo` writes its log key
in), `ml-dsa-65:` and the ML-DSA-65 seed, or `ed25519+ml-dsa-65:` and both seeds for a hybrid, for
buyers who require post-quantum or hybrid signatures (`bide keygen -alg`). `bide approve` holds the key
only as an `audit.Signer`: it signs the decision bytes, names the scheme and derives the public key
through that interface, so a signer in an HSM or a KMS can take the file's place later without changing
the flow (not built yet). `bide keygen` writes the file with mode 0600, never overwrites a file, never
prints it, and prints the public key in the text policies and verifiers read (`<scheme>:<hex>`, see
`audit.ParsePublicKey`; an ML-DSA public key is nearly 4000 hex characters, so the command names it by
its key ID where it shows one). The file is not encrypted: keep it on your own machine. ML-DSA needs a
Go build with `crypto/mldsa`, which the FIPS 140-3 module v1.0.0 lacks. A passkey signer is a different
`Alg` (see the [roadmap](../ROADMAP.md)).

**Which key counts.** The journal records approver ids, not keys: the application's verifiers
(`agent.WithApproverVerifiers`) hold those. A key that is not the one enrolled for you produces a
decision that is recorded and never counts (`agent.ReasonBadSig`), and does not take your place. So an
m-of-n decision requires `-approver-keys`, the file of enrolled public keys that `bide view
-approver-keys` reads (`bide keygen -as <id>` prints an entry for it): `bide approve` refuses a key that
is not the one enrolled for `-as`, before anything is recorded, and verifies the tally it prints with the
counting rule the gate runs (`agent.TallyApprovals`). `-unverified` decides without the file: the
warning is printed prominently and the tally is labelled unverified, since it counts the decisions
recorded and the gate checks every signature when the run resumes. A single-approver gate is unsigned and
takes neither flag.

After the decision is recorded, re-run the pause's `RootRunID` (or let `RecoverLoop` pick it up): the
gate counts the decisions and the call runs or is denied. `bide approve` never runs the call.

`bide approve -list` shows the calls that wait for a decision, each with the command that decides it
(`-json` for scripts). The commands never carry a secret: a Postgres journal is named by
`-postgres-env VAR`, not by its DSN, and the key by its file. `internal/approveline` builds the same
line, for a notification to carry.

Deciding on a click in a chat message is out of scope on purpose. A button handled by a server must sign
with a key the server holds, and one server holding the keys of k approvers collapses k-of-n
independence: whoever controls the server decides alone. See the [roadmap](../ROADMAP.md) for approvals
in the viewer, with keys held in the browser, and with passkeys.

## Proving the gate held

`audit.ApprovalEvidence` builds the complete evidence for a gated call under one signed tree head, in
journal order: the model turn that requested the call (its tool and arguments), the gate's
`approval-pending` record when the journal holds one (a `step` item: it says when approvers were
asked), every decision record the gate read, valid or not, the gate's recorded tally, and the
call's result.

<!-- docsnip: setup ctx context.Context; journal *agent.Journal; runID string; toolUseID string; logSigner audit.Signer -->
```go
th, _ := audit.NewTreeHead(ctx, journal, runID, time.Now().UnixNano())
sth, _ := audit.SignTreeHead(th, logSigner)
actions, _ := audit.ApprovalEvidence(ctx, journal, runID, toolUseID, sth)
```

The result appends onto an `EvidencePackage`'s `Actions`; drop the trailing result entry if the
package already carries the call.

An auditor checks it offline with `audit.VerifyApprovals`, holding only the evidence, the policy they
expect, the approvers' public keys (the same resolver shape the gate uses), and the log key:

<!-- docsnip: setup pkg audit.EvidencePackage; toolUseID string; policy agent.ApprovalPolicy; approverVerifiers agent.ApproverVerifierFor; logKey audit.Verifier -->
```go
v, err := audit.VerifyApprovals(pkg.Actions, toolUseID, policy, approverVerifiers, logKey)
// err == nil (and v.OK): the evidence is consistent and at least Need approved.
// errors.Is(err, audit.ErrNotVerified): the gate did not hold, or the evidence does not.
// v.ToolName, v.Args: the exact call that was approved.
// v.Counted, v.DeniedBy: who. v.Ignored: every other decision, with the reason.
// v.Times, v.AskedAt: when each counted decision was made and recorded, and sanity flags.
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
returns an error wrapping `audit.ErrNotVerified` without a recount. A denied gate yields consistent
evidence whose verdict is not OK (again with `ErrNotVerified`) and names who denied. An invalid
expected policy, or a resolver under which two of the policy's approvers share a key, is
`agent.ErrConfig`; a proof or record that cannot be read is `audit.ErrFormat` or
`audit.ErrMalformed`.

That certifies exactly one claim: **these named approvers approved this exact call, and it did not
execute until k of them had, under the expected policy, from evidence that leaves nothing out.** It
does not certify that their judgment was right. `audit.ProveApproval` proves a single decision by its
record name.

### From the command line

For an auditor who does not write Go, `bide audit verify-approvals` runs the same check from the files
alone, with approver keys as a JSON object of id to key text (`<alg>:<hex>`, as
`audit.FormatPublicKey` writes it, or bare ed25519 hex):

```
bide audit verify-approvals -evidence evidence.json -pubkey <log key> -call refund-1 \
    -need 2 -approvers ops,finance,risk -approver-keys approver-keys.json
```

It prints the approved call, each approver who counted, every ignored decision with its reason, when
approval was asked and each counted decision's `decided_at` and `recorded_at` with the gap (and a
`note:` line for each flag, which never changes the exit status), and every problem. It exits 0 only when the gate held: 1 when it did not, 4 when an input cannot be read
or used (including a key file that gives two of the policy's approvers one key), and 2 on a usage error (see the audit guide's
[exit status](audit.md#exit-status); only 0 means verified). `bide audit verify-evidence` checks the proofs only,
so it cannot detect an omitted decision; use `verify-approvals` for the approval claim. Every proof in the package must verify for `verify-approvals` to pass as well, so a package that carries
a run certificate needs its allowlist: `-approved <digest>` (repeatable) or `-approved-file <file>`, as for
`verify-evidence`.

Evidence files commit to the exact recorded bytes. Keep them byte-exact: a tool that re-orders JSON
object keys inside a recorded message breaks that record's proof.

## Scope

- The core does not authenticate who holds a key. Binding an approver id to a person is your identity
  provider's job; the core verifies signatures against whatever keys your resolver returns. It refuses
  two approvers on one key, but it cannot see that one person holds two different keys: issuing each
  person one key is the identity provider's job too.
- The approver set is bounded and fixed per policy. There are no weighted votes, role predicates
  ("at least one from risk"), delegated approval, or deadline to resolve a gate that never reaches k.
- The tally does not record which key verified each approver (each decision does record its scheme,
  `Record.ApproverAlg`). An auditor needs the keys that were
  valid when the gate decided; after a rotation, keep the old public keys to verify old evidence.

## Runnable example

`examples/approval` runs the whole flow across separate processes on a SQLite journal: the run pauses
and exits, approvers record signed decisions from their own processes, an ineligible approver and a
forged signature are ignored without locking anyone out, `-check` refuses a bad decision at
submission, the refund runs exactly once at 2 of 3, and an auditor verifies the exported evidence
with public keys only (`cd examples/approval && go run .`). Its test drives every actor as a separate
process and checks the evidence with `bide audit verify-evidence` and `verify-approvals`, including a
tampered file and one with a decision left out. `examples/approval/flow` does the same for a gated
plan flow node loaded from a config (`go run ./flow`).

The design and its tradeoffs are in [docs/design/design-mofn-approval.md](../design/design-mofn-approval.md).
