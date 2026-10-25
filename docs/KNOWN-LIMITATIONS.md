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
- [Anchoring](#anchoring)
- [Governed state (gsm)](#governed-state-gsm)
- [Notifications](#notifications)
- [Untrusted content and the taint policy](#untrusted-content-and-the-taint-policy)
- [Stores](#stores)
- [Anchoring: Google Cloud Storage](#anchoring-google-cloud-storage)

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

**Run IDs under `@cap/` and the cap and gate step-name prefixes are reserved.** `Run`, `Stream` and `Session`
refuse a run ID that starts with `@cap/` (`ErrConfig`), as they refuse `@stop`, and `Step`,
`Parallel` and `ResolveHalt` refuse a step name that starts with `slot:`, `cap-intent:`,
`cap-reserve:`, `cap-release:` or `gate-reason:`. The engine will keep cap ledgers and gate reasons under them in
later releases. `ClaimAttempt` claims bare attempt markers only: it refuses every reserved name except an attempt marker key (`attempt:`, not the engine's own `attempt:not-started:` and `attempt:retry:` forms) and any record that is not an attempt marker, so a caller
with journal access cannot plant an engine record such as a `cap-intent:`, a tool result or a single-approver approval with it (v0.23.0 and earlier could). A caller with journal access can still pre-claim an `attempt:tool:<call>` marker: that halts the call (`OutcomeUnknown`), a denial of service in the safe direction, and it cannot admit the call or release a cap, because only a not-started record that carries the marker's own claim id voids an attempt. `Approve`, `Cancel`, `Signal`, `Step`, `Lease` and the other exported calls that write to a run you name refuse a
`@cap/` run too (`@stop` keeps its old behaviour in them: they still write to it, a known gap left
as it was). A run already named under `@cap/` keeps its journal, but `Recover`, the run viewer,
`bide approve -list`, `bide audit notifications` and `bide audit redactions` skip it from this version on, so it is not resumed and a gate
waiting in it is not listed: rename such a run (start it again under a new ID) before upgrading. A
`Step` named with one of the four prefixes in a run that resumes now fails with `ErrConfig`.

**Cumulative caps keep a reservation they cannot prove unused.** The loop releases the slot of a marked call it proved never began, and `Recover` and `RecoverLoop` release the slot of an attempt recorded as not started (in windows that are still open); nothing else is released, so a call that may have run, an `Idempotent` call that was refused or cancelled, a `ResolveHalt`ed call and a saga's rollback stay counted until the window ends and a window only over-counts. `Recover` and `RecoverLoop` decide which windows are still open by the wall clock; they take no clock option. An agent built with `WithClock` reserves by its own clock, so under a clock far from the wall clock the reconcile pass may skip a window that is open by the agent's clock, and the slot stays counted until the window ends: an over-count, never a release of a slot that may have run. A tumbling window allows up to twice the limit across a boundary. A key with a byte outside ASCII is refused (the key is hashed from its text, and without Unicode normalisation two spellings of one customer would be two windows): normalise the key in your tool, or make it ASCII, before it reaches the rule. A rule keyed by something the model chooses is only as strong as its canonical form (pair it with an unkeyed total). See the [amount caps guide](guides/amount-caps.md).

**A writer of the store can undercount a cap window.** The cap ledger (`internal/capledger`) keeps its slots as ordinary journal entries, and they are unsigned: anyone who can write the store can append a fully valid release of a live marked reserve, which lowers the window's total and lets more through than the limit. The ledger refuses every slot that is not well formed (a wrong total, a release of nothing, a foreign currency), so a forged slot has to be one a correct writer could have written; it cannot refuse that one. What detects it is the `AuditedStore`: with the store anchored, a slot appended outside the engine changes the run's head, and `bide audit caps -sth ... -pubkey ...` compares each ledger with the heads and recomputes each window from its slots; entries after the newest head are reported as not attested and fail. `bide audit caps -unanchored` has no head to compare with, so it finds only what is inconsistent in the store itself (a slot that breaks the ledger's rules, a slot and the `cap-reserve` record of its run that disagree, a slot no run recorded): a writer who changes a slot and the run's record together passes it, and so does one who plants the loop's `cap-release:` record beside a forged release of a call that ran and failed (the audit does find a release of a call whose run holds a result the tool produced, and a release with neither the record nor a void fails when anchored and warns when not), and its first line says so. A signed head covers what the store was given when it signed: a slot written through the signing store is signed too. The audit compares each slot with the attempt's cap-intent (rule, key, amount, window) and the intent's window with the rule's index run, so a planted intent that moves a slot to another window is found; it does not re-apply the writer's clock bound (a setting, not recorded) but holds each intent's window to its call's own time (the window of the attempt marker's time, or the next one; for an attempt with no marker, not earlier than the window of the run's start, so a long-lived run can pin such an attempt to any later window), and it ignores index entries no intent uses. Someone who can write the store and sign heads can write a history that is consistent with itself: the audit finds contradictions, and does not find that. A head signs whatever the run held when it was signed, so a record planted before a later legitimate write to the same run is signed by that write's head. The same writer can also plant a cap-intent for an attempt before it first reserves, with a stale or a future window: that bypasses the clock bound (which runs only when an intent is first written) and moves the attempt's reservation into another ledger, so a window can be spent twice. It is the same trust boundary and the same detection. Without anchoring the window's total is only as trustworthy as the store's write access.

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

**A tool-use ID that holds `#` fails the turn.** The engine reserves `#` in a tool-use ID, to derive
a second approval subject from a call's ID, so a live model turn whose call has one fails with
`ErrToolUseIDInvalid` (wrapping `ErrModel` and `ErrProtocol`; `middleware.Retry` asks the model
again, and the rejected turn is never journaled). The shipped adapters pass the provider's ID
through, and none of their tests or generated IDs holds a `#`; whether a provider, or a model
server you point the OpenAI-compatible adapter at, never sends one is not something bide can know,
and a server that always does fails every turn that calls a tool. Only live turns are checked: a
journal written before this check, with a `#` in an ID, resumes, and is outside the guarantees
that rest on the reservation. `agent.Replay` used as the model of a new run is the exception: it
returns the recorded turns as live ones, so a recorded `#` ID fails that new run with
`ErrToolUseIDInvalid`.

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
`attempted_at` is its time, on the wall clock; the marker's own `recorded_at` is the same instant on the run's clock). Nothing reads these times to decide anything.

**A re-ask note is proven in what it supersedes, not in the policy it states.** Readers honor a re-ask note only where the journal holds a recorded tally the gate would ask again over and the note's `Supersedes` names it (`agent.ValidReaskNote`), so a note pre-inserted without that is ignored. A journal writer who does have write access can still pre-insert a note with the correct `Supersedes` and a forged `Need`, `Approvers` or `Requesters`; readers that only have the journal (`bide view`, `bide approve`, the intake's fallback, notification details, the verdict's `Reask`) then display that policy. It changes what is shown, not what counts: the gate never reads the note, every decision is signed by an approver's key, and the intake and the gate count under the application's live policy.

**A certificate-bound gate cannot start excluding the requester after it counted.** The gate's first count is `approval-tally:<id>:ident`. If that tally passed without excluding the requester and a redeploy then turns on `ExcludePrincipal`, the gate refuses with `ErrConfig` and the call does not run: unlike a bare-key gate it has no separation-of-duties re-tally name, because a re-ask of a certificate-bound gate is named by its generation (an exec hold of the previous generation) and nothing writes one for this case. Cancel the run and start it again. The error wraps `agent.ErrGateNeedsNewRun`, so a reader (`agent.EvaluateGate`, the approvals views) can say so. A certificate-bound gate is likewise refused, with `ErrConfig`, when its policy or resolver no longer has the certificate-bound approvers its journal says it was asked under, or has other approver kinds. See [the hitl-approval guide](guides/hitl-approval.md#a-gate-with-a-certificate-bound-approver-its-tally-its-holds-and-its-re-asks).

**The check before a certificate-bound call runs has limits.** (1) A revocation the CA records after the check passed is not seen by that call; the interval is the time the process takes to begin the tool, and the published latency bounds in [the operator guide](guides/certificate-approvers.md#published-revocation-latency) say what the gate can know and when. (2) A gate waiting for current status (`agent.PendingIdentityStatus`) sends no approval notification, because nobody is asked to decide; the phase note (`exec-hold`) and your own CRL monitoring are how it is noticed. (3) CRLs reach the `StatusSnapshot` through `crlrefresh.Refresher` (see the CRL refresher paragraph below); an application that does not run one adds them itself and raises its own alarm on a stale one. (4) A call runs on a count that is only provisional; no `RequireFinal` policy exists yet, so nothing holds a call until its count is final (the `held-until-final` phase is reserved for it). (5) Startup requires that at least one CA that can issue a leaf (a pinned intermediate, or the anchors when none is pinned) publishes at least twice within `ExecStatusAge`, but which pinned CA is a given leaf's issuer is known only from the accepted chain, so in a policy with several intermediates where only some fit, a leaf whose issuer does not fit is held by the check before its call, not refused at startup. (6) The check stops after eight rounds of holds in one drive with `ErrConfig`; the next drive goes on. (7) The check re-verifies the decisions the gate's identity record names; a tally with no identity record is refused, never accepted unchecked.

**Signing with a certificate (`bide approve -cert`, `bide-hsm approve -cert`) has limits.** (1) The chain is a PEM file; a certificate object on a PKCS#11 token is not read. (2) `bide keygen -csr` makes the certificate request for a new key file, and `bide-hsm csr` for a key in an HSM or KMS; both are Ed25519 or ECDSA P-256 only (no RSA, ML-DSA or hybrid request), and `bide-hsm csr` has been run against SoftHSM 2, an AWS KMS fake, the Google Cloud KMS emulator and a Key Vault fake, not a hardware HSM or a real cloud KMS. A key file that already exists has no request command (make a new key, or use the CA's own tooling). A request can ask a CA for a SAN and an EKU, but the CA decides what the certificate says and bide cannot request the policy OID. (3) The command checks the chain against the policy file, the key against the leaf and the validity window; it has no CRLs and does not judge revocation status, and it cannot tell that the gate counted the decision (that needs a signed head and current status): the application's gate decides. (4) The approve page in the browser cannot hold a certificate; it tells a certificate-bound approver to use `bide approve -to ... -cert`. (5) With `-unverified` the chain is not checked against a policy before signing; the gate still refuses a chain it does not accept, and the decision is then recorded and never counted.

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

**A person with two identifiers fills two seats of a certificate-bound gate.** The design's seat is the canonical identifier of the certificate (an opaque person URI is recommended), and bide cannot see that two different identifiers belong to one person; it refuses only two approver ids that map to one identifier. The control is the bank's: its CA's issuance policy gives one person one identifier, and the identity map lists one identifier per approver. Formal model 15 states the limit (`limits/alias-two-seats`) and the Go refinement test documents it (`TestModel15AliasLimit`).

**The CRL refresher has limits.** (1) HTTP and HTTPS only: LDAP distribution points are refused, and OCSP is not fetched. (2) A source with one URL over plain HTTP can be replayed until the CRL's `nextUpdate`; with several mirrors the refresher asks all and keeps the highest-numbered valid CRL, but a network attacker who controls every path can still withhold or replay. (3) A CRL signed by a delegated CRL signer, an indirect CRL and a partitioned CRL (an issuing distribution point with a scope flag, a reasons field or a relative name) are refused and reported, not fetched around. A CRL whose issuing distribution point is only a full name of URIs (what step-ca stamps) is read, and is evidence of absence only for a certificate whose own CRL distribution points name one of those URIs byte for byte (a certificate without one gets no status from it and the gate waits); a listing in it revokes regardless, and the refresher refuses one whose URI is not the URL it was fetched from. A delta CRL with any issuing distribution point is still refused, and a delta (which has none) is never merged with a base that has one. A certificate with several distribution points takes status from a CRL covering any one of them. The refresher's check of the point against the source URL is the refresher's only. The conditions for a bank are in the certificate approvers guide. (4) The CRL of the leaf's issuing CA is never read from a certificate: it must be configured, because only client-supplied leaves name it; `from_pinned_cdp` helps only for CAs above it. (5) Without an archive a restart forgets the rollback mark. The archive directory is the refresher's memory: whoever can write to it can lower a mark (every start reports an unacknowledged reset, and `Health().UnackedResets` shows it). (6) A mistaken huge CRL number needs an operator's `bide identity reset-high-water`; nothing resets a mark by itself, and a reset that would drop a revocation no other CRL lists is refused without `-accept-lost-listings`. (7) The alarm is not a literal half of the CRL's validity (a healthy root would alarm every cycle): it is `min(max(limit/2, period + overlap + tolerance), limit - interval)`, and a CA tier whose limit leaves no room for it is refused at startup. (8) `bide identity fetch-crl` needs a copy of the gate's CA timings (`-params`); it prints the policy digest of the approver it built, which equals the gate's only when `-params` also states every member the digest covers (`coverage_tolerance`, `w`, `min_w`, `routine_grace`, `clock_tolerance`, `allow_holds`, `consecutive_numbers`, `retained`, `effective_from`); the start-up event says which case it is and what an incomplete file lacks, and nothing enforces the comparison. (9) A server's `Date` header is advisory and never used for a decision. (10) `bide approve -cert` and `bide-hsm approve -cert` fetch nothing; `bide identity check` judges a certificate against CRLs read from the archive or fetched, and is the only pre-flight (see the limits below).

**`bide identity check` is a pre-flight, not a guarantee.** It runs the gate's own code on the files and CRLs it is given, from where it runs, at one moment. It cannot check the signature of a real decision, when a real decision becomes provable (the covering signed head and the log clock; no independent time source is shipped), whether a future decision will be provisional or final, revocations published after the check, the application's own HTTP client, proxy and DNS rules, or that the gate embeds the refresher or a `fetch-crl` job runs; every report lists these. A `-params` file can state every member the policy digest covers, but older files do not, and a gate's `independent_time` and a `count_live` of false cannot be declared: with a complete file `-expect-digest` is an equality, with an incomplete one it fails naming the missing members and `-expect` lists them as not verified. A report with `-no-status` or `-no-expect` is partial (exit 4) and never ready. See the [certificate approvers guide](guides/certificate-approvers.md#pre-flight-bide-identity-check).

**Certificate status is conformance-tested against EJBCA and a standalone Microsoft AD CS, not an enterprise AD CS CA.** The certificate-bound approver checks (`audit.IdentityPolicy`, the CRL status rule) are tested under `audit/testdata` against captures from EJBCA Community and from an AD CS standalone root and subordinate. An enterprise AD CS CA with certificate templates needs an Active Directory domain and has not been captured. A bank must capture its own CA with the `crl-fixtures.yml` workflow before go-live.

## Amount caps

**A cap reads an object member, not an array.** A rule (`agent.WithCaps`) names one argument by a path
of object members, and registration refuses a member that is an array or object. A bulk tool that
takes `payments: [{amount: ...}, ...]` cannot be capped by a line, and the input schema cannot say
whether a tool carries amounts elsewhere in its arguments, so such a tool is not detected: it has no
cap at all. Use one call per payment, or cap a total member the tool also takes.

**Caps are per currency, with no conversion and no total across rules.** A model that splits a payment
into many calls under the limit is not stopped by a per-call rule; the cumulative form (`sum ... per
day`) counts one rule's amounts across calls (see "Cumulative caps keep a reservation they cannot prove unused") and is not a total across rules or tools. bide does not convert
currencies (an exchange rate is data an attacker may influence), so a call in a currency no rule of the
argument covers gets the strictest action among the rules, and the currency is compared byte for byte
(`usd` is not `USD`).

**A cap does not read what a tool decodes differently.** The arguments are checked strictly (a repeated
member, or two members that differ only in case, anywhere in the document, is not assessable and gets
the rule's action), but a tool that reads a number as a double cannot represent an amount past 2^53
minor units exactly, so registration refuses a limit above it; an amount the tool reads from a member
the rule does not name is not capped.

**Caps refuse; they do not yet hold a call for approval.** A rule with `-> gate` (or `-> gate(name)`)
is refused at registration. The taint policy's escalation gate exists, but a cap gate is not built on it
yet, and a cap gate on a tool that has its own approval gate needs two gate instances on one call. Until
then a rule is `-> refuse`, which works on a tool with its own gate.

**A decorator must carry the tool's caps.** A tool that embeds a capped tool but declares its own `Spec` without the same rule lines is refused at registration (`ErrConfig`), as one that hides an approval gate or timeout is; a decorator keeps the rules by returning them in its `Spec`. Implementing `Unwrap` does not exempt it: a wrapper whose `Spec` lacks a rule line of a tool on its `Unwrap` chain is refused too, word for word.

**Compensation and plan flows are not capped.** A saga's rollback is not cap-checked, and neither is the
rollback's re-run of a call that already ran. A tool that carries caps cannot be a plan flow block (the
plan registry refuses it), so a flow cannot run it uncapped by accident.

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

**Two distinct types spelled the same are one type to the digest and to a description.** The topology digest
writes a named type as its package path and its name, so two types of one name declared in different functions
of one package (function-local types) spell alike and give the same digest, though `Load` compares Go types
and would refuse an edge between them. Anything that pins a digest to mean "this exact flow" (a stored digest,
a ticket, a paused run resumed under a flow) therefore cannot tell flows apart that differ only in such types.
`plan.InspectConfigDescribed` has the same limit, because a description holds the spelling and not the type;
the check with the program's own Registry does not. The digest is unchanged.

## The flow editor

**The editor's check is structural unless it is typed.** `bide edit` checks a config as `bide check` does: block,
predicate and merge names and the types are checked when a program loads the flow with its `plan.Registry`, and a
gate or a safety a block declares in Go is not visible. With `-blocks <file>` (or `flowedit.WithRegistry` or
`WithBlocks` in a program that embeds the editor) the editor is typed and checks the config as `Load` does, but a
description file is only what the program wrote when the file was made: the page says it is not checked against the
program, tells types apart by name only, and shows NOT CERTIFIED while the text has errors against the blocks. A
clean page is not `plan.Load` accepting the flow. Without `-blocks`, a changed `block` on an ungated effect node is
reported as weakened because the new block may carry no gate.

**A file with any YAML anchor or alias is refused by the editor.** `plan` reads such a file (the YAML converter resolves anchors, aliases and merge keys), but the editor's splice cannot place an edit inside an alias, and a walk that follows aliases can overflow the stack or expand exponentially, so it refuses the whole document and says why; edit that file in your text editor. It likewise refuses a repeated key, a key that is not a string, NaN and infinity, which the loader refuses too.

**The weakening classifier is a check, not a proof.** The banner and the confirmation show what `bide check
-diff` classifies: a gate removed or loosened, an `assert` line removed, loosened or no longer holding, an
node newly reachable without a gate, whatever its safety (a lowered `safety` label is itself reported). The
check does not read predicates. The confirmation is not written into the file; the
control that stops a weakened policy merging is the pull request, with `bide check -diff -check
-fail-on-weaken` as a required status and a policy owner's approval. See the [flow editor
guide](guides/flow-editor.md).

**A rename is matched to its node only inside the editing session, and only in the editor.** The editor
keeps the renames made since the page opened and reads the baseline as if they had been made in it, so
renaming a gated node is not reported as weakened; any real weakening made with it still is. The list
belongs to the page: reloading it or restarting `bide edit` forgets it, and the saved file is then
compared with the baseline by name. `bide check -diff -fail-on-weaken` in CI sees two files and cannot
know a rename: it pairs a node renamed with the same block, kind and edges (a pure rename passes), and
reports a rename beside another change to that node, its block or its edges, as a removed gate and a
new node, which a policy owner reviews and accepts.

**Removing a node in the editor removes the assertions that name it, and not those that only depend on it; a join input is not removed.** Removing a node also removes the `assert` lines that name it, which is a weakening and is reported as one; an `assert` line the grammar cannot read that has the node's name in it as a whole word refuses the removal (fix or remove the line first) rather than guess; an assertion that does not name the node but depends on it (`b reachable` after the only path to `b` is gone) is not removed and is caught by the weakened banner and the save confirmation. A node that is an input of a join is not removed until the join is changed or removed first (both are done in the editor).

**The approve page's look is tested in Chrome only.** `TestPage_LookInChrome` and the no-input test run in Chrome and Go; Firefox and Safari are not run. In particular, Firefox processes a `Link: <...>; rel=stylesheet` response header, which Chrome ignores for this page, and a CSP of the host can change what loads: the pin covers the bundle's bytes, not the headers or proxy of whoever serves them (an organisation hosting the bundle should serve it with the CSP `approvals.PageCSP` returns, which allows no other stylesheet).

**Forced colours and more contrast are tested in Chrome's emulation, not on a real contrast theme.** The viewer's and the editor's colour-only and contrast checks run in real Chrome under light, dark, high-contrast, `prefers-contrast: more` and `forced-colors: active` (the editor's also measure the colours the browser paints for its controls, selection ring and dialog); they are not Windows High Contrast on Windows, macOS Increase contrast, or a screen reader. A manual pass with those is still owed, on the viewer, the editor and the approve page (which takes the shipped themes by the system's settings alone; see the [themes guide](guides/themes.md)). Under forced colours the system drops shadows, so the editor's save dialog does not dim the page there; it is still modal and keeps its border in the system's colour.

**The editor's accessibility has automated and keyboard tests and no manual screen-reader pass yet.** The
keyboard-only run and axe-core in Chrome (light, dark and high-contrast, five states) pass in CI; they are not a screen
reader. To check by hand with VoiceOver, NVDA or Orca: the outline reads as a list of buttons named by
node, kind, safety and gate; selecting a node announces it; a form field reads its label and its help;
the draft's verdict is announced as you change it; the weakened banner is announced when it appears; the
save dialog reads its title, the items to confirm and the diff, and returns focus when it closes. Report
what does not read as described.

**A save is atomic, not exclusive.** The file is replaced by a rename of a temporary file, so a reader sees
the old file or the new one. Saves in one editor are serialized, so of several over the same hash exactly
one succeeds and the rest are conflicts. A write by another process between the editor's last check that
the file is unchanged (made just before the rename) and the rename is overwritten, . The config's directory is
opened once at startup and every read and write goes through that handle, so a directory swapped for a
link or another directory later cannot redirect a save. A file with other names (hard links) is refused on Unix, because the others
would keep the old text, and a read-only file is refused; on Windows hard links are not counted. The
owner and group are carried over where the user may set them; extended attributes, ACLs and SELinux labels
are not carried over (there is no portable way to see them without a new dependency). On Windows the open directory handle (the price of the swap protection) stops anything from renaming or removing the config's directory while `bide edit` runs. A process killed
between creating the temporary file and the rename leaves `.bide-edit-*` beside the config (mode 0600,
the config's text); the next `bide edit` in that directory removes such files that are ours and older
than ten minutes.

## Audit and proofs

**JSON documents nest at most 128 levels, and some readers have no size cap.** The strict JSON reader behind tool-call arguments, plan configs, theme files and audit exports (`audit.UnmarshalStrict`) refuses a document whose structure nests more than 128 levels, with an error naming the offset. JSON a journal keeps verbatim (a tool call's arguments, a tool's result) may nest up to about 10000 levels (the most encoding/json writes; a value inside a journal record counts the record's own wrapper, so exactly 10000 is refused and 9991 is read), so a run's export stays readable whatever a model or tool produced. Nothing bide writes nests more than a handful of levels, so this only matters if you feed bide documents of your own that deep. The reader's time and memory otherwise grow linearly with the document, and the audit export readers and tool-call arguments do not cap its size: put a size limit on the reader you pass in (for example `io.LimitReader`) if the source is not trusted. `plan.LoadReader` and the viewer's config readers refuse a flow config over 16 MiB.

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
`Result.ESSV1` set and a verdict should show it. Under `GODEBUG=fips140=only` the opt-in re-admits
SHA-1 into the process (that one comparison runs with enforcement off, as does the consistency check
for a token that carries both signing-certificate attributes). A token that uses SHA-1 as its CMS digest algorithm
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

**Erasing personal data cannot reach everything.** `bide journal redact` replaces chosen records of
a finished run with tombstones on SQLite and Postgres, and records each erasure in a signed trail that
`bide audit redactions` verifies ([retention guide](guides/retention.md#erasing-personal-data)); anchor
its heads, and erase through the command, since an erasure outside it is reported as unaccounted. The
command signs the trail with a key file; HSM and KMS signing go through the library until `bide-hsm`
gains it. On SQLite it overwrites the replaced bytes and truncates the write-ahead log, which a reader
holding the database open prevents (the command says so and exits 1); on Postgres the replaced row
versions stay until the operator's `VACUUM`. A hold lift's second-person authorization is judged against the lift record's own time, written from the tool's clock (the store has no writer-independent clock, as for the kill switch), so a lifter who controls the clock can use a lapsed authorization; the CLI uses the real clock. Legal holds (`bide journal hold`, `agent.Holder`) stop the store from erasing a held run and are recorded in the signed trail, but they stop only bide's own erasure (not a database administrator, a restore or a hand-written `DELETE`), and lifting one does not yet need a second person's signed authorization. A record someone redacted by hand and stamped with the time of an unfinished pass of the trail counts as accounted, limited to the records that pass was authorised to erase.
A journal holding a tombstone cannot produce a new absence set, run certificate or event stream
(`audit.ErrRedacted`): make that evidence first. It does not erase the run's final result
(`run:complete` is an end marker and stays), the record names, copies in exported journals,
evidence packages and approver receipts, the event store (`bide_events`, which has no tombstone for
an event leaf), Postgres dead tuples before `VACUUM`, SQLite free pages and the write-ahead log,
backups, replicas and WAL archives, logs and traces, or the model provider's copy. Govern event logs
hold opaque application strings and cannot be erased. A tombstone at an end marker's name counts as an end of unknown kind (a redaction may erase an end marker, by name): every reader finds the run over by the name it keeps and none reads how it ended from it (`agent.RunEnded`), so a rename gives no power, but a tombstone written over an end marker by hand is read as an end, and only `bide audit redactions` tells it from an erasure (`UNACCOUNTED` when the signed trail has no record of it; `NAME_MISMATCH` for a rename), and it is the one place that says how such a run ended (`ended: completed (per trail)`). A flow's conformance check (`Conform`) does not compare an erased completion's output. A tombstone at another name a redaction never replaces is corruption: `Status`, `bide view` and `bide audit` report it, `Recover` does not. A tombstone's name is outside the leaf hash, so it is unattested in an export or package (readers label it). The notifier's sweep of an aborted flow run
whose `run:start` was redacted attributes its events to the run ID's root, as it does for any run
whose `run:start` it cannot read. Sessions have no end marker, so a session's journal and turn runs cannot be redacted (the session reads its turns' runs), and a sub-agent's run can be redacted only once every run above it is over.

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

## Anchoring

The S3 Object Lock anchor (`anchor/awss3`, [Anchoring](guides/anchoring.md)) is the first production `audit.Anchor`. What it does not do:

- **It is tested against MinIO and a fake, never against AWS.** MinIO implements Object Lock, conditional writes and legal holds, and refuses to delete or shorten a compliance-locked version. It ignores `x-amz-expected-bucket-owner` and has no KMS in the test setup, so the owner check and SSE-KMS are tested against the fake only. The IAM actions the package needs (for example that `s3:PutObjectRetention` is what a write with a retention date requires, and that the lock headers are returned to a reader holding `s3:GetObjectRetention`) rest on AWS's documentation. The whole list of what is unverified: that `s3:PutObjectRetention` is what a write with a retention date needs; that the lock headers come back on a read only for a holder of `s3:GetObjectRetention` and `s3:GetObjectLegalHold`; that SSE-KMS returns the key ARN the read-back compares; that `x-amz-expected-bucket-owner` is refused with a 403; that the policy templates are complete; and that S3 lists a key at once after a write (documented as strongly consistent). [The runbook](guides/anchoring.md#trying-it-against-aws) tries each in a scratch account; do it before relying on the anchor, and report what differed.
- **Governance mode is not operator-proof.** Any principal holding `s3:BypassGovernanceRetention` can delete a head. It needs `Config.AllowGovernance` and warns on every start. Use compliance mode in a separate account; even then the account's owner can delete the objects after their retention date, close the account or disable the KMS key (the last makes heads unreadable, not altered).
- **Run and trail IDs in the anchor cannot be erased.** The ID is part of a head's signed bytes, so every object carries it and cannot be deleted before its retention date. Redaction does not reach the anchor. Use opaque IDs and keep the customer mapping in a store you can erase from. `Config.OnWarn` and `Config.RefuseSuspectIDs` catch an ID that looks like an email address or a long run of digits (also with spaces or hyphens between them); they are a tripwire, not a proof, and do not catch a name, free text or an obfuscated address.
- **Anyone who can write the bucket can add garbage, and a locked object cannot be removed.** The Reader skips and reports an object that is no head, so it stays usable, and `check` fails on it; a well-formed head the log key did not sign is an error and is not skipped.
- **A downloaded copy can be stale.** `bide-anchor verify` and `export -dir` show that what is in the directory is genuine, not that it is all there; compare the largest size with the journal's.
- **The bucket policy cannot stop the account's owner from removing it.** An organization service control policy on the anchor account is the control ([anchoring](guides/anchoring.md#the-iam-policies)); the SCP example there is untested.
- **A delete marker can hide a head from a plain listing, and an administrator can add one.** `s3:DeleteObject` without a version creates a marker and leaves the locked version. `Reader` lists versions and so still finds the head, and `bide-anchor check` reports the marker; a tool that lists the bucket without versions would not see it.
- **One retained object per journal write, unless you set an interval.** `AuditedStore` anchors after every write by default, so a busy trail produces millions of objects a year that cannot be deleted before their retention date, each with a write, a read-back and a KMS request. `WithMinInterval` bounds that; with it, records written in the last interval are not under a head, and after a crash what a process held back stays unanchored until the next start or recovery pass (`AuditedStore.ReanchorRuns`, the notifier's `AnchorTrails`).
- **A failed publish leaves a window.** `Publish` does not queue (a queue on the application's disk would not be tamper-evident). Between a failed publish and the next success the journal holds records no anchored head covers; the failure goes to `OnError`, and the next write or `Reanchor` publishes again. Alert on it, and on a latest head older than the journal.
- **The timestamp is AWS's, not a timestamping authority's.** An anchored object's `LastModified` is S3's clock and the head's own timestamp is the log signer's; neither is an RFC 3161 token.
- **`bide-anchor verify` is offline and cannot see the lock.** A downloaded file carries no lock state. `bide-anchor check` reads it with read-only credentials, and neither command shows the journal matches the heads: that is `bide audit` against the exported heads.

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

## Kill switch

- A stop does not cancel a call that has begun: it finishes and is journaled. A stop is seen by the
  call-level checks no later than the store shows it (on Postgres the control run must be read from
  the primary; a lagging replica adds its lag), and by drive and turn starts within the poll interval
  (default 1s).
- Approval deadlines keep running during a stop, so a long stop can turn a pending approval into an
  expiry and the call into a denial.
- Basic mode trusts store access completely: anyone who can write the store can stop, lift, or write
  records a basic process does not check. Audited mode needs keys, a policy, a pinned root and the
  verifier constructor; certificate-bound stoppers (the approver-identity machinery) are not built.
- Moving a deployment to audited mode needs the policy on every process first; a process left basic
  sees the stops as unknowable and holds covered side-effect calls.
- The staleness bound (default 15s) is an engineering figure the formal model does not cover; the
  model checks the synchronous case.
- `bide audit notifications -db` checks that every stop was told, from the control run (give `-stop-policy` for an audited deployment); it does not check the receivers' routing of stop events unless `-routing` is given, as for any kind. A deployment runs the sweep (`NotifyPending` with `WithNotifyStops`) or a `StopNotifier` in the process that holds its notifier; `bide stop` has no notifier and notifies nothing itself. The prompt path needs a `StopSwitch` in a process that runs, and the sweep sees a stop within its period, so a deployment that runs neither is told of no stop.
- `bide view`'s held-by-stop state is derived from the journal and the active stops: it cannot tell a run a live process is holding from one no process is driving, a tool stop is matched only against calls the journal shows no result for (a call the model has not proposed yet is not seen), and an agent stop can only say "may be held" because the agent identity is not journaled. `bide stop -lift` counts the first 2000 unfinished runs and stops after 5 seconds ("at least N held, count incomplete"). The banner is served again for a couple of seconds (it says when it was read).
- A stop is named by its scope, compensation and a generation, so concurrent identical stops cannot both be in force. Records written before generations (named by their stop ID) are read as they always were, and a scope two older writers set at once stays in force twice (one lift each); a writer that is not the current version does not take part in the once-only insert.
- **A writer with store write access can slow stop reads by filling the control run with junk.** An entry under `@stop/set/...` that is no stop never blocks a stop (SetStop steps over it, or writes the stop under its content-derived name), but every full read of the control run reads and folds every entry, so the cost of `SetStop`, of `State()` and of a `StopSwitch`'s first read grows linearly with the number of junk entries. Measured with junk under the global scope's generation names, on a loaded development machine, one `SetStop` for that scope took 8.6 ms with 1,000 entries, 102 ms with 10,000 and 844 ms with 100,000 on SQLite, and 59 ms, 150 ms and 2.0 s on Postgres. With 10,000 entries a switch's first read took 34 to 55 ms, a repeat read with nothing new 0.1 to 0.4 ms, and a read after one new entry 7 to 8 ms (the switch reads only entries after its cursor, and folds the run again when any arrive), so in steady state the check before a call is cheap, and a writer that keeps adding entries costs each reader a refold per new entry; those two were not measured at 100,000. A stop still lands and holds throughout. A guard test (`TestStopFlood_StopStillLandsInBoundedTime`, SQLite, and Postgres with `PG_DSN`) fails if any of these calls takes over 2 seconds at 10,000 entries or grows faster than linearly from 2,500. Deployments should keep the control run's store writable only by the processes that operate the kill switch; making the cost independent of junk needs an indexed prefix read in the store interface, which is a design item.
- `bide stop` writes signed leaves to the stop trail and does not sign or anchor heads: a deployment must run `bide audit stops -sign-head` on a schedule (and publish the head file where the store's writers cannot edit it), or `bide audit stops` fails on the unattested tail. `bide audit stops -sign-head` takes a key file; a log key in an HSM or a KMS is signed by `bide-hsm audit stops -sign-head`, which takes a key reference. A repeated stop after exit 5 reports the stop in force and writes nothing: `-reaudit` is what puts it on the trail.
- The stop trail records each signed record when the control writes it; a record written around the
  control (straight into the control run, with no trail) is found by `bide audit stops -db` or
  `-postgres-env`, not prevented. `Reaudit` appends what the trail lacks, and a signed record that reached the trail but never the control run (a crash between the two writes) is listed as unapplied or superseded, not as a forgery; `ApplyPending` applies the unapplied ones, an operator's decision that is itself a signed record (`bide stop -apply-pending`); a signed lift cannot be revoked, so whoever may apply can make an old pending lift effective. The order of the two
  writes (a stop to the control run first, every other record to the trail first) is checked by Model 16. A plan flow is checked only if it was given a switch (`Builder.WithStopSwitch`); one without, on a store with a control run, is refused.
- An agent-scope stop matches `Identity.Actor`: a run with no identity is never covered by one.
- A session turn and a `Step` are not held by a stop of a tool; a global or agent stop holds the run
  at its next drive or turn start.

## Untrusted content and the taint policy

The provenance declarations (`agent.Untrusted`, `agent.WithUntrustedInput`, `agent.WithTrustedInput`,
`agent.RetrievalUntrusted`, `agent.TaintExempt`, `agent.Trusted`) record what a run read. They are part of the
[design](design/untrusted-content-and-amount-caps.md) of untrusted-content tainting and amount
caps; `agent.WithTaintPolicy` ([guide](guides/untrusted-content.md)) acts on them. A taint policy bounds
the consequences of an injected request. It does not detect or prevent the injection. Its limits:

- **A mark is evidence, not a control, without a policy.** A run that read an untrusted tool's result still calls
  any tool its gates allow. With a policy, the side-effecting calls of a later turn need the escalation gate.
  Nothing in bide judges whether a text is an attack, and it will not.
- **Anything a human approves.** An approver who reads a convincing injected justification and approves is a
  successful attack. The policy does not clear taint on approval; each call is asked again.
- **`ReadOnly` tools are not gated.** A "read-only" fetch whose URL carries stolen data is an exfiltration
  channel; a wrongly declared `ReadOnly` is out of scope, as before.
- **Output the user reads, and laundering.** The final answer of a tainted run is not gated, and a tainted run
  that writes to a memory or index the next run trusts launders the content.
- **Text the host injects outside the journal** (a system prompt, a model middleware) is invisible to the marks;
  an input is trusted unless the entry point passes `WithUntrustedInput`.
- **Children and sessions inherit taint, with limits.** A sub-agent's run starts with the marks of the turn that
  called it, its tainted answer marks the parent's result, and a tainted session turn marks the turns after it
  ([guide](guides/untrusted-content.md#children-sub-agents-and-sessions)). A saga's rollback re-run of a sub-agent
  and the answer `ResolveHalt` injects carry no mark. The turns of a session before a policy joined are not
  retro-marked. A tool that starts a run by a path bide does not own (a goroutine of its own, another process)
  passes nothing.
- **A run is gated by the policy it started under.** A run that started with no policy is never gated for its
  marks; a no-policy process refuses the side effects of a policy-started run, and a changed policy digest holds
  a call asked under the old one. Roll the whole fleet before enabling a policy; an older binary ignores the
  policy and runs a tainted run's calls ungated.
- **The declarations report sees only what the host dumps.** `bide doctor -declarations` and `-diff` read a
  document the host writes from its agent; a tool behind a wrapper that hides its spec, a tool that runs an
  agent on a context of its own, and an agent the host does not dump are not in it. It lists a tool
  with no provenance declaration only when the policy does not require one.
- **Approval volume.** A deployment whose agents read outside text and write will be asked more. `TaintExempt` is
  the relief valve and is visible in the declarations digest.
- **`RequireProvenance` declares; it does not verify.** With it on, every tool says `Untrusted` or `Trusted` and
  every new run states its input's provenance, but a `Trusted` reason is the host's word: a tool that returns outside
  text under `Trusted` leaves no mark. Without the setting, an input is trusted unless the host says otherwise.
  A sub-agent's run and a resume are not asked for an input provenance.
- **A mark is only as good as the declaration.** A tool that returns outside text and declares nothing leaves no
  mark. A decorator that embeds the inner `Tool`, or
  implements `Unwrap`, and does not carry the declaration (`Untrusted`, a source in `Safety`, `Trusted`) is refused at
  registration, as one that weakens a gate, a timeout or a cap is. A wrapper that neither embeds the inner tool
  nor implements `Unwrap` is not seen: carrying the declaration through it is the wrapper author's duty.
- **`ResolveHalt` records no mark.** It has the journal, not the registered tools, so the result it injects
  carries no `Safety` and so no mark. A taint policy does not yet derive it from the tool's declaration by name.
- **Plan flow nodes are not covered.** A tool that declares `Untrusted` or `TaintExempt` is refused with
  `ErrConfig` as a plan node (`plan.Registry.RegisterTool`, `Builder.Tool`), because a flow has no taint rule yet.
- **MCP tools are not untrusted by default.** A tool bridged by `mcptools` declares nothing unless the host
  does: a server's results are outside text, and a host that does not mark them leaves no mark (with
  `RequireProvenance`, registration refuses the unmarked tool, which is how the omission is caught). A default
  that marks them under a policy is not built.
- **No offline check that the gate held.** `bide doctor -declarations` reviews what the host declares. Nothing yet
  proves from a run's journal that every side effect after a mark had a gate outcome or an exemption (a
  run-certificate property), and `bide audit` does not report a run under taint with an ungated side effect.
- **The relays do not show why.** The approve page, `bide approve -list`, `bide view` and the notifier's
  `approval.why` say why a call is held; the Slack and PagerDuty relays do not render it.
- **The experimental label.** The declarations and the new `Safety.Untrusted`, `ToolSpec.Untrusted`,
  `ToolSpec.TaintExempt`, `ToolSpec.Trusted`, `TaintPolicy.RequireProvenance` and `RunSettings` members may change in a minor release.
- **Downgrade.** A binary older than this change ignores the new record members (checked by decoding the golden
  with the binary built before it), so it neither records nor reads marks.

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

**A stop set and lifted with both times backdated can go unnotified.** A stop record's time is its writer's own, so the notifier and `bide audit notifications` do not trust it for a stop in force. A stop that is set and lifted before any notifier looked, with both records dated more than the window (`-stops-within`, default 7 days) in the past and no earlier record of the control run to make them a clock anomaly, is read as history and owes nothing. Its records are still in the control run (`bide stop -list`). Dating the control run by a time the writer does not choose (such as a signed trail head) would close it.

## Anchoring: Google Cloud Storage

The Google Cloud Storage anchor (`anchor/gcs`, [Anchoring: Google Cloud Storage](guides/anchoring-gcs.md)) is the second production `audit.Anchor`. What it does not do, and what has and has not been checked:

- **It was run once against a real bucket, by one person, on 2026-10-17, and is otherwise tested against an in-memory model.** One scratch project, one regional bucket (europe-west1) with a locked 24-hour retention policy, versioning, uniform bucket-level access, enforced public access prevention, a customer-managed default key and object retention enabled, using the Go client's JSON API. Checked there: `New` accepts it (and refused it before the policy was locked); `Publish` and the read-back pass; an object's key is reported as `<key>/cryptoKeyVersions/1`; a retained generation cannot be deleted (HTTP 403); deleting the live object makes its generation noncurrent and the Reader still returns the head; an object under a temporary hold cannot be deleted, overwritten or made noncurrent; a writer holding only `storage.buckets.get`, `storage.objects.create`, `storage.objects.get` and `storage.objects.list` can publish (including with a temporary hold) and read back with no Cloud KMS role of its own, and a reader with the reader role can read and audit; a create that sets a per-object retention is refused without `storage.objects.setRetention` (so `Config.ObjectRetention` needs `WriterObjectRetentionRole`); a hold is released by an identity holding only `storage.objects.update` (which cannot delete). Nothing else is claimed from that run.
- **Not verified.** Whether a project owner can delete a project whose lien was removed while objects are still within their retention, and what happens to the locked data then: Cloud Storage put a lien on `resourcemanager.projects.delete` (origin `storage.googleapis.com`) one second after the bucket with a retention policy was created; after the retention ended the run deleted the objects and the bucket, the project deletion was still refused by the lien, and the lien had to be removed by hand, after which the project was deleted. The data was already gone, so that run does not show what the lien protects against. How soft delete interacts with locked objects beyond what was seen: with the default 7-day soft delete, deleting a live retained object produced a noncurrent generation and nothing appeared as soft-deleted, and a delete after the retention ended produced soft-deleted objects that stayed listed for the soft delete period (the bucket could still be deleted). The gRPC client path (per-object retention is not reported over gRPC, so `Config.ObjectRetention` fails its read-back there). Multi-region and dual-region buckets, other locations, and the Cloud Storage emulators (neither was found to model locked policies, retention on delete, versioning or the create-only precondition). The IAM deny policy and organization policy examples in the guide.
- **A project owner can wait out the retention and delete.** After the retention date the bucket's owner can delete the objects (a deleted head stays listed as soft-deleted for the soft delete period, 7 days by default, and Google documents it as restorable then) and the bucket, and delete the project once its lien is removed by hand (the lien is best effort, survives the bucket's deletion, and is removable), or disable or destroy the Cloud KMS key (heads become unreadable, not altered). Use a separate project in a folder the application's administrators do not administer, with a retention at least as long as the journals the heads cover.
- **Deleting the live object makes it noncurrent.** Versioning keeps the generation and the locked policy protects it; `Reader` lists noncurrent generations and `Audit` reports each one. A tool that lists the bucket without versions would not see a head whose live object was deleted. A temporary hold stops even that, but the hold has no end date and releasing it needs `storage.objects.update`, held by a separate identity.
- **A locked retention policy cannot be undone and cannot be tested cheaply.** The bucket cannot be deleted until every object in it is past its retention, and a bucket with objects under a 7-year retention is there for seven years. Read [the runbook](testing/gcs-anchor-real-run.md) before locking anything.
- **Run and trail IDs in the anchor cannot be erased** (as for the S3 anchor: [anchoring](guides/anchoring.md#run-and-trail-ids-must-not-hold-personal-data)), anyone who can create objects in the bucket can add garbage that cannot be removed (the Reader skips and reports it), a downloaded copy can be stale, one retained object is written per journal write, a failed publish leaves a window, and the timestamp is Google's, not an RFC 3161 token.
- **A failure that is not an API answer is reported as unavailable.** The adapter treats any error that is neither a Cloud Storage API response nor a gRPC status it maps (permission denied, unauthenticated, not found) as `ErrUnavailable`, so a credential problem that surfaces as a plain error (for example a failed service account impersonation) reads as "unavailable" while its message names the cause.
