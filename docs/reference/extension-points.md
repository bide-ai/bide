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
| `Model` | root | the provider (LLM) primitive | `model/anthropic`, `model/openai`, `model/gemini` |
| `Store` | root | the crash-safe journal substrate, under `Journal` | `MemStore`, `store/sqlite`, `store/postgres` |
| `Tool` | root | an action the agent can take | `Func`, `CompensatedFunc`, `mcp` tools |
| `Compensator` | root | how a tool undoes its side effect (sagas) | `CompensatedFunc` |
| `Retriever` | root | bring-your-own RAG | your store; wired via `RetrievalTool` / `WithRetrieval` |
| `Anchor` | `audit` | out-of-band anchoring of a commitment | `MemAnchorLog` |
| `EventStore` | `audit` | durable event-trail persistence | `MemEventStore` |

## `Model`: the provider primitive

<!-- docsnip: api agent -->
```go
type Model interface {
	Stream(ctx context.Context, req Request) (*Stream, error)
}
```

Streaming is **first-class**: `Stream` is the only method an adapter must implement.
`agent.Generate` is a convenience drain built on top (stream and assemble in one call), the
opposite of frameworks that make blocking generation primary and bolt streaming on later. An
implementer maps a provider's wire format onto normalized `Event` values (`TextDelta`,
`ReasoningDelta`, `ToolCallDelta`, `Finish`).

An adapter that reads a response as it arrives builds its stream with `agent.NewStreamFunc`:

<!-- docsnip: setup ctx context.Context; resp *http.Response; ev agent.Event; returns (*agent.Stream, error) -->
```go
return agent.NewStreamFunc(ctx, func(send func(agent.Emit) bool) {
	defer resp.Body.Close()
	for /* each event read from resp.Body */ {
		if !send(agent.Emit{Event: ev}) {
			return // the consumer stopped reading, or ctx was cancelled
		}
	}
}), nil
```

`send` returns false once the consumer breaks out of `Stream.Events`, calls `Stream.Close`, or
cancels ctx, so the adapter stops and releases the response. After cancellation no further
event is delivered and the stream ends with the context's error, so a response cut short is
never read as a complete one. A turn ends with a `Finish` event, sent only once the provider
has signalled the end of the turn: a stream that closes without one reads as
`agent.ErrIncompleteResponse` (an `ErrModel`), so a response cut off partway fails the model
call, which retry middleware can repeat, instead of being journaled as the model's answer.
The `Finish` is the turn's last event: an event after it reads as `agent.ErrStreamProtocol`
(an `ErrModel` and an `ErrProtocol`). An adapter sends it on the provider's end-of-turn signal
only, never on usage alone, and fails with `agent.ErrStreamProtocol` on content the provider
sends after that signal. A `Finish` whose usage has a negative count fails the call with
`agent.ErrNegativeUsage` rather than lowering the run's totals; so does negative usage a
middleware returns.
The `Finish` names why the turn ended in the neutral `agent.FinishReason` vocabulary
(`FinishStop`, `FinishToolUse`, `FinishLength`, `FinishFiltered`), with the provider's own value in
`Raw`; a turn cut off at its token limit or stopped by a filter fails the call with
`agent.ErrOutputTruncated` or `agent.ErrOutputFiltered`.
`agent.NewStream` wraps a channel the caller fills itself, which suits a response buffered up
front. `model/modeltest.Run` checks an HTTP adapter against this
contract, including the truncation case, and `modeltest.CheckFinish` checks the `Finish` it sends.
The HTTP kit the first-party adapters share (error classification, SSE framing, the response cap,
`Retry-After` parsing, the tool-result codec) is the `model/provider` package, which a third-party
adapter can use too.

**Reference adapters.** `model/anthropic`, `model/openai`, and `model/gemini` each provide
`New(apiKey, opts...)` returning a `*Model` that satisfies the port, with options like
`WithModel`, `WithMaxTokens`, and `WithPromptCache` (Anthropic).

## `Store`: the crash-safe substrate

<!-- docsnip: api agent -->
```go
type Store interface {
	// Insert stores data under (runID, name) if no entry has that name, and returns the stored
	// entry (the caller's, or the one already there) and whether this call stored it.
	Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error)
	// Get returns runID's entry named name, if there is one.
	Get(ctx context.Context, runID, name string) (Entry, bool, error)
	// Load yields runID's entries whose Seq is greater than after (-1 for all of them), in
	// ascending Seq.
	Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error]
}

type Entry struct {
	Seq  int64 // opaque; strictly increasing in commit order within the run
	Name string
	Data []byte
}
```

A `Store` is a dumb, append-only set of named entries per run. It stores bytes and knows nothing
of what they mean. Everything with meaning lives in the `agent.Journal` above it
(`agent.NewJournal(store)`): the record encoding and its salt, named-step memoization, the journal
format header, the attempt claims that keep side effects at most once, and recording a step's
outcome after its caller's context is cancelled. So a new backend implements three methods and
gets every engine guarantee without reimplementing any of them.

