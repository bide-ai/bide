# Examples

Runnable programs that exercise Bide end to end. Each is a `main.go` in its own
directory with a header comment giving a one-line description and its run command.

Model-centric examples call an OpenAI-compatible endpoint (here OpenRouter) and read an
API key from the environment; set `OPENROUTER_API_KEY` before running them. Examples
marked "offline" need no key and no network.

Most examples are packages of the root module: run them from the repository root with
`go run ./examples/<name>`. Four are their own modules because they import an adapter module with
its own dependencies (`approval`, `plan`, `mcp`, `observability`); `cd` into the directory and run
`go run .` (the `go.work` at the root resolves the local modules; with `GOWORK=off` each module's
`replace` directives do the same).

## Authoring basics

Start here to learn the core authoring surface: the agent loop, typed results,
streaming, sessions, sub-agents, parallel steps, and retrieval.

| Example | What it shows | Run |
|---------|---------------|-----|
| `smoke` | The agent loop end to end against a live model, with tool calling. | `OPENROUTER_API_KEY=sk-... go run ./examples/smoke` |
| `typed` | `RunTyped[T]`: do work with tools, return a strongly-typed struct (note `RunTypedNative` for provider-native structured output). | `OPENROUTER_API_KEY=sk-... go run ./examples/typed` |
| `streaming` | `Agent.Stream`: range lifecycle Events for token deltas and tool start/finish, then `Final` for the answer. | `OPENROUTER_API_KEY=sk-... go run ./examples/streaming` |
| `session` | Durable multi-turn `Session.Send`, then rebuilding the transcript from the store. | `OPENROUTER_API_KEY=sk-... go run ./examples/session` |
| `subagent` | A parent agent delegating to a `SubAgent` exposed as a tool, sharing one store. | `OPENROUTER_API_KEY=sk-... go run ./examples/subagent` |
| `parallel` | `agent.Parallel` over several `Task[T]`: durable, auditable fan-out/fan-in (offline). | `go run ./examples/parallel` |
| `rag` | A trivial in-memory `Retriever` wired via `WithRetrieval` (classic) and `RetrievalTool` (agentic). | `OPENROUTER_API_KEY=sk-... go run ./examples/rag` |

## Accountability, durability & integration

The accountability layer: governance, delegation and authority, proofs, crash-safety,
evaluation, and integration seams.

| Example | What it shows | Run |
|---------|---------------|-----|
| `authority` | Authority-as-governed-state: a principal's delegated limit is a state variable, seeded from identity (offline). | `go run ./examples/authority` |
| `delegation` | A non-repudiable delegation chain across sub-agents, each hop signed and never widened (offline). | `go run ./examples/delegation` |
| `earned-authority` | Authority earned from a provable track record: widens on a clean streak, resets on an anomaly (offline). | `go run ./examples/earned-authority` |
| `compliance` | A KYC-shaped flow: parallel provable checks, a governed decision, an offline proof against a signed tree head (offline). | `go run ./examples/compliance` |
| `proof-carrying-run` | A run that ships one offline-checkable certificate of behavioral-property compliance (offline). | `go run ./examples/proof-carrying-run` |
| `quorum` | A governed model quorum: k-of-n agreement admits the commit, with the whole vote in the audit trail (offline). | `go run ./examples/quorum` |
| `compose` | Compositional construction: verify a subsystem once, embed it as a black box into larger systems (offline). | `go run ./examples/compose` |
| `mesh` | A coordination-free safety mesh where governed state constrains agents cyclically (offline). | `go run ./examples/mesh` |
| `chaosbench` | The crash-injection benchmark: an exhaustive crash-point sweep proving a side effect never double-fires (offline). | `go run ./examples/chaosbench` |
| `plan` | The `plan` flow builder driving an order-triage flow on a SQLite journal: the declared diagram, a typed result, `Conform`, and an offline proof that the run followed the signed flow digest (offline). Its own module. | `cd examples/plan && go run .` |
| `recover` | Crash recovery: a supervisor resumes an interrupted run from its journal without re-firing side effects. | `go run ./examples/recover` |
| `interrupt` | Human-in-the-loop: a tool pauses the run for a durable approval decision, then resumes. | `go run ./examples/interrupt` |
| `approval` | m-of-n human approval across separate processes on a SQLite journal: 2 of 3 approvals, each signed over the exact call, gate a refund; a forged signature and an ineligible approver are ignored without locking anyone out; and an auditor verifies the evidence offline with public keys only, in Go or with `bide-audit verify-approvals` (offline). Its own module. **Its keys are predictable demo keys** (each is SHA-256 of a public name), so anyone can sign as any approver; never reuse them. | `cd examples/approval && go run .` |
| `signals` | Durable timers and external signals: a run sleeps or waits for an event and resumes on delivery. | `go run ./examples/signals` |
| `hedge` | The hedged-model middleware: race a primary against a backup and take the first good answer (offline). | `go run ./examples/hedge` |
| `eval` | The statistical evaluation harness: labeled cases, metrics, repeated runs, a pass-rate report (offline). | `go run ./examples/eval` |
| `observability` | OpenTelemetry GenAI spans in one call (`trace.Instrument`), exported to stdout, with token usage and cost on the chat span (offline). Its own module. | `cd examples/observability && go run .` |
| `mcp` | Wiring runtime MCP tools into an agent from an in-memory MCP server, with trusted annotations marking a tool retry-safe (offline). Its own module. | `cd examples/mcp && go run .` |
| `coordination` | Multi-agent coordination through shared durable state. | `go run ./examples/coordination` |
| `webhook` | Driving an agent from an inbound messenger webhook, with idempotent redelivery handling. | `go run ./examples/webhook` |
