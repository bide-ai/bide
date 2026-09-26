# Extension points: the ports and adapters

The framework is built on a small set of **ports** (Go interfaces) with reference
**adapters** (built-in implementations). You wire your own infrastructure in by implementing
a port; the reference adapters exist for tests, local dev, and as a template for a real
backend. Every built-in adapter asserts its contract with a `var _ Port = (*Adapter)(nil)`
line, so a breaking change to a port fails to compile against the adapters.

This page documents each port: its method set, what an implementer provides, and the
reference adapters that ship with it.

| Port | Package | What it abstracts | Reference adapter(s) |
|---|---|---|---|
| `Model` | root | the provider (LLM) primitive | `model/anthropic`, `model/openai` |
| `Durable` | root | the crash-safe journal substrate | `MemStore`, `store/sqlite`, `store/postgres` |
| `Tool` | root | an action the agent can take | `Func`, `CompensatedFunc`, `mcp` tools |
| `Compensator` | root | how a tool undoes its side effect (sagas) | `CompensatedFunc` |
| `Retriever` | root | bring-your-own RAG | your store; wired via `RetrievalTool` / `WithRetrieval` |
| `Anchor` | `audit` | out-of-band anchoring of a commitment | `MemAnchorLog` |
| `EventStore` | `audit` | durable event-trail persistence | `MemEventStore` |

## `Model`: the provider primitive

```go
type Model interface {
	Stream(ctx context.Context, req Request) (*Stream, error)
}
```

Streaming is **first-class**: `Stream` is the only method an adapter must implement.
`agent.Generate` is a convenience drain built on top (stream and assemble in one call), the
opposite of frameworks that make blocking generation primary and bolt streaming on later. An
implementer maps a provider's wire format onto normalized `Event` values (`TextDelta`,
`ReasoningDelta`, `ToolCallDelta`, `Finish`) pushed onto a channel wrapped by
`agent.NewStream`.

**Reference adapters.** `model/anthropic.New(apiKey, opts...)` and
`model/openai.New(apiKey, opts...)` both return a `*Model` satisfying the port, with options
like `WithModel`, `WithMaxTokens`, and `WithPromptCache` (Anthropic).

## `Durable`: the crash-safe substrate

```go
type Durable interface {
	// Do returns the recorded Record for (runID, name) without running fn if present;
	// otherwise runs fn, records the returned Record (with Name set), and returns it.
	// If fn errors, nothing is recorded; the step re-runs on the next attempt.
	Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error)
	// History returns all recorded steps for a run, in order.
	History(ctx context.Context, runID string) ([]Record, error)
}
```

`Durable` is named-step memoization: `Do` runs a step **at most once** per `(runID, name)`.
A recorded step returns its `Record` without re-running `fn`; if `fn` errors, nothing is
recorded, so the step re-runs on the next attempt. `History` returns the ordered `Record`
sequence, which is the run's full replayable history. The side-effect-safety layer (`Safety`
/ `ResumeHalt`) sits *above* this and is substrate-agnostic.

**Reference adapters.** `agent.NewMemStore()` is the in-memory implementation for tests and
local dev (it single-flights concurrent `Do` on the same `(runID, name)` so a side effect
cannot fire twice under in-process concurrency). `store/sqlite.Open(path)` and
`store/postgres.Open(ctx, dsn)` are persistent backends; a real store enforces at-most-once
across processes with a primary key / `ON CONFLICT` on `(run_id, name)`.

### Implement your own store

A `Durable` needs to satisfy exactly one invariant: `Do` records the result of `fn` under
`(runID, name)` and never runs `fn` a second time once a result is recorded. Here is a
minimal, correct in-memory implementation (equivalent in shape to `MemStore`, minus its
single-flight optimization):

```go
package mystore

import (
	"context"
	"sync"

	agent "github.com/dayna/go-agents"
)

type Store struct {
	mu   sync.Mutex
	runs map[string]map[string]agent.Record // runID -> name -> record
	ord  map[string][]string                // runID -> names in insertion order
}

func New() *Store {
	return &Store{runs: map[string]map[string]agent.Record{}, ord: map[string][]string{}}
}

var _ agent.Durable = (*Store)(nil) // port/adapter contract

func (s *Store) Do(ctx context.Context, runID, name string,
	fn func(context.Context) (agent.Record, error)) (agent.Record, error) {

	s.mu.Lock()
	if byName, ok := s.runs[runID]; ok {
		if rec, ok := byName[name]; ok {
			s.mu.Unlock()
			return rec, nil // memoized: do NOT re-run fn
		}
	}
	s.mu.Unlock()

	rec, err := fn(ctx) // run without holding the lock (fn may do model/tool I/O)
	if err != nil {
		return agent.Record{}, err // not recorded: re-runs on the next attempt
	}
	rec.Name = name

	s.mu.Lock()
	defer s.mu.Unlock()
	byName := s.runs[runID]
	if byName == nil {
		byName = map[string]agent.Record{}
		s.runs[runID] = byName
	}
	if existing, ok := byName[name]; ok { // a concurrent write landed first
		return existing, nil
	}
	byName[name] = rec
	s.ord[runID] = append(s.ord[runID], name)
	return rec, nil
}

func (s *Store) History(ctx context.Context, runID string) ([]agent.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := s.ord[runID]
	out := make([]agent.Record, 0, len(names))
	for _, n := range names {
		out = append(out, s.runs[runID][n])
	}
	return out, nil
}
```

