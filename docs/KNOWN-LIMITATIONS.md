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
the lease TTL by default) of its lease expiring.

**Leases prevent duplicate work, not duplicate side effects.** With a store that supports leases
(`MemStore` in one process, Postgres across processes), each run is normally driven by one holder at
a time, and another node takes over if the holder dies. The holder renews its lease from half the
TTL on, retries a failed renewal, and cancels its drive with `agent.ErrLeaseLost` if no renewal has
succeeded by three quarters of the TTL, a quarter of the TTL before any other node could take the
lease. That bound holds for a process that is running. A holder that stalls past its lease (a long
GC pause, a suspended VM, a network partition) can wake up still driving and take a step before it
notices. That is safe: at-most-once rests on the attempt claim written before each side effect, not
on the lease, so the second driver stops with `*ResumeHalt` instead of firing again. The cost is
repeated work, such as a model call made twice.

**Leases are not fenced.** A fencing token (a number the lease hands out that every write must
carry, so the store rejects a write from a holder whose lease was superseded) would turn the lease
into mutual exclusion for journal writes. bide does not use one, for three reasons. The effect that
matters happens outside the store (a charge, an email), where no token can be checked, so fencing
the journal would not stop a stalled holder's effect; only the attempt claim, written before the
effect, can stop a second one. The journal writes that a stalled holder can still make are safe
without it: every step is recorded at most once by name, so its write either is the step's one
record or loses to the one already there. And fencing would make every journal write depend on the
lease, so a store without leases (SQLite) could not offer the guarantee at all.

**Crash safety is tested, not formally proven.** The crash tests fail the store at every write point,
across hundreds of randomized multi-crash schedules, and check that no side effect fires twice and
every rollback completes. That is strong evidence, but it is not a machine-checked proof over every
possible interleaving. A crash is modelled as a failed durable write followed by the run unwinding,
which matches a process dying around its writes. See [How bide is verified](testing/verification.md).

**`run:complete` appears in diagrams.** A finished run records a `run:complete` step, so
`RenderMermaid` shows it just before `done`. This is expected.

## Sagas

**Each forward step must be all-or-nothing.** When a step fails, the steps before it are
compensated, but the failing step itself is not: it has no recorded result to undo. A step must not
leave a partial side effect behind when it returns an error. Make it atomic or idempotent.

**An unknown outcome stops the saga instead of rolling back.** If a step that cannot be retried
crashed after it started but before its result was recorded, bide cannot know whether it happened, so
`RunSaga` returns `*ResumeHalt` for a person or a reconciler to resolve with `ResolveHalt`. See
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

**Replay does not know the provider's finish reason.** The journal records each turn's message and
token usage, so a replayed run reports the same usage and stops on the same token budget. The finish
reason is not recorded: a replayed turn ends with `tool_use` when it has tool calls and `stop`
otherwise. Only turns that ended with one of those two reasons are journaled (a turn cut off at its
token limit or stopped by a filter is an error; see the finish reasons in docs/guides/models.md), so
no journaled turn's outcome changes on replay.

**MCP tools are untyped.** Tools discovered from an MCP server at runtime use raw JSON arguments,
because Go cannot create a struct type from a schema at runtime.

**Settings apply to the whole agent.** `WithSampling`, `WithSystemPrompt` and `WithMaxTurns` are set on
the agent. There is no per-`Run` override yet; use a separate agent for different settings.

## Approval and quorum

**Approvers are a fixed, named set.** An m-of-n approval gate names its approvers up front. There are
no weighted votes, role rules (such as "at least one from risk"), delegated approval, or deadline for
a gate that never reaches k. bide checks signatures against the keys you provide; linking a key to a
person is your identity provider's job. Keep old public keys after a rotation so old evidence still
verifies. See [Approval](guides/approval.md#scope).

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

**A flow node cancelled before its body starts still halts.** Tool calls and `agent.Step` record
that an attempt never started when they are cancelled after writing its marker and before calling
the effect, and re-attempt it on resume. A `plan` flow's non-idempotent node does not yet: a
cancellation in that gap leaves its marker without a result, and the resume stops with
`*HaltAmbiguous` for a person to confirm, as after a crash.

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

**SQLite is for one machine.** SQLite allows one writer at a time; a writer waits up to 30 seconds
for the lock before failing. It does not support leases, so it cannot coordinate several processes
driving the same runs. Use Postgres for more than one node.