**Requirements.** The Journal relies on these, and `agent/storetest` checks each one:

- **A1, unique names, linearizable insert.** At most one entry per `(runID, name)`. Concurrent
  Inserts of one name, from any number of processes, have exactly one winner, and every caller and
  every later reader sees the winner's bytes. Which entry `(runID, name)` names does not depend on
  the context: a tenant carried in the context and mixed into the keys breaks the journal, which
  remembers runs by run ID; put the tenant in the run ID instead (`RunFilter.Prefix`).
- **A2, commit-ordered, prefix-closed visibility.** `Seq` is strictly increasing in commit order
  within a run, and every read returns a prefix of the run's final order: no entry ever becomes
  visible with a lower `Seq` than one already visible. Gaps are allowed; nothing reads `Seq` as a
  count.
- **A3, durable before return.** Insert reports `inserted` only after the commit. On an error the
  entry is either absent or complete, and the same bytes may be inserted again.
- **A4, read-your-writes, monotone visibility.** Once visible, an entry stays visible with the same
  bytes and position.
- **A5, byte fidelity.** Data comes back exactly as inserted.
- **A6, immutable.** No update or delete; only a redaction may replace an entry's bytes with a
  tombstone, in a run that is over.
- **A7, context.** Every method honors `ctx`.
- **A8, iterator hygiene.** Breaking out of `Load` releases everything it holds, and a store holds
  no connection, transaction or lock across a `yield`, so a caller may write inside its loop.

**Optional capabilities.** A store may also implement `Lister` (`Runs(ctx, RunFilter)`, which a
recovery supervisor needs; a SQL store evaluates the filter in its query) and `Leaser` (run leases
that coordinate drivers). `agent.Capability[T](store)` finds them, looking through wrappers that
implement `Unwrap() Store`. Only a wrapper that passes run IDs and names through unchanged may
implement `Unwrap`; one that rewrites keys (a tenant prefix, say) implements each capability
itself. `storetest.CheckWrapper(t, wrap, ctxA, ctxB)` checks this, and that the wrapper's keys do
not depend on the context; give it at least two contexts that differ in the values your wrapper
reads from a context. A wrapper whose `Do` and `History` come from `MemStore` or a SQL store (it
embeds one, directly or through another wrapper, or embeds an `agent.Durable`) while its `Insert`,
`Get` or `Load` comes from elsewhere would have every write bypass those methods: the engine
refuses it with `ErrConfig`, so use it through `agent.NewJournal(wrapper)`.

**Reference adapters.** `agent.NewMemStore()` is the in-memory implementation for tests and local
dev. `store/sqlite.Open(path)` (one machine; three connection pools: a writer, readers, and a lease
connection) and `store/postgres.Open(ctx, dsn)` (any number of nodes) are the persistent backends;
each also has `New(ctx, db)` for a `*sql.DB` you opened, and `WithTablePrefix` (tables are
`bide_steps`, `bide_leases` and `bide_schema_version` by default). All three implement `Lister` and
`Leaser`. `store/postgres` runs every write in a transaction at read committed that it sets
itself, so a deployment whose `default_transaction_isolation` is repeatable read or serializable
does not change how it records steps or leases.

**Transition.** The engine's functions still take the `Durable` interface (`Do` and `History`),
which `*agent.Journal` implements; `MemStore` and the SQL stores also implement it, through a
Journal over themselves, so existing code keeps working. `Durable` and those store methods are
removed by the 1.0 rewrite.

### Implement your own store

Here is a small in-memory implementation (the same shape as `MemStore`):

```go
package mystore

import (
	"bytes"
	"context"
	"iter"
	"sync"

	"github.com/bide-ai/bide/agent"
)

type Store struct {
	mu   sync.Mutex
	runs map[string][]agent.Entry // runID -> entries in Seq order
}

func New() *Store { return &Store{runs: map[string][]agent.Entry{}} }

var _ agent.Store = (*Store)(nil) // port/adapter contract

func (s *Store) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return agent.Entry{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.runs[runID] {
		if e.Name == name { // A1: the first insert of a name wins
			e.Data = bytes.Clone(e.Data)
			return e, false, nil
		}
	}
	e := agent.Entry{Seq: int64(len(s.runs[runID])), Name: name, Data: bytes.Clone(data)}
	s.runs[runID] = append(s.runs[runID], e)
	e.Data = bytes.Clone(e.Data)
	return e, true, nil
}

func (s *Store) Get(ctx context.Context, runID, name string) (agent.Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return agent.Entry{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.runs[runID] {
		if e.Name == name {
			e.Data = bytes.Clone(e.Data)
			return e, true, nil
		}
	}
	return agent.Entry{}, false, nil
}

func (s *Store) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	return func(yield func(agent.Entry, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(agent.Entry{}, err)
			return
		}
		s.mu.Lock()
		snap := s.runs[runID] // entries are only appended, so the snapshot stays valid
		s.mu.Unlock()         // A8: no lock held across a yield
		for _, e := range snap {
			if e.Seq > after {
				e.Data = bytes.Clone(e.Data)
				if !yield(e, nil) {
					return
				}
			}
		}
	}
}
```

