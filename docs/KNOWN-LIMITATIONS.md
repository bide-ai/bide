# Known limitations

This page lists what bide does not do yet, and where its guarantees stop. Read it alongside
[the guarantee](GUARANTEE.md): everything bide promises holds inside these bounds.

Each section says what the limit is, whether it affects you, and what to do about it.

- [Durability and recovery](#durability-and-recovery)
- [Sagas](#sagas)
- [Parallel tool calls](#parallel-tool-calls)
- [Sub-agent depth](#sub-agent-depth)
- [Models and providers](#models-and-providers)
- [Approval and quorum](#approval-and-quorum)
- [Plan flows](#flows)
- [Audit and proofs](#audit-and-proofs)
- [Key custody](#key-custody)
- [Governed state (gsm)](#governed-state-gsm)
- [Notifications](#notifications)
- [Stores](#stores)

## Durability and recovery

**Durability needs a durable store and waker.** `MemStore` and `MemWaker` are for local development
and tests. They keep everything in memory, so after a real crash there is nothing to recover, and a
pending `Sleep` has no timer to wake it. In production, use SQLite or Postgres, and either a durable
waker or an external scheduler that re-drives sleeping runs.

**You supply the Resumer.** `agent.Recover` finds incomplete runs and re-drives them, but only
your deployment knows which agent drives each run and which waker and clock to bind. Pass that as the
`Resumer`: `agent.ResumeAgent(a, agent.WithWaker(w))` for an agent's runs, `agent.ResumeTyped[T](a)`
for its typed runs, and `agent.ResumeAny` to combine them. A run's input, entry point and per-run
options are journaled at its first drive (`agent.RecordedStart`), and every recovery drive runs
under them. `Recover` skips sub-agent runs, which their root run drives, and session runs, which
their session drives when the message is sent again; a run with no `run:start` is skipped and
reported once per process (`ErrNotStarted`), and so is a run no `Resumer` drives (`ErrNotResumable`), which includes a run an earlier version started (its `run:start` does not say whether it is typed, so `ResumeAgent` and `ResumeTyped` decline it). See [Crash recovery](guides/debugging.md#4--crash-recovery-lister-and-recover).

**Takeover needs a process that keeps looking.** `agent.Recover` is one pass: a run whose holder
has died but whose lease has not yet expired is skipped. Run `agent.RecoverLoop` in every worker for
the life of the process, and a dead holder's run is taken over within about one pass interval (half
the lease TTL by default) of its lease expiring: a loop of its own lists only the runs whose lease
lapsed, so halted runs do not delay it. Its slots (`WithRecoverLapsedConcurrency`) bound how many
lapsed runs a worker takes over at once; more lapsed runs than that wait for a slot.

**A long full pass delays the runs that held no lease.** A run whose driver held no lease (a plain
`Agent.Run` whose process died) has no lease to lapse, so only `RecoverLoop`'s full pass re-drives
it. A full pass costs about five store round trips for each unfinished run it lists, halted runs
included, and the next one starts only after this one has started all of its drives. With many
unfinished runs (halted runs left unresolved count) or a slow store, a full pass can outlast its
interval, and such a run can then be picked up as much as a pass's length later than the interval
suggests. Drive runs under `agent.Lease`, resolve halted runs promptly, and measure a pass against
your store if takeover time matters.

**Leases prevent duplicate work, not duplicate side effects.** With a store that supports leases
(`MemStore` in one process, SQLite across the processes sharing one file, Postgres across nodes),
each run is normally driven by one holder at
a time, and another node takes over if the holder dies. The holder renews its lease from half the
TTL on, retries a failed renewal, and cancels its drive with `agent.ErrLeaseLost` if no renewal has
succeeded by three quarters of the TTL, a quarter of the TTL before any other node could take the
lease. That bound holds for a process that is running. A holder that stalls past its lease (a long
GC pause, a suspended VM, a network partition) can wake up still driving and take a step before it
notices. That is safe: at-most-once rests on the attempt claim written before each side effect, not
on the lease, so the second driver stops with `*OutcomeUnknown` (cause `HaltContended`) instead
of firing again. The cost is repeated work, such as a model call made twice. The stall does not
delay the takeover: on Postgres every lease call and journal insert is one statement that commits
before the store sees its reply, so a holder stalled between two of its round trips holds no lock,
and another node takes the run over once the last renewal that committed has expired.

**Leases are not fenced.** A fencing token (a number the lease hands out that every write must
carry, so the store rejects a write from a holder whose lease was superseded) would turn the lease
into mutual exclusion for journal writes. bide does not use one, for three reasons. The effect that
matters happens outside the store (a charge, an email), where no token can be checked, so fencing
the journal would not stop a stalled holder's effect; only the attempt claim, written before the
effect, can stop a second one. The journal writes that a stalled holder can still make are safe
without it: every step is recorded at most once by name, so its write either is the step's one
record or loses to the one already there. And fencing would make every journal write depend on the
lease, so a store without leases (a custom one) could not offer the guarantee at all.

**A session turn is single-driver only over a store with leases.** A `Session` drives each turn's
run under its lease when the store implements `Leaser`, so two workers given one message do not
both drive its turn: the second gets `agent.ErrTurnContended` and sends the message again later,
and the turn's token budget holds across workers. A worker that dies mid-turn holds the turn until
its lease lapses (one TTL, `agent.WithLeaseTTL` on `Agent.Session`). Over a custom store with no
`Leaser`, two workers can drive one turn at once: it is still recorded once and its side effects
stay at-most-once, but each worker counts only the spend it has seen, so the turn can spend up to
its budget once per worker. (A saga turn's rollback, driven by the next message's `Send`, holds
the session handle over such a store, so callers sharing one handle never drive it at once;
callers on separate handles still can, as with any turn over such a store.) A stalled holder that
wakes past its TTL is the case above: it can
make a model call the next holder does not count.

**Any role that can connect to the database can stall a run's writes.** The Postgres store
serializes inserts into one run on a transaction-level advisory lock whose key is
`hashtextextended(run_id, 0)`, and `govern/postgreslog` does the same for an entity. Any role
that can connect can take that key itself and hold it, and the run's inserts wait until it lets
go. It corrupts nothing, and earlier versions took the same key. The key names neither the
schema nor the table prefix, so stores in different schemas or with different prefixes, and a
governed entity named like a run, also wait on each other: throughput, not correctness.

**Pin the Postgres store's schema.** Without `postgres.WithSchema` (or `postgreslog.WithSchema`),
every `Open` finds the schema through the search path, and a role that can create a schema earlier
on the path, which any role with `CREATE` on the database can do for the `"$user"` schema the
default search path puts first, can redirect a restarting node to a store of its own. `Open` logs
a warning when it discovers the schema. With the schema pinned, the search path plays no part.

**Migrations have no down step, and the only schema version is 1.** A Postgres store or governed
log schema at another version is refused (`ErrSchemaVersion`), not converted.

**`WithoutMigration` checks the shape of the schema, not who can use it.** A store opened with
`postgres.WithoutMigration` (or after `bide migrate`) verifies that the tables, columns, indexes and function the
migration creates are there and unaltered, and refuses a missing schema or another version. It does not check
privileges, partitioning details, table or index storage options, or the contents of the tables, so a service
role that was granted `UPDATE`, `DELETE` or `CREATE` is not noticed. Grant the roles as
[the operations guidance](trust/operations.md#least-privilege-postgres-roles) sets out and check them in your own
deployment tooling.

**Renaming the Postgres store's schema needs one step.** The `bide_next_seq_v1` function names
its schema, so after `ALTER SCHEMA ... RENAME` inserts fail and `Open` refuses it: drop the
function and `Open` again, and the migration recreates it.

**Crash safety is tested and model-checked within bounds, not proven for the code.** The crash
tests fail the store at every write point, across hundreds of randomized multi-crash schedules, and
check that no side effect fires twice and every rollback completes. The protocol designs (claims,
the approval gate, flows, spend accounting, the bide protocol's claim rules, tool calls, the run
lifecycle and recovery, delegation and saga trees, and sessions) are TLA+ models that TLC checks in
every interleaving within each configuration's bounds; nothing is proven beyond those bounds, and
the models state the rules, not the Go code. CI checks that marked code changes with its model and
that the names agree; that each Go function does what its model step says is checked by review
until trace validation lands. A crash is modelled as a failed durable write followed by the run
unwinding, which matches a process dying around its writes. See [How bide is verified](testing/verification.md)
and [the formal models](../spec/tla/README.md).

**`run:complete` appears in diagrams.** A finished run records a `run:complete` step, so
`RenderMermaid` shows it just before `done`. This is expected.

## Sagas

**Each forward step must be all-or-nothing.** When a step fails, the steps before it are
compensated, but the failing step itself is not: it has no recorded result to undo. A step must not
leave a partial side effect behind when it returns an error. Make it atomic or idempotent.

**An unknown outcome stops the saga instead of rolling back.** If a step that cannot be retried
crashed after it started but before its result was recorded, bide cannot know whether it happened, so
the saga's `Run` returns `*OutcomeUnknown` for a person or a reconciler to resolve with `ResolveHalt`. See
[Sagas](guides/durable-steps.md#sagas-transactional-agents-with-reverse-order-compensation).

**A cut-off retry-safe call keeps no record of its safety.** A call that was retry-safe when it
ran writes no attempt marker. If it is cut off before its result is recorded and its tool is then
relabelled a side effect, a resume runs it again and a saga rollback treats it as never started.
A completed call does not have this gap: its result records whether it ran `ReadOnly`.

**A delegated rollback needs every grant the saga delegated under.** A rollback verifies each
`audit.AttenuatingSubAgent` delegation against the grant it was minted from, so keep a rotated or
expired root grant, and its signer, until the sagas that delegated under it have finished, and bind
it with `audit.WithRollbackGrants`. After a delegation's grant has expired, the rollback does not
run its cut-off retry-safe writes again to learn their results: it lists them in
`SagaAborted.UnknownOutcome`, for a person to check. See the [delegation guide](guides/delegation.md).

**Rollback follows the sub-agent tree.** Compensation runs in one order, through the tree of
sub-agents. Independent agents changing shared state concurrently need
[governed state](guides/governance.md), not a saga.

## Parallel tool calls

A model turn's tool calls run concurrently, unbounded by default. Use the `WithMaxConcurrency(n)` option
to cap them, or `WithMaxConcurrency(1)` to run them one at a time. Each side effect still fires at most once,
and the conversation lists tool results in the order the model asked for them.

**Journal order is completion order.** Within a turn, parallel tool results are recorded in the
order they finish, not the order they were started. This does not affect the model, which matches
results by tool-call id. It matters only if you read the journal and expect dispatch order, or if a
saga's parallel steps depend on each other (they should not: parallel steps must be independent).

## Sub-agent depth

**Very deep sub-agent trees use a lot of memory.** Every level of a tree stays in memory while its
children run, so memory grows with depth. Trees up to a few hundred levels deep complete and keep
every guarantee; trees thousands of levels deep can need gigabytes. Real agent trees are usually a
handful of levels deep, so this rarely matters. If you need very deep chains, restructure them as a
loop of sequential runs instead of nested sub-agents.

## Models and providers

**Three adapters.** bide ships native Anthropic and Gemini adapters, and one OpenAI-compatible adapter
that also covers Groq, DeepSeek, Ollama, Mistral and other compatible endpoints through `WithBaseURL`.
There is no native Bedrock adapter yet. See [Models](guides/models.md).

**Images are the only non-text input.** Messages carry text, reasoning, tool calls, tool results and
images. Audio and video input are not supported, and models produce text, reasoning and tool calls,
not images.

**Structured output varies by provider.** `RunTyped[T]` with `WithOutputMode(OutputNative)` uses the provider's JSON-schema mode,
which the OpenAI-compatible and Gemini adapters support and the Anthropic adapter does not (it
returns `ErrConfig` rather than run unconstrained). With Anthropic, use `RunTyped`'s default tool mode, which works
with every provider.

**Gemini schemas are a subset.** Gemini accepts only part of JSON Schema. A tool or typed output that
uses a map, an `interface{}` or `json.RawMessage` field, or a recursive type fails with `ErrConfig`
instead of being silently changed.

**Gemini does not receive earlier reasoning text.** Gemini does not accept thought text as input, so
the adapter sends back only the thought signatures Gemini needs to continue a thinking turn.

**Replay simplifies a reply's layout.** A replayed model turn has the same content as the original,
but it is laid out as reasoning blocks, then the text, then the tool calls. Text that appeared
between two reasoning blocks is moved after them, and separate text blocks are joined into one.

**A model request that ignores cancellation past the run's end is not in its spend.** When a run
ends (it completes, pauses, or fails), it waits for its model requests still in flight, such as a
hedge loser, and journals their usage in a late spend record. The wait is bounded: two seconds, and
not past the run's context. A request whose model ignores the cancellation it was sent for longer
than that is billed by the provider but is not in `Result.Spend`, the journal, or the token budget.

**Spend a drive could not journal waits in its process.** When a spend record's write fails, or a
model record's write reports an error and the read that should settle it fails too, the process
keeps the spend (and the turn's `OnAnswer` functions) and the run's next drive in the same process
journals it, deciding from the journal so nothing is counted twice, through any Journal over the
same store and any wrapper over one (such as `audit.AuditedStore`). If the process ends first, that spend is not journaled. The process keeps at most 4096
such entries; past that it drops the oldest runs' entries, so a run the process drives again after
its entry was dropped does not journal that spend.

**Replay's recorded model travels on the replayed Finish.** A Model that wraps the replay model
and forwards its events keeps it; one that builds its own `Finish` events does not, and the
replayed turn then journals what `agent.ModelInfoOf` reports for the wrapper.

**Replay of a turn journaled before finish reasons were.** Each model record journals the turn's
finish reason and the provider's raw reason (`Record.Finish`, `Record.RawFinish`), and a replayed
turn ends with them. A record written before they were journaled has neither: its replayed turn ends
with `tool_use` when it has tool calls and `stop` otherwise, and no raw reason. Only turns that ended
with one of those two reasons are journaled (a turn cut off at its token limit or stopped by a filter
is an error; see the finish reasons in docs/guides/models.md), so no journaled turn's outcome changes
on replay.

**MCP tools are untyped.** Tools discovered from an MCP server at runtime use raw JSON arguments,
because Go cannot create a struct type from a schema at runtime.

**A run's settings are fixed once journaled.** `WithSampling`, `WithSystemPrompt`,
`WithMaxTurns`, `WithTokenBudget`, `WithToolChoice` and `WithToolFilter` apply to one run through
`Run`, `Resume`, `Stream`, `RunTyped` and `Session.Send`. A run's settings other than its limits cannot change once its first drive
has journaled them: start a new run for different settings.

**A cancelled run's in-flight calls finish.** `agent.Cancel` stops a run at its next check (a turn
boundary, or a won side-effect claim before its call), so a call already past its check when the
cancellation lands is called and its result recorded. A retry-safe call (`ReadOnly` or
`Idempotent`) takes no claim and is not checked one by one: a retry-safe call already dispatched in
the current turn may still run after `Cancel` returns, and the run stops at its next turn boundary
or claim. `Cancel` takes no lease: it can land while a
recovery pass is about to resume the run, whose drive then reports the run cancelled. A cancelled
run and its completion can both be journaled, since they are two keys; the first in journal order
is the run's end.

## Approval and quorum

**Approvers are a fixed, named set.** An m-of-n approval gate names its approvers up front. There are
no weighted votes, role rules (such as "at least one from risk"), or delegated approval. A gate
that never reaches k waits until its deadline (`ApprovalPolicy.Timeout`), and with none until the
run is cancelled; a deadline is checked against the clocks of the processes involved, so clock skew
moves its edge. bide checks signatures against the keys you provide; linking a key to a
person is your identity provider's job. It refuses two approvers whose verifiers resolve to one key,
but it cannot see that one person holds two different keys. Keep old public keys after a rotation so old evidence still
verifies. See [Human approval](guides/hitl-approval.md#scope).

**Separation of duties compares ids, not people.** With `ExcludePrincipal`, the gate never counts an
approver whose id is a requester of the run: its journaled principal (`OnBehalfOf`) or that of a run
above it, ignoring ASCII case. A requester who also holds another approver's key can still sign as
that approver, and the requesters and the approvers must be named in one id namespace. A run started
before this version cannot pass a gate that excludes the requester: it has no `run:principal` from
before its calls' requests, and nothing backfills one (a principal recorded later would name whoever
resumed the run, not who requested it), so cancel such a run and start it again. While a rollout
leaves some processes on a policy without the exclusion and some with it, a call a process on the
old policy runs may also get a re-tally from a process on the new one (`approval-tally:<id>:sod`),
and an audit then reports that re-tally as the gate's outcome; drain the old processes before
relying on the exclusion. See
[Separation of duties](guides/hitl-approval.md#separation-of-duties).

**Journals written before the decision step was keyed by identity.** A decision recorded by an earlier
version is named by its signature (`approval:<call>:<approver>:<hash with the signature>`). It reads,
counts and verifies as before, and `bide audit` and evidence read it unchanged; an identical resubmission is a no-op with or without a check, and a re-signing under different bytes
(`WithDecisionCheck`) finds it by what it says and writes nothing more. Only a randomized
scheme re-signing submitted without `WithDecisionCheck` can still add a duplicate record, which the counting
rule supersedes. See [Human approval](guides/hitl-approval.md).

**A decision's time is the approver's claim.** Each signed decision carries when the approver
decided (`decided_at`, by their clock, inside the signature) and when it was journaled
(`recorded_at`, by the clock of the process that submitted it, not signed). The signature stops
anyone changing `decided_at` later, and the journal's Merkle trail stops anyone changing
`recorded_at` once a signed head covers it, but nothing vouches for either clock: bide flags a
`decided_at` far after `recorded_at` or before the run asked, and a `recorded_at` out of journal
order or after the signed head, and never counts by them. No trusted timestamp authority (RFC 3161) stamps decisions yet. See
[When a decision was made](guides/hitl-approval.md#when-a-decision-was-made).

**A decision another process wrote waits for a pass to be signed.** The application that holds
the log key signs a run's tree head on its own writes. A decision recorded by another process
(for example `bide approve` writing the database) is under no signed head until `agent.RecoverLoop`
next drives the run and re-signs it (within one pass interval), and not at all if no pass drives
it, a run another holder leases is that holder's to sign, and a failing anchor delays it. A re-sign ratifies what the database holds: a signed head shows a record was there, not that it is authentic. The window is bounded by the interval only while a loop runs; `bide audit verify-decision-coverage`
finds a decision left outside every head. See
[A decision written by another process](guides/hitl-approval.md#a-decision-written-by-another-process).

**The approval intake serves m-of-n tool-call gates only.** `approvals.Handler` refuses a
single-approver gate (no signature to check) and a plan flow node's gate. Two different decisions of one approver
sent at once are both recorded and the first counts (the response says `counted`). `GET /pending` shows a call's arguments to any caller the handler lets through, so
confidential arguments need `WithAuth` or a private network. See
[Taking decisions over HTTP](guides/hitl-approval.md#taking-decisions-over-http).

**Until the run is driven again, a viewer that cannot see the live policy shows a gate asking again as decided.** A redeploy that changes a gate's policy (adding `ExcludePrincipal`, say) is enforced by the gate when the run resumes. If the recorded tally passed under the old policy and the call has not run (a crash between the tally and the call), the gate asks again, and writes a re-ask note with the policy it asks under (`approval-pending:<id>:sod` or `:v3`); `bide view`, `bide approve`, the intake's fallback, notifications, evidence and `plan.Conform` read the newest note. The note is written by the drive, so before the run is re-driven (by you or `RecoverLoop`) a reader with no access to the application's policy still shows the gate as decided. The intake does not have this gap: `approvals.New` requires the live policy (`approvals.WithGatePolicy`) and counts the gate under it.

**The browser approve page is as strong as the origin that serves it, and no stronger than a key a
script can use.** A page the application serves can sign anything the approver's key will sign (the
page says so); the design is a bundle hosted by the approver's organization on a dedicated origin and
pinned by its published hashes (`approve-page.SHA256SUMS`, signed with each release), whose entry document Subresource Integrity cannot pin, so the hash is
checked at deploy time. The page's key is non-extractable but usable by any script on its origin, and the
page's confirmation steps (the typed id, the buttons, the scroll to the end) are aids, not controls. It
supports Ed25519 only (WebCrypto has no ML-DSA), one browser per approver id, and key enrolment needs a
fingerprint comparison out of band. Gates for high-value actions should wait for passkeys (WebAuthn),
which are not built: they need a new signature scheme in the gate and the auditor. See
[Approving in the browser](guides/hitl-approval.md#approving-in-the-browser).

**A resolution's, a single approval's, an answer's and a run's end time are the writer's claim.** Each of these
records notes when it was written (`recorded_at`), by the clock of the process that wrote it, and
nothing signs it: an m-of-n approver signs their `decided_at`, but a single approver (`agent.Approve`)
and the operator who resolves a halt (`agent.ResolveHalt`) hold no key the runtime knows, so their
time is anchored only by the journal's Merkle trail once a signed head covers the record, and no
clock is vouched for. A record written before the times existed has none and is reported as having
none. A tool result a model-driven call produced has no `recorded_at` (the attempt marker's
`attempted_at` is its time). Nothing reads these times to decide anything.

**A re-ask note is proven in what it supersedes, not in the policy it states.** Readers honor a re-ask note only where the journal holds a recorded tally the gate would ask again over and the note's `Supersedes` names it (`agent.ValidReaskNote`), so a note pre-inserted without that is ignored. A journal writer who does have write access can still pre-insert a note with the correct `Supersedes` and a forged `Need`, `Approvers` or `Requesters`; readers that only have the journal (`bide view`, `bide approve`, the intake's fallback, notification details, the verdict's `Reask`) then display that policy. It changes what is shown, not what counts: the gate never reads the note, every decision is signed by an approver's key, and the intake and the gate count under the application's live policy.

**A certificate-bound gate cannot start excluding the requester after it counted.** The gate's first count is `approval-tally:<id>:ident`. If that tally passed without excluding the requester and a redeploy then turns on `ExcludePrincipal`, the gate refuses with `ErrConfig` and the call does not run: unlike a bare-key gate it has no separation-of-duties re-tally name, because a re-ask of a certificate-bound gate is named by its generation (an exec hold of the previous generation) and nothing writes one for this case. Cancel the run and start it again. The error wraps `agent.ErrGateNeedsNewRun`, so a reader (`agent.EvaluateGate`, the approvals views) can say so. A certificate-bound gate is likewise refused, with `ErrConfig`, when its policy or resolver no longer has the certificate-bound approvers its journal says it was asked under, or has other approver kinds. See [the hitl-approval guide](guides/hitl-approval.md#a-gate-with-a-certificate-bound-approver-its-tally-its-holds-and-its-re-asks).

**The check before a certificate-bound call runs has limits.** (1) A revocation the CA records after the check passed is not seen by that call; the interval is the time the process takes to begin the tool, and the published latency bounds in [the operator guide](guides/certificate-approvers.md#published-revocation-latency) say what the gate can know and when. (2) A gate waiting for current status (`agent.PendingIdentityStatus`) sends no approval notification, because nobody is asked to decide; the phase note (`exec-hold`) and your own CRL monitoring are how it is noticed. (3) bide fetches no CRLs and raises no alarm on a stale one in this build: the application adds CRLs to the `StatusSnapshot`, and the 50 percent alarm on the oldest CRL of each issuer is the application's. (4) A call runs on a count that is only provisional; no `RequireFinal` policy exists yet, so nothing holds a call until its count is final (the `held-until-final` phase is reserved for it). (5) Startup requires that at least one CA that can issue a leaf (a pinned intermediate, or the anchors when none is pinned) publishes at least twice within `ExecStatusAge`, but which pinned CA is a given leaf's issuer is known only from the accepted chain, so in a policy with several intermediates where only some fit, a leaf whose issuer does not fit is held by the check before its call, not refused at startup. (6) The check stops after eight rounds of holds in one drive with `ErrConfig`; the next drive goes on. (7) The check re-verifies the decisions the gate's identity record names; a tally with no identity record is refused, never accepted unchecked.

**An exec hold, like a tally, is trusted as a journal value.** A reader accepts a hold whose format, tool-use id and generation read right and which sits over a passed tally of that generation. Anyone who can write the journal can therefore force a certificate-bound gate to count again, and anyone who can write a plain-name tally can force it to ask again; neither can undo a recorded denial, and the new count passes again if nothing changed. It is the same power as writing a tally and is documented as the journal's trust boundary, not defended against.

**Approval evidence is a selection, so an omitted exec hold is not detected from it.** For a certificate-bound gate that was held and counted again, `audit.ApprovalEvidence` discloses the superseded tally, the hold and the replacing tally, and `audit.VerifyApprovals` reads the outcome by the same rule and checks their order. A producer who leaves out the hold and everything after the first tally presents the first generation as the outcome; the verifier cannot see what the evidence omits. A check of the complete journal reads the chain and sees the hold. No run writes an exec hold in this build (the pre-execution status check is a later step), so the case does not arise yet.

**Decisions signed before `bide.approval.v3` must be signed again.** They no longer verify. A run
whose gate passed on them before the upgrade, and whose call has not run, pauses to ask again
(`agent.PendingReapproval`) rather than run on approvals an audit would fail. See
[A run approved under an earlier decision format](guides/hitl-approval.md#a-run-approved-under-an-earlier-decision-format).

**`bide approve` depends on the approval-pending note.** The journal's record of a gate's policy is the
informational `approval-pending` note, written once when the run first pauses. If that write was lost,
`bide approve` refuses the call (it will not take the approvers or the count from a flag), and `bide
approve -list` does not show it: decide from the application. A decision written by `bide approve`
goes through the journal's plain store, so it is covered by a signed tree head only at the
application's next write to the run or the next `RecoverLoop`/`Recover` pass that finds the run waiting (`AuditedStore` anchoring is best-effort even there; `bide audit verify-decision-coverage` flags a decision left unsigned past a bound); a receipt
kept by the approver, checked with `bide audit verify-receipts -update`, can prove a decision
deleted once a signed head has committed to it; before that, a mismatch is only the approver's claim. A store cannot refuse a second decision of one approver without letting a bad record take their
place, so concurrent decisions of one approver are both recorded and the first valid one counts. See
[Deciding from the terminal](guides/hitl-approval.md#deciding-from-the-terminal) and [Decision
receipts](guides/audit.md#decision-receipts-noticing-a-decision-deleted-before-it-was-anchored).

**A denied flow gate compensates nothing.** A plan flow node whose approval gate is denied never
runs, but plan flows have no saga mode, so the nodes before it keep their effects and the run stays
unfinished until it is cancelled. See [Plan flows](guides/plan-flows.md#approval-gates-on-nodes).

**The all-clear is found by a scan, not pushed, and is not ordered after its event.** A `resolved` notification is sent when the next drive, recovery pass or `NotifyPending` sweep touches the run, or reads the open trail (`notify:@open`) that lists every event whose all-clear may be owed, so a decision, a resolution or a completion is noticed by the next pass even in a run that is no longer unfinished. The first sweep or recovery pass after an upgrade reads every run's trail once to list events observed earlier, and once more a window later (`WithNotifyUpgradeWindow`, a day by default) to list what a process of the previous version, still running during a rolling upgrade, delivered without listing it; an event such a process delivers after that, in a run that ends before any new-version scan reaches it, stays unlisted and is reported by `bide audit notifications`, so set the window to the time your rolling upgrade takes to reach every process. Records of the open trail are dated no earlier than their segment's day: a process whose clock is behind a segment that another process's clock ran ahead into has its time moved later (and the clock reading it was moved from kept in `clock_at`), never earlier, so the open trail's times are structural and are not evidence of when something happened. It can arrive before its event when the event was cleared with a request in flight or its reply lost; a receiver matches them by `clears.id`. It is never overdue. A notifier whose process never runs a sweep or a recovery pass only finds an all-clear when the run is driven again. See [The all-clear](guides/notifications.md#the-all-clear).

**A quorum needs short, fixed answers.** Voters must pick from a set of labels, so a quorum does not
apply to free-form output. Agreement lowers the risk of one model's mistake, but models can be wrong
together. A tie is never agreement. See [Quorum](guides/quorum.md#what-a-quorum-does-not-do).

**A person with two identifiers fills two seats of a certificate-bound gate.** The design's seat is the canonical identifier of the certificate (an opaque person URI is recommended), and bide cannot see that two different identifiers belong to one person; it refuses only two approver ids that map to one identifier. The control is the bank's: its CA's issuance policy gives one person one identifier, and the identity map lists one identifier per approver. Formal model 15 states the limit (`limits/alias-two-seats`) and the Go refinement test documents it (`TestModel15AliasLimit`). Nothing in the approval gate uses certificate-bound approvers yet.

**Certificate status is conformance-tested against EJBCA and a standalone Microsoft AD CS, not an enterprise AD CS CA.** The building blocks for certificate-bound approvers (`audit.IdentityPolicy`, the CRL status rule) are checked in under `audit/testdata` against captures from EJBCA Community and from an AD CS standalone root and subordinate. An enterprise AD CS CA with certificate templates needs an Active Directory domain and has not been captured. A bank must capture its own CA with the `crl-fixtures.yml` workflow before go-live. Nothing in the approval gate uses these blocks yet.

## Flows

**Fan-in has fixed arity.** `Join2` and `Join3` combine two or three branches; there is no join over
a variable number of branches.

**Some wiring is rejected.** A flow's `Build` refuses wiring the runtime cannot run as declared, and names the
step: for example, a step fed by several producers without a join, or nested loops.

**`agent.Sleep` and `agent.WaitUntil` are not available inside plan flow nodes.** They need the run
context an agent supplies to its tools, and a flow does not bind one. Called from a `Step` or `Tool`
node, they return an error wrapping `ErrConfig`: `agent: Sleep called outside a running agent`
(`TestTimerInNodeBodyIsNotAvailable` pins it). A durable timer needs an agent run today; see the
[ambient agent recipe](recipes/ambient-agent.md).

**Only Steps inside a flow node are scoped to it.** A `Journal.Step` or `Parallel` task a node's body
runs is recorded per node and per loop iteration; an `Interrupt` or `Await` it takes is not,
so give a pause inside a loop body a name unique per iteration. See [Plan flows](guides/plan-flows.md).

**`Model` nodes expect JSON.** A `Model` node decodes the reply as JSON into its output type. A
model with structured output (`model/openai`, `model/gemini`) is sent the type's JSON schema; for
any other (`model/anthropic`), prompt the model for JSON. See [Plan flows](guides/plan-flows.md#limits).

**`bide check -diff` cannot see an effect the config does not state.** Without the `plan.Registry` (the
command-line check), a node's `safety` and its block's own approval gate (`ToolSpec.Approval`, set in Go)
are the config's claim or invisible. A gate bypass is reported whatever the label, and a lowered label
is itself reported, but a new block the config labels `readonly` while its Go code has an effect, wired
in where no gate and no earlier node of that block stood, looks like any new read-only node and is not
reported. Check with the Registry (`plan/planview`) where the blocks are registered. See [Compare two
versions of a flow](guides/run-viewer.md#compare-two-versions-of-a-flow).

## Audit and proofs

**Tamper evidence needs an anchor outside your database.** Hash chains, Merkle roots and signed tree
heads always detect accidental corruption. They detect deliberate tampering only when the root is
also committed somewhere an attacker cannot rewrite: signed with a key the application cannot use
freely, or published to a separate system. Someone who fully controls the database can rewrite and
re-hash everything in it. See [Audit](guides/audit.md#security-model-read-this-first).

**Some leaves are not salted.** Journal and event leaves carry a random salt, so the hashes in a proof
cannot be matched against guessed neighbouring records. Key-set leaves are not salted, because an absence proof names its
neighbouring keys by design, and neither are anchor-log leaves, which hold signed tree heads meant to
be public.

**Notification evidence is only as strong as the anchor and key you give the trail.** The notifier
signs and anchors its trail only if you wrap its event store in an `audit.AuditedEventStore`
(or sign heads by hand with `audit.SignNotifyTrail`); without a signed head the report says
`UNANCHORED`. Publishing is in the background and best effort, so records written since the last
published head (the publisher's delay, or an anchor outage plus the time to the next sweep, or a
process that died before it published) are read but not attested; the report gives their count
and age, and `-max-unattested` fails one older than your bound. An event that rests on such records is
`unattested` and fails, unless a nonzero `-max-unattested` accepts recent ones, which are then trusted
without a signature (content and times). Per-receiver completeness is checked only with the
notifier's routing (`-routing`). A record deleted from that tail
cannot be found. The heads attest the trail, not who may sign: a signing key the application can
use freely lets an attacker who controls the process sign a rewritten trail, so keep it in an HSM or
KMS signer. The check cannot find an event that left no trace in the journals it was given and no
record in the trail (a halt that a later result resolved before the audit), cannot say that a person
read a notification (an acknowledgement is the sink's 2xx), and dates events by the notifying
processes' clocks and the resolving processes' (the `recorded_at` of a decision or a `ResolveHalt`
result, unsigned). An all-clear owed for a journal an earlier version wrote, whose resolving record
holds no time, cannot be judged late and is reported as the warning `all_clear_untimed` (`-strict`
fails on it). See [Audit](guides/audit.md#notification-evidence-were-the-people-told).

**The log key can be in an HSM or KMS only under ed25519 or ecdsa-p256, not under the post-quantum
schemes.** `ecdsa-p256` is the scheme most HSMs and cloud KMS can sign (AWS KMS, Azure Key Vault,
PKCS#11), and an evidence package's seal signs a fixed 60-byte message, so a KMS with a 4096-byte
limit can seal any package. Hybrid post-quantum keys inside an HSM are deferred: `audit.HybridSigner`'s
halves are the concrete `Ed25519Signer` and `MLDSASigner` types that hold their keys in process, and no
HSM or cloud KMS signs ML-DSA yet. A buyer who needs both a hardware-held key and post-quantum
protection has to choose one for now. ECDSA P-256 signatures are randomized, so signing one tree
head twice gives two different valid signatures (ed25519 and ML-DSA give the same bytes).

**The RFC 3161 token verifier reads a narrow profile and refuses everything else.** `audit/rfc3161`
(standard library only) accepts a DER token with one `SignerInfo`, SHA-256 or better, ECDSA P-256 or
P-384, RSA of 3072 bits or more (2048 only through `Policy.RSA2048Until`) or Ed25519, and the signed
attributes `contentType`, `messageDigest` and `signingCertificateV2`. It is a component: no command or
gate calls it yet. A token that binds its signer only through the SHA-1 `signingCertificate` attribute
is refused with `signing-certificate-v1-only` unless the policy opts in for that TSA's key
(`Policy.AllowESSV1`, off by default, per TSA); a result that relied on the opt-in has
`Result.ESSV1` set and a verdict should show it. A token that uses SHA-1 as its CMS digest algorithm
is refused with `weak-hash` whatever the policy says. **Of ten public TSA endpoints tried on
2026-10-15, six produce tokens that verify by default (DigiCert, GlobalSign, QuoVadis, SSL.com,
Microsoft, SwissSign), three verify only with the opt-in (Sectigo, Entrust, whose endpoint returned
tokens signed by Sectigo's TSA, and FreeTSA) and one cannot be used (Apple: SHA-1 digest algorithm).**
Also refused: SHA-3 and SHA-224 hashes, ECDSA P-521, `crls` and attribute certificates, a token
that carries any certificate Go's parser rejects even when unused, a SHA-256 default spelled out in
`ESSCertIDv2`, and an accuracy above `Policy.MaxAccuracy` (one second by default). The stated accuracy
widens the window in which genTime may precede the stamped head, which is why it is capped, and the
latest the token proves is `Result.Latest()`, not `GenTime`. The TSA certificate path is judged at the
token's own `genTime`, which the holder of a retired or compromised TSA key chooses: bound that with
`Policy.DistrustAfter`, a status function that checks every non-anchor certificate as of `genTime`
(`Policy.CertStatus` is mandatory; `rfc3161.StatusUnknown` is the fail-closed placeholder; the package
fetches no CRL or OCSP response), and, for long-term evidence, renewal by an archive timestamp.
The time bounds use the caller's clocks (the head's, and `Expected.Now` for a live gate), so a log
clock running ahead refuses a genuine token. The verifier takes the token, not the `TimeStampResp`:
the caller checks the `PKIStatus`. OpenSSL accepts more than the profile does; the cases are listed
in `audit/rfc3161/openssl_test.go` (`opensslAccepts`). The reverse also holds once: a parseable but corrupted certificate in the token that the path does not use is ignored here and rejected by OpenSSL. Tokens that differ only in such certificates attest the same thing but are different byte strings; `rfc3161.Normalize` strips them, and anything keyed on token bytes should use its output.

**Evidence sealed before format v6 keeps its older seal.** A `bide.audit.evidence.v5` package, whose
seal covers the whole package JSON, still verifies (`bide audit verify-evidence` notes the format),
but it cannot be re-sealed and its seal cannot be produced by a signer that refuses large messages.
Re-create the evidence to get a v6 package.

## Key custody

A signing key can be held in a PKCS#11 token, AWS KMS, Google Cloud KMS or Azure Key Vault ([Key custody](guides/key-custody.md)). What is and is not supported:

- **Ed25519 and ECDSA P-256 only.** The PKCS#11, AWS KMS and Google Cloud KMS signers sign `ed25519` (the default) or `ecdsa-p256` (`Config.Scheme`) and refuse a token or key of the other kind (`ErrNoEd25519`, `ErrNoP256`); they never substitute one scheme for the other. The Azure Key Vault signer signs `ecdsa-p256` only, because Key Vault and Managed HSM offer no Ed25519. A token or key service with neither cannot be used.
- **No ML-DSA or hybrid in a token.** `audit.HybridSigner` holds a software Ed25519 key, and KMS has no ML-DSA context parameter, so post-quantum and hybrid anchoring use software keys. Hybrid in a token is deferred.
- **The PKCS#11 signer is tested against SoftHSM2 only.** No hardware HSM has been tried. It uses `CKM_EDDSA` without parameters (pure Ed25519) or `CKM_ECDSA` over a SHA-256 digest, and needs cgo; a token that requires `CK_EDDSA_PARAMS` for pure Ed25519 is not supported.
- **The AWS KMS signer is tested against a fake client and an emulator, never against AWS.** Whether real KMS's `ED25519_SHA_512` and `ECDSA_SHA_256` output verifies under bide's verifier rests on AWS's documentation of those algorithms; the emulator ([aws-kms-emulator](https://github.com/blackwell-systems/aws-kms-emulator), run in the test process by the module `signer/awskms/emulatortest`) is its author's reading of them.
- **AWS KMS signs at most 4096 bytes under Ed25519.** Tree heads, grants and approver decisions fit unless they carry long text: a tree head is about 110 bytes plus the run ID's length, and an approver decision carries the run, call, tool and approver names and the SHA-256 of the arguments (not the arguments); `awskms.ErrMessageTooLarge` names the size. An evidence seal is 60 bytes and fits. Under ECDSA P-256 KMS signs a 32-byte digest and there is no limit.
- **The Google Cloud KMS signer is tested against an emulator and a fake client, never against Google Cloud.** The emulator is [gcp-kms-emulator](https://github.com/blackwell-systems/gcp-kms-emulator) v0.10.0 run in the test process (module `signer/gcpkms/emulatortest`, kept separate so the emulator is not in the signer's module graph); the fake client covers the failures the emulator cannot produce, and the `HSM` protection level, which the emulator never reports. Whether Cloud KMS's `EC_SIGN_ED25519` and `EC_SIGN_P256_SHA256` output verifies under bide's verifier rests on Google's documentation of those algorithms and on the emulator's reading of it; check it once against your own key version before relying on it.
- **Google Cloud KMS has no HSM-backed Ed25519 key.** Google offers `EC_SIGN_ED25519` at protection level `SOFTWARE` only. The `gcpkms` signer is fail-closed about protection level: by default `Open` accepts only `HSM` and `HSM_SINGLE_TENANT` (`gcpkms.ErrProtectionLevel` otherwise), so the default scheme is `ecdsa-p256` and the Ed25519 scheme needs both `Config.Scheme` and `Config.AllowSoftwareKey` set; the errors name the fields and point to `ecdsa-p256` (`EC_SIGN_P256_SHA256`, which supports HSM). A deployment that must keep its signing key in an HSM uses `ecdsa-p256` with an `HSM` key version and leaves `AllowSoftwareKey` and `AllowExternalKey` off.
- **Google documents no size limit for an Ed25519 `data` field** (its signing guide states one only for raw RSA algorithms, and the quotas page none; checked 2026-10-14). The signer sends the message as given and enforces no limit of its own; if Cloud KMS rejects a long message the error is a `gcpkms.CodeError` with code `InvalidArgument`. Cloud KMS cannot sign an empty message under Ed25519 (`gcpkms.ErrEmptyMessage`); audit never signs one.
- **The Google Cloud KMS signer needs `cloudkms.cryptoKeyVersions.get`** (to check the version is enabled and read its algorithm and protection level) as well as `viewPublicKey` and `useToSign`; neither `roles/cloudkms.signer` nor `roles/cloudkms.signerVerifier` has it (checked against Google's role reference, 2026-10-14), so grant the three permissions in a custom role ([the role](guides/key-custody.md#google-cloud-kms)). It checks the state and protection level at `Open` and the protection level on every signature; a version disabled or destroyed after `Open` fails at `Sign`. Protection levels `EXTERNAL` and `EXTERNAL_VPC` (Cloud EKM) are refused unless `Config.AllowExternalKey` is set, and bide does not look inside the external manager.
- **Attestation is checked only for Google Cloud KMS, and only on request.** `gcpkms.Config.VerifyAttestation` (off by default) verifies the Cloud HSM attestation of the key version ([Key custody](guides/key-custody.md#google-cloud-kms)); it is tested against the real attestation, certificate chain and public key of a Cloud HSM P-256 key version (`CAVIUM_V2_COMPRESSED`, created 2026-10-14; the test data is committed), against a real version 1 chain and signature (public test data from another project), and against synthetic attestations and chains built in the tests. It has been run on one key version of one firmware generation; a Cloud HSM key attested by a different firmware or in the `HSM_SINGLE_TENANT` protection level may use a layout this parser refuses, and it then fails closed. The PKCS#11 and AWS KMS signers check no attestation: the PKCS#11 signer can check that the token reports the key as non-extractable and sensitive, and never extractable or non-sensitive (`RequireNonExtractable`, off by default in the library, on in `bide-hsm`), which is the token's word and not proof, and a token or KMS key is only as separate from the application as its access policy makes it. Attestation certificates' validity periods are not enforced (Marvell's root expired on 2025-11-16), as in Google's reference verifier, and there is no revocation.
- **`bide approve` reads key files only; `bide-hsm approve` reads keys in a token or KMS.** `bide-hsm` is a separate program, built from source and not in the release archives, because the PKCS#11 library needs cgo and the `bide` binary is pure Go and reproducible; a build without cgo still signs with a KMS key and refuses a `pkcs11:` reference. Its PIN comes from a file, an environment variable or a prompt on the controlling terminal (tested without a pseudo-terminal: the prompt reader is faked, and the refusal without a terminal is tested; the real prompt was checked by hand on macOS); a token that authenticates through a a PIN-pad token is used with `pin-source=protected` (tested against a fake login, never a real device); a key with `CKA_ALWAYS_AUTHENTICATE` is authenticated for each signature (tested against SoftHSM2 and a fake), so a PIN prompt appears twice per approval. It is tested against SoftHSM2 and a fake KMS client only.
- **`bide-hsm approve` against Google Cloud KMS depends on Google's application default credentials,** and the principal needs the three permissions the signer needs (`cloudkms.cryptoKeyVersions.get`, `viewPublicKey`, `useToSign`). Cloud KMS has no HSM-backed Ed25519 key, so an Ed25519 approver key needs `-allow-software-key` and is not in an HSM. Each run makes one billed self-test signature. Tested against the gcp-kms-emulator v0.10.0 (which reports every key as SOFTWARE) and, for the signer, a fake client; never against Google Cloud from `bide-hsm`.
- **`bide-hsm approve` against Azure Key Vault depends on the Azure SDK's default credentials,** and the principal needs the Key Vault Crypto User role on the key. The key is ECDSA P-256 only, and each run makes one billed self-test signature. Tested against the Azure SDK's in-process fake Key Vault server (Microsoft's, behind the real azkeys client), never against Azure.
- **An HSM key protects the key, not the decision.** The token signs the bytes `bide-hsm` hands it and does not see the call. On a compromised approver workstation with the token unlocked, malware can have the token sign a decision other than the one displayed. A token with a trusted display that confirms each signature is not supported.
- **A reference by label alone can match two keys.** Labels are not unique on a token. `bide-hsm` refuses a reference that selects both an Ed25519 and an ECDSA P-256 key (add `id=` or `-alg`); two keys of one scheme under one selection are refused by the signer (`ErrAmbiguous`).
- **`bide-hsm approve` against KMS depends on the AWS SDK's configuration.** Endpoint overrides in the environment or the shared config file are honored, and the role needs `kms:GetPublicKey` and `kms:Sign`; each run makes one billed self-test signature. The KMS path is tested against a fake client and a local endpoint, never against AWS.
- **`bide-hsm` loads the library `module-path` names into its process.** It requires an absolute path and refuses a relative one, but it runs whatever library that path holds, so name only a vendor library you trust.
- **The Azure Key Vault signer is tested against a fake client and the Azure SDK's in-process fake server, never against Azure.** Microsoft publishes no Key Vault emulator (checked 2026-10-14; the community emulators are not Microsoft's and are not used). That `ES256` returns the 64-byte `r||s` of RFC 7518 rests on Microsoft pointing at RFC 7518 for ES256; Microsoft's pages do not restate the encoding. The signer accepts only 64 bytes and checks every signature against the key's public key, so a different encoding is an error and never a bad signature; check it once against your own key before relying on it. It needs the key identifier with its version, so moving to a new key version is a configuration change and a restart.
- **The Azure signer's HSM and non-export checks read the key's metadata.** By default only an `EC-HSM` key (a Premium vault or any Managed HSM key) opens (`Config.AllowSoftwareKey` opts out), and a key that is exportable or has a release policy is refused; both are what the service reports, not an attestation.

## Governed state (gsm)

**A federation built under gsm v0.12.0 was not checked for event order across registries.** bide
requires gsm v0.14.0, whose `Federation.Build` checks cross-registry CC (C1: a target event commutes
with every change of its shared component its sources can cause) and repaired CC (C2: two target
events its own CC covers still commute with the morphism repair between them), and rejects a
federation that fails either. gsm v0.12.0 checked neither, so a federation it built could reach a
state that depends on which event was appended first; every governor replaying the shared log
agreed on that state, but the state was timing-dependent. bide's earlier docs said cross-registry
conflicts resolve without coordination by the authority argument; that claim is false
(normalization-confluence `FederationGRS.v`, `fed_thm_fed_convergence_refuted`). The authority
argument makes the federated repair terminate in a unique valid normal form, and event order is
safe because of the C1 and C2 checks (`fed_thm_fed_convergence_guarded`). Both checks are static,
over every valid source state, so a rejection's witness may be a state a given run never reaches.

**A convergence verdict recorded under gsm v0.11.0 is not covered by v0.12.0's fix.** bide's
`govern` module required gsm v0.11.0 before this release. Its `Build` skipped the
compensation-commutativity (CC) check for an event pair it judged independent from what the two
events write, and did not look at what their guards and effects read, so a machine where one
event's guard or effect reads a variable another event writes could pass `Build` although two
orders of its events end in different states (the order machine with `pay` and a `ship` event
guarded on `paid` is the standard case). gsm v0.12.0's `Build`, and every later one, checks every
pair it checks for CC (every event pair, or only the pairs declared with `Independent`) exactly,
with no footprint shortcut. A `govern.CertifyConvergence` certificate, or any other verdict,
produced under v0.11.0 and anchored in a trail is still what v0.11.0 said: rebuild the machine
under v0.14.0, the version bide requires, and record a new certificate before relying on it.

**Declared pairs narrow the guarantee.** With pairs declared, CC certifies only those pairs, so the
guarantee covers runs whose events are reordered only across declared pairs; the events of every
other pair must never be concurrent. Since gsm v0.13.0, `Build` also checks the undeclared pairs
without failing on them and lists each one that does not commute (`Report.CausalOrderRequired`,
recorded as `causal_order_required` in `govern.ConfluenceCertificate`). A log-backed governor
replays one shared log in one order that respects every causal dependency running through the log,
so a listed pair whose second event is issued after its first is observed is applied in that order;
bide does not track causality, so that the listed pairs are never concurrent is the caller's claim.

**Delivery obligations.** The convergence guarantee is about each event delivered once. gsm v0.13.0
lists the events a duplicate delivery would change (`Report.NotIdempotent`, recorded as
`not_idempotent`). `ApplyOnce` applies an event at most once per id across every process sharing
the log, and inside a run `EventTool` and `FederatedEventTool` key it by the tool call; `Apply`
takes no id, so a caller that retries `Apply` itself applies the event again. gsm also lists the
rules whose writes it clamped into a variable's range (`saturations`): convergence is unaffected,
but an invariant meant to catch the overflow never sees it. A certificate recorded before bide
carried these lists has none of their keys, and `bide audit` says so rather than reading it as
"none".

**What the proof-derived checks cover.** Since gsm v0.12.0, `Build` returns a machine only after
the table oracle (and, within its fragment and cap, the rules oracle), generated from gsm's Coq/Rocq
proof, re-checks it in-process:

- The table oracle checks every machine `Build` returns: its step tables, for every state.
  `BuildCompositional` is checked per footprint component, and independence across components rests
  on gsm's footprint check, not on an oracle.
- The rules oracle also checks the machine from its rules, independently of gsm's tables, only when
  the rules are combinator declarations (not Go closures), inside the oracle's arithmetic fragment,
  and within a work cap of 2^29 units; otherwise `Build` certifies with the table oracle alone and
  says why in `Report.RulesOracleSkipped`.
- A federation's own conditions (morphisms, resolvers, the event-order checks C1 and C2,
  acyclicity, the monotone-cycle check) are checked by gsm's Go code, not by an oracle. Each component registry is a machine `Build` checks
  as above.
- `govern.ConfluenceCertificate` records the WFC and CC verdicts, `Converges`, which needs a
  machine `Build` returned, and the delivery obligations above, but not which oracles certified it
  (`Report.Assurance`).

bide's required `gsm machine gate` CI check runs the two checkers extracted from the proof on every
machine the governance examples build (see `.github/gsm-gate`); it covers those examples, not the
machines an application builds. See the [roadmap](ROADMAP.md#gsm-convergence) and
[Convergent governance](guides/governance.md#under-the-hood).

## Notifications

**The relay's memory is in the process.** `bide notify relay` (package `notify/relay`) drops
redeliveries by delivery ID and drops a trigger whose event it saw cleared, both from bounded
in-memory caches (`-dedupe`, 10000 by default). Without `-state <file>`, a restart empties them: a late
redelivery is forwarded again, and a trigger that arrives after a clear the relay has forgotten
reopens the PagerDuty incident. With `-state` the caches survive a restart, but once more than
`-dedupe` other events have gone by the oldest are still forgotten, and a failed write to the file is
logged, not fatal. A crash can lose the forwarded-delivery entries of the last `-state-flush` (1 s) window, so a redelivery of those events is forwarded once more; clears are written at once. A second relay on the same file is refused at start by an operating-system lock. Slack shows a repeat twice. Run one relay: two behind a
load balancer have separate caches. A lost notification is not among the failures: the relay answers
2xx only after every destination took the event. It does not forward error text or tool arguments.

**A test notification is a real one to a relay.** `bide notify test` sends a sample for a run that does
not exist; a relay forwards it to Slack and PagerDuty like any other event.

**Routing and escalation are configuration, not evidence of intent.** The trail records which receiver
each event went to, when it was observed, claimed and acknowledged, and each step of an escalation
chain; it does not record the routing rules or the chain that were configured, so it cannot show that an
event *should* have been escalated and was not (that needs the configuration, which is code under your
version control). A route is a kind or an approver ID, not a tool or an amount.

**An escalation needs a running process at the deadline.** Deadlines are read by a process with the
notifier configured (a long-lived `Agent`, `RecoverLoop` or a periodic `NotifyPending`). Nothing alive
at the deadline means the step is taken at the next pass, late and not lost. A step made after a restart
rebuilds the event from the trail and the journal: it has the gate's tool, policy and approvers and the
halted call's tool name, but not a halt's cause (the original's notification has it).

**Acknowledged is not read.** A receiver's 2xx means its endpoint accepted the request. For an event
that is not an approval, an acknowledged step ends the chain, which a person may not have seen; route
the step to a receiver that pages. An approval is escalated until it is decided, whatever was
acknowledged. A chain is at most four steps and is not repeated.

## Stores

**Upgrading from v0.8.0 or earlier is not a rolling deploy.** Their journals have no journal
format header, so this version refuses them (a SQLite file that holds any, at `Open`; a Postgres
run, when it is first read), and those versions cannot read the journals this version writes. The
versions also keep their leases in different tables, so they do not exclude each other. Finish or
resolve the older runs with the older version, stop every node of it, then start this version.

**Before 1.0, a run is not promised to resume across releases.** Every pre-release writes the
same journal format tag, `bide.journal.v1-dev`, and the tag is not bumped when the keys or the
record shape change between pre-releases, so a later pre-release does not refuse an earlier one's
journal even where it reads it differently. Finish or resolve runs before upgrading between
pre-releases. At 1.0 the tag becomes `bide.journal.v1`, and 1.0 refuses every pre-release journal.

**A SQLite lease TTL must exceed the disk's worst commit.** A lease renewal is a committed write.
`store/sqlite` puts it ahead of the journal writes queued in its process, so it waits for at most the
one write already inside, but it cannot commit faster than the disk: a commit that stalls for longer
than the lease TTL (a saturated shared disk, antivirus, a network-backed volume that should not hold
the file) lets the lease lapse, and the run's drive is cancelled with `agent.ErrLeaseLost`. Choose
`agent.WithLeaseTTL` several times the worst commit latency you see on the storage (the default TTL is
sized for local SSDs); on slow or shared storage use a longer TTL or Postgres.

**One SQLite file, one name, one link.** SQLite coordinates the connections to a WAL-mode database
through files it names from the path it was given, so two handles that reach one file by different
names can each take the WAL for their own and corrupt the database ([SQLite: how to
corrupt](https://sqlite.org/howtocorrupt.html)). `store/sqlite` and `govern/sqlitelog` open the
canonical path (absolute, symlinks resolved), so a symlink, a `..` spelling, a relative path or a
different case reaches the same WAL. A hard link has no canonical name: a file with more than one hard
link is refused with an `ErrConfig` error at `Open`, `OpenReadOnly` and `New`. Keep one directory entry
per journal file. A `*sql.DB` you open yourself and give to `sqlite.New` is not rewritten: open it on
the canonical path too.

**SQLite is for one machine.** SQLite allows one writer at a time; a writer waits up to 30 seconds
for the lock before failing. The writers in one process that share a file (several `store/sqlite`
stores or `govern/sqlitelog` logs on it) take turns in the order they ask, so a burst of them cannot
starve one past the 30 seconds. Writers in different processes are not ordered: SQLite's wait for
the lock is not a queue, so with many processes writing one file at once (or a very slow disk) an
unlucky one can still wait out the 30 seconds and fail with "database is locked"; keep the writing
processes few, or use Postgres. Its leases coordinate the processes that share one database file on
one machine, not nodes: the file must be on a local disk, since SQLite's locking does not work over
NFS. Use Postgres for more than one node. A forward jump of the system clock expires SQLite leases
early (their expiry is computed from the clock), which the attempt claims keep safe.
