# RAG & memory: bring your own

**Decision: go-agents does not ship a vector store, an embedder, or a memory backend.**
It provides the *seam* — a `Retriever` port and thin glue — and you plug in the store you
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
  positioning: *"we don't ship a vector DB you'll outgrow and fight — we give you a clean
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

- **Agentic RAG** — `agent.New(model, store, agent.RetrievalTool(myStore, 5))`. The model
  decides when to search and with what query; results come back as a tool result.
- **Classic RAG** — `a.Use(agent.WithRetrieval(myStore, 5))`. On each fresh user turn the
  middleware retrieves top-k for the user message and prepends them as a system message; it
  does not retrieve on mid-loop tool-result turns. A retrieval error aborts the call — return
  `(nil, nil)` from your `Retriever` if you prefer to degrade to no context.

## Memory, in layers

- **Conversational memory** is already built in: `Session` / `Session.Send` carry the Q&A
  transcript across turns, durably (see the sessions docs). No retriever needed.
- **Dynamic context** (current time, tenant, retrieved summaries) goes through
  `WithSystemPromptFunc(func(ctx) string)`.
- **Semantic / long-term memory** is the `Retriever` seam above, backed by your store.

## If demand appears

Concrete store adapters (e.g. a pgvector `Retriever`) would ship as **separate modules**
(like `store/postgres` and the other adapters), never in the core — preserving the zero-dep
core. Until then, the seam + your ~20-line `Retriever` is the whole story.
