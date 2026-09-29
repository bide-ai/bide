package agent

import (
	"context"
	"fmt"
	"strings"
)

// Doc is a document returned by a Retriever: its text plus optional id, similarity score,
// and metadata. This is the neutral shape the retrieval helpers speak; your Retriever
// maps your store's results onto it.
type Doc struct {
	ID       string         `json:"id,omitempty"`
	Text     string         `json:"text"`
	Score    float64        `json:"score,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Retriever is the bring-your-own-RAG port: given a query, return the top-k relevant
// documents from YOUR store (pgvector, Pinecone, a file index — anything). Bide
// ships no vector store and no embedder; you implement Retrieve against infrastructure you
// already run, and wire it in with RetrievalTool (agentic — the model searches on demand)
// or WithRetrieval (classic — top-k auto-injected as context on each user turn). Sessions
// already give conversational memory; this is the seam for semantic / long-term memory.
type Retriever interface {
	Retrieve(ctx context.Context, query string, k int) ([]Doc, error)
}

// RetrievalTool exposes a Retriever as a tool the model can call to search on demand
// (agentic RAG): the model decides when to retrieve and with what query. It returns the
// top-k documents: a Retriever that returns more is cut to its first k. Read-only (retry-safe).
// It panics if k is below 1.
func RetrievalTool(r Retriever, k int) Tool {
	checkK("RetrievalTool", k)
	type args struct {
		Query string `json:"query" desc:"what to search the knowledge base for"`
	}
	return Func("retrieve", "Search the knowledge base and return the most relevant documents.",
		Safety{ReadOnly: true},
		func(ctx context.Context, in args) ([]Doc, error) {
			docs, err := r.Retrieve(ctx, in.Query, k)
			return topK(docs, k), err
		})
}

// WithRetrieval is model middleware that auto-injects retrieved context (classic RAG):
// when the model is responding to a fresh user turn, it retrieves the top-k documents for
// that user message and prepends them as a system message. It does NOT retrieve on
// tool-result turns (mid-loop). A retrieval error aborts the model call — have your
// Retriever return (nil, nil) instead of an error if you prefer to degrade to no context.
// A Retriever that returns more than k documents is cut to its first k. It panics if k is
// below 1.
func WithRetrieval(r Retriever, k int) Middleware {
	checkK("WithRetrieval", k)
	return func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, req Request) (Message, Usage, error) {
			if q := lastUserQuery(req.Messages); q != "" {
				docs, err := r.Retrieve(ctx, q, k)
				if err != nil {
					return Message{}, Usage{}, fmt.Errorf("retrieval: %w", err)
				}
				docs = topK(docs, k)
				if block := formatDocs(docs); block != "" {
					req.Messages = append([]Message{SystemText(block)}, req.Messages...)
				}
			}
			return next(ctx, req)
		}
	}
}

// checkK panics unless k is at least 1: k is how many documents to return, and a k below 1
// asks for none, which a Retriever would serve inconsistently (one store returns nothing,
// another ignores the limit), so it is a construction-time programmer error.
func checkK(fn string, k int) {
	if k < 1 {
		panic(fmt.Sprintf("agent: %s requires k >= 1, got %d", fn, k))
	}
}

// topK returns the first k of docs, the top k in the order the Retriever ranked them.
func topK(docs []Doc, k int) []Doc {
	if len(docs) > k {
		return docs[:k]
	}
	return docs
}

// lastUserQuery returns the text of the final message if it is a user turn (the point at
// which retrieval is relevant), else "".
func lastUserQuery(msgs []Message) string {
	if n := len(msgs); n > 0 && msgs[n-1].Role == RoleUser {
		return msgs[n-1].Text()
	}
	return ""
}

// formatDocs renders retrieved docs as a context block for a system message.
func formatDocs(docs []Doc) string {
	if len(docs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Relevant context:\n")
	for i, d := range docs {
		fmt.Fprintf(&b, "[%d] %s\n", i+1, d.Text)
	}
	return b.String()
}
