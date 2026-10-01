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
- [Flows](#flows)
- [Audit and proofs](#audit-and-proofs)
- [Stores](#stores)

## Durability and recovery

**Durability needs a durable store and waker.** `MemStore` and `MemWaker` are for local development
and tests. They keep everything in memory, so after a real crash there is nothing to recover, and a
pending `Sleep` has no timer to wake it. In production, use SQLite or Postgres, and either a durable
waker or an external scheduler that re-drives sleeping runs.

**You supply the resume function.** `agent.Recover` finds incomplete runs and re-drives them, but only
your deployment knows which agent drives each run and which waker and clock to bind. Pass that as the
`resume` function. The run's input and entry point are journaled at its first drive (read them with
`agent.RecordedStart`), and a resume with a different input or entry point is `ErrConfig`. `Recover` skips sub-agent runs, which their root run drives; a `resume`
that does not own any other run it is handed should do nothing. See [Crash recovery](guides/debugging.md#4--crash-recovery-lister-and-recover).

**Takeover needs a process that keeps looking.** `agent.Recover` is one pass: a run whose holder
has died but whose lease has not yet expired is skipped. Run `agent.RecoverLoop` in every worker for
the life of the process, and a dead holder's run is taken over within about one pass interval (half
the lease TTL by default) of its lease expiring, as long as a pass is short.

**A long recovery pass delays takeover.** A pass costs about five store round trips for each
unfinished run it lists, halted runs included, and the next pass starts only after this one has
started all of its drives. With many unfinished runs (halted runs left unresolved count) or a slow
store, a pass can outlast its interval, and a dead holder's run can then be picked up as much as a pass's
length later than the interval suggests. Resolve halted runs promptly, and measure a pass against
your store if takeover time matters. Bounding a pass's cost is planned after v0.9.0.

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

**Renaming the Postgres store's schema needs one step.** The `bide_next_seq_v1` function names
its schema, so after `ALTER SCHEMA ... RENAME` inserts fail and `Open` refuses it: drop the
function and `Open` again, and the migration recreates it.

**Crash safety is tested and model-checked within bounds, not proven for the code.** The crash
tests fail the store at every write point, across hundreds of randomized multi-crash schedules, and
check that no side effect fires twice and every rollback completes. The protocol designs (claims,
the approval gate, flows, spend accounting and the bide protocol's claim rules) are TLA+ models that
TLC checks in every interleaving within each configuration's bounds; nothing is proven beyond those
bounds, and the models state the rules, not the Go code, whose correspondence is checked by review
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
`RunSaga` returns `*OutcomeUnknown` for a person or a reconciler to resolve with `ResolveHaltRef`. See
[Sagas](guides/durable-steps.md#sagas-transactional-agents-with-reverse-order-compensation).

**A cut-off retry-safe call keeps no record of its safety.** A call that was retry-safe when it
ran writes no attempt marker. If it is cut off before its result is recorded and its tool is then
relabelled a side effect, a resume runs it again and a saga rollback treats it as never started.
A completed call does not have this gap: its result records whether it ran `ReadOnly`.

**Rollback follows the sub-agent tree.** Compensation runs in one order, through the tree of
sub-agents. Independent agents changing shared state concurrently need
[governed state](guides/governance.md), not a saga.

## Parallel tool calls

A model turn's tool calls run concurrently, unbounded by default. Use `SetMaxConcurrency(n)` to cap
them, or `SetMaxConcurrency(1)` to run them one at a time. Each side effect still fires at most once,
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

**Structured output varies by provider.** `RunTypedNative[T]` uses the provider's JSON-schema mode,
which the OpenAI-compatible and Gemini adapters support and the Anthropic adapter does not (it
returns `ErrConfig` rather than run unconstrained). With Anthropic, use `RunTyped`, which works
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

**Settings apply to the whole agent.** `WithSampling`, `WithSystemPrompt` and `WithMaxTurns` are set on
the agent. There is no per-`Run` override yet; use a separate agent for different settings.

## Approval and quorum

**Approvers are a fixed, named set.** An m-of-n approval gate names its approvers up front. There are
no weighted votes, role rules (such as "at least one from risk"), delegated approval, or deadline for
a gate that never reaches k. bide checks signatures against the keys you provide; linking a key to a
person is your identity provider's job. It refuses two approvers whose verifiers resolve to one key,
but it cannot see that one person holds two different keys. Keep old public keys after a rotation so old evidence still
verifies. See [Human approval](guides/hitl-approval.md#scope).

**Flows cannot hold an approval gate yet.** A `plan` flow with an approval node fails to build with
`ErrConfig` rather than running the node unapproved. Put the gate on an agent tool instead.

**A quorum needs short, fixed answers.** Voters must pick from a set of labels, so a quorum does not
apply to free-form output. Agreement lowers the risk of one model's mistake, but models can be wrong
together. A tie is never agreement. See [Quorum](guides/quorum.md#what-a-quorum-does-not-do).

## Flows

**Fan-in has fixed arity.** `Join2` and `Join3` combine two or three branches; there is no join over
a variable number of branches.

**Some wiring is rejected.** `Build` refuses wiring the runtime cannot run as declared, and names the
step: for example, a step fed by several producers without a join, or nested loops.

**Only Steps inside a flow node are scoped to it.** An `agent.Step` or `Parallel` task a node's body
runs is recorded per node and per loop iteration; an `Interrupt`, `Await` or `Sleep` it takes is not,
so give a pause inside a loop body a name unique per iteration. See [Flows](guides/flows.md).

**`Model` nodes expect JSON.** A `Model` node decodes the reply as JSON into its output type, so
prompt the model for JSON. See [Flows](guides/flows.md#limits).

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

**SQLite is for one machine.** SQLite allows one writer at a time; a writer waits up to 30 seconds
for the lock before failing. Its leases coordinate the processes that share one database file on
one machine, not nodes: the file must be on a local disk, since SQLite's locking does not work over
NFS. Use Postgres for more than one node. A forward jump of the system clock expires SQLite leases
early (their expiry is computed from the clock), which the attempt claims keep safe.