Use it through a Journal, which goes wherever a store goes (`agent.New(model, j)`,
`agent.Step(ctx, j, ...)`, `agent.Recover(ctx, j, ...)`):

<!-- docsnip: setup mystore struct{ New func() agent.Store }; returns error -->
```go
j, err := agent.NewJournal(mystore.New())
if err != nil {
	return err
}
_ = j
```

For a SQL backend, let a primary key on `(run_id, name)` enforce A1 (`INSERT ... ON CONFLICT DO
NOTHING`, then read back the stored row), assign `seq` in the insert under a per-run lock so it
follows commit order (A2), read `Load` in pages with no connection held between them (A8), and
store `data` as bytes, never as JSON or text (A5).

**Check it with the conformance suite.** `agent/storetest` checks every requirement above
(including 64 goroutines racing one name through three handles), the journal format header, and
the record-fidelity cases (records whose encoding is easy to get wrong: HTML-significant
characters, U+2028, NUL, invalid UTF-8, unusual number forms, key order). `open` may be called
several times; every handle it returns must reach the same data:

```go
package mystore_test

import (
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/storetest"
)

func TestMyStore(t *testing.T) {
	s := New()
	storetest.Run(t, func(*testing.T) agent.Store { return s })
}

func New() agent.Store { return agent.NewMemStore() } // your store here
```

`MemStore`, `store/sqlite`, and `store/postgres` all run it. (`agent/durabletest` is its former
name, kept for the transition.)

## `Tool`: an action the agent can take

<!-- docsnip: api agent -->
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
the [MCP guide](../guides/mcp.md)). Its optional `Approval` field upgrades the approval gate to a signed
m-of-n policy; approver signatures are checked through the `ApproverVerifier` hook (`Alg()`, `Verify` and `KeyIDs`), which the
`audit` package's Ed25519, ML-DSA, and hybrid verifiers implement; a decision counts only under the scheme
journaled with it (`Decision.Alg`, recorded as `Record.ApproverAlg`), which must be its approver key's. Its `KeyIDs` method names the keys
behind a verifier, derived from the public key's bytes, and the gate refuses a policy two of whose
approvers share one (see [approval](../guides/hitl-approval.md#key-identity)).

## `Compensator`: how a tool undoes its side effect

<!-- docsnip: api agent -->
```go
type Compensator interface {
	// Compensate undoes a completed call. args are the arguments the tool accepted (after tool
	// middleware; see CompensatedFunc); result is what Call returned. Must be idempotent: on a
	// crash mid-rollback it may re-run.
	Compensate(ctx context.Context, args, result json.RawMessage) error
}
```

An optional interface a `Tool` implements to declare how to roll back its write. In a saga
run (`RunSaga`), if a step fails after earlier writes succeeded, the completed compensatable
writes are rolled back in reverse order, automatically and recursively through sub-agent
trees. `agent.CompensatedFunc[In, Out](name, desc, safety, do, undo)` builds a typed tool
that declares both its forward action and its compensator.

## `Retriever`: bring-your-own RAG

<!-- docsnip: api agent -->
```go
type Retriever interface {
	Retrieve(ctx context.Context, query string, k int) ([]Doc, error)
}
```

The bring-your-own-RAG port: given a query, return the top-k relevant `Doc` values from your
store (pgvector, Pinecone, a file index, anything). Bide ships no vector store and no
embedder; you implement `Retrieve` against infrastructure you already run and wire it in with
`agent.RetrievalTool(r, k)` (agentic: the model searches on demand) or
`agent.WithRetrieval(r, k)` (classic: the top-k for the run's user message, sent on every model
call of the run as a user message just before that message). Both journal what was retrieved, so a
resumed run sees the same documents, and both call `Retrieve` concurrently, so it must be safe for
concurrent use. See [RAG and memory](../guides/rag-memory.md).

## `Anchor`: out-of-band anchoring (`audit`)

<!-- docsnip: api audit -->
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
version is for tests and local dev. See the [audit guide](../guides/audit.md).

## `EventStore`: durable event-trail persistence (`audit`)

<!-- docsnip: api audit -->
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
journal projection (`agent.ReplayEvents`, see [debugging](../guides/debugging.md)), not the live
stream, so re-mirroring after a crash appends the same leaves at the same positions
(idempotent). `audit.PersistJournal` drives that mirroring; `audit.LoadEventLog` rebuilds an
`EventLog` from the store for `Root` / STH / proofs even after the journal is deleted. Each leaf
holds its event's random salt, which a proof discloses from the stored bytes, so store leaves
verbatim (see [audit](../guides/audit.md#committing-the-event-stream-not-just-the-journal)).

**Reference adapter.** `audit.NewMemEventStore()` is the in-memory implementation; it
enforces the same append-only, idempotent, contiguous contract a real backend would enforce
with a unique constraint and insert-only permissions.
