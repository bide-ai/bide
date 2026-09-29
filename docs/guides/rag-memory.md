# RAG & memory: bring your own

**Decision: Bide does not ship a vector store, an embedder, or a memory backend.**
It provides the *seam* (a `Retriever` port and thin glue) and you plug in the store you
already run. This is a deliberate scope boundary, not a gap.

## Why

- **It's orthogonal to the moat.** Our differentiator is durable, side-effect-safe resume.
  Retrieval is a separate concern; owning it wouldn't strengthen the moat, it would dilute
  focus and pull us into a fast-churning, commoditized space (pgvector / Pinecone / Weaviate /
  the embedded-DB-of-the-month).
- **It would break the dependency story.** We just made the core zero-dep and split adapters
  into their own modules so consumers don't inherit infra they don't use. Bundling a vector
  store + embedding client (and their transitive trees) into the core would blow a hole
  straight through that.
- **Teams already have a store.** Most run pgvector / Pinecone / their own index. Forcing our
  memory abstraction on them is friction; respecting their infrastructure is a feature. The
  positioning: *"we don't ship a vector DB you'll outgrow and fight; we give you a clean
  retrieval seam that works with the store you already run."*

## The seam (in core, zero-dep)

```go
type Doc struct { ID string; Text string; Score float64; Metadata map[string]any }

type Retriever interface {
    Retrieve(ctx context.Context, query string, k int) ([]Doc, error)
}

func RetrievalTool(r Retriever, k int) Tool        // agentic: the model searches on demand
func WithRetrieval(r Retriever, k int) Middleware  // classic: top-k auto-injected each user turn
```

Implement `Retriever` against your store (~20 lines), then wire it in one of two ways:

- **Agentic RAG**: `agent.New(model, store, agent.RetrievalTool(myStore, 5))`. The model
  decides when to search and with what query; results come back as a tool result.
- **Classic RAG**: `a.Use(agent.WithRetrieval(myStore, 5))`. The middleware retrieves top-k
  for the run's user message and adds them as a system message after the agent's system
  prompt, on every model call of the run, so the call that follows a tool result still has
  the context. A retrieval error aborts the call; return `(nil, nil)` from your `Retriever` if
  you prefer to degrade to no context.

Both helpers panic if `k` is below 1, and both cut a result longer than `k` to its first `k`
documents, in the order your `Retriever` ranked them. Order ties deterministically in your
`Retriever` (by ID, say) if two fresh runs of the same question should see the same documents.
A NaN or infinite `Score` (cosine similarity against a zero vector is 0/0) has no JSON encoding,
so the helpers report it as 0.

## Resume, the journal, and concurrency

- **A resumed run sees the documents the original saw.** `RetrievalTool` results are tool
  results, journaled like any other, so a replayed turn reads the recorded documents.
  `WithRetrieval` records its retrieval as a read-only step of the run (`@retrieval/0`, holding
  the query and the documents): the run retrieves once, and every later model call of the run,
  including one made after a crash and resume, is given the recorded documents without asking
  your store again. Outside an agent run there is no journal, so it retrieves on every call.
- **Retrieved text is durable content.** Either way the documents are stored in the journal in
  full, so keep `k` and document size bounded (trim `Text` in your `Retriever`). They are stored
  as written, like any tool result: the tool-error redaction does not apply to them, so do not
  return text you would not keep in the journal. See the [security model](security-model.md)
  for what the journal does and does not protect.
- **`Retrieve` must be safe for concurrent use.** Parallel tool calls in one turn, concurrent runs
  on one `Agent`, and sub-agents sharing a `Retriever` all call it at once.
- **Memory writes are side effects.** Bide ships no write path. A tool that writes to your
  memory store is a tool like any other: leave its `Safety` unset so it runs at most once and a
  crash mid-write halts for confirmation, or declare it `Idempotent` (with an
  `IdempotencyKey`) only when a repeat write is a no-op downstream, such as an upsert by a stable
  id. Do not mark it `ReadOnly`: a read-only call with no recorded result is re-run on resume.

## Memory, in layers

- **Conversational memory** is already built in: `Session` / `Session.Send` carry the Q&A
  transcript across turns, durably (see the sessions docs). No retriever needed.
- **Dynamic context** (current time, tenant, retrieved summaries) goes through
  `WithSystemPromptFunc(func(ctx) string)`.
- **Semantic / long-term memory** is the `Retriever` seam above, backed by your store.

## If demand appears

Concrete store adapters (e.g. a pgvector `Retriever`) would ship as **separate modules**
(like `store/postgres` and the other adapters), never in the core, preserving the zero-dep
core. Until then, the seam + your ~20-line `Retriever` is the whole story.