For a cross-process backend, replace the map with your database and let a unique constraint
on `(run_id, name)` enforce the at-most-once write: on a conflicting insert, read back and
return the already-stored `Record` instead of the one `fn` just produced.

## `Tool`: an action the agent can take

```go
type Tool interface {
	Name() string
	Description() string
	ArgsSchema() json.RawMessage // provider-neutral; the schema package dialectizes it
	Safety() Safety              // how the tool may be retried on resume
	Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}
```

The interface is untyped (`json.RawMessage`) so heterogeneous tools share one type,
including runtime `mcp` tools whose schema is only known at connect time. Most native tools
never implement this by hand: `agent.Func[In, Out](name, desc, safety, fn)` wraps a typed Go
function and derives `ArgsSchema` from `In` at construction, so changing `In` is a
compile-time change. `Safety` declares retry behavior on resume (`ReadOnly`, `Idempotent`,
`IdempotencyKey`, `RequiresApproval`) and maps directly onto MCP annotations (see
[MCP.md](../guides/mcp.md)).

## `Compensator`: how a tool undoes its side effect

```go
type Compensator interface {
	// Compensate undoes a completed call. args are the tool's x	// result is what Call returned. Must be idempotent (a mid-rollback crash re-runs it).
	Compensate(ctx context.Context, args, result json.RawMessage) error
}
```

An optional interface a `Tool` implements to declare how to roll back its write. In a saga
run (`RunSaga`), if a step fails after earlier writes succeeded, the completed compensatable
writes are rolled back in reverse order, automatically and recursively through sub-agent
trees. `agent.CompensatedFunc[In, Out](name, desc, safety, do, undo)` builds a typed tool
that declares both its forward action and its compensator.

## `Retriever`: bring-your-own RAG

```go
type Retriever interface {
	Retrieve(ctx context.Context, query string, k int) ([]Doc, error)
}
```

The bring-your-own-RAG port: given a query, return the top-k relevant `Doc` values from your
store (pgvector, Pinecone, a file index, anything). go-agents ships no vector store and no
embedder; you implement `Retrieve` against infrastructure you already run and wire it in with
`agent.RetrievalTool(r, k)` (agentic: the model searches on demand) or
`agent.WithRetrieval(r, k)` (classic: top-k auto-injected as context on each user turn). See
[RAG-MEMORY.md](../guides/rag-memory.md).

## `Anchor`: out-of-band anchoring (`audit`)

```go
type Anchor interface {
	Publish(ctx context.Context, runID string, sth SignedTreeHead) error
}
```

The bring-your-own port for out-of-band anchoring: publish a `SignedTreeHead` to a trust
domain separate from the app (a Certificate-Transparency-style log, a notary / timestamping
service, another account's WORM store, a public ledger). Anchoring is what upgrades
*integrity* to *tamper-evidence*: a Merkle root stored in the same database an attacker
controls can be rewritten and rehashed, so the guarantee only bites once the signed head is
committed to a domain the app tier does not fully control.

**Reference adapter.** `audit.NewMemAnchorLog()` is a reference external transparency log:
an append-only, independently Merkle-committed record of published STHs. Because it keeps its
own RFC 6962 tree over the entries, a third party can verify that the anchor log itself only
grew (`ProveConsistency`) and that a specific STH was anchored (`Prove`). In a real
deployment the anchor lives in a different trust domain than the journal; this in-memory
version is for tests and local dev. See [AUDIT.md](../guides/audit.md).

## `EventStore`: durable event-trail persistence (`audit`)

```go
type EventStore interface {
	// Append durably records leaf at position seq (0-based, contiguous) for runID.
	// Append-only and idempotent on (runID, seq): re-appending the same bytes is a no-op,
	// a DIFFERENT leaf at an existing seq must error (fork/tamper), and a seq beyond the
	// next position is a gap and must error.
	Append(ctx context.Context, runID string, seq int, leaf []byte) error
	// Load returns every leaf for runID in seq order (0..n-1), or nil if unknown.
	Load(ctx context.Context, runID string) ([][]byte, error)
}
```

The bring-your-own port for a compliance trail that has to outlive the journal (keep 7 years,
on WORM storage, in a different trust domain). You implement `Append` / `Load` against an
append-only backend you run (a Postgres table with `UNIQUE(run_id, seq)` and insert-only
grants, object storage with object-lock/WORM, or a log). The trail is fed from the durable
journal projection (`agent.ReplayEvents`, see [DEBUGGING.md](../guides/debugging.md)), not the live
stream, so re-mirroring after a crash appends the same leaves at the same positions
(idempotent). `audit.PersistJournal` drives that mirroring; `audit.LoadEventLog` rebuilds an
`EventLog` from the store for `Root` / STH / proofs even after the journal is deleted.

**Reference adapter.** `audit.NewMemEventStore()` is the in-memory implementation; it
enforces the same append-only, idempotent, contiguous contract a real backend would enforce
with a unique constraint and insert-only permissions.
