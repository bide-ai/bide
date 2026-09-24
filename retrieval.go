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
// documents from YOUR store (pgvector, Pinecone, a file index — anything). go-agents
// ships no vector store and no embedder; you implement Retrieve against infrastructure you
// already run, and wire it in with RetrievalTool (agentic — the model searches on demand)
// or WithRetrieval (classic — top-k auto-injected as context on each user turn). Sessions
// already give conversational memory; this is the seam for semantic / long-term memory.
type Retriever interface {
	Retrieve(ctx context.Context, query string, k int) ([]Doc, error)
}

// RetrievalTool exposes a Retriever as a tool the model can call to search on demand
// (agentic RAG): the model decides when to retrieve and with what query. It returns the
// top-k documents. Read-only (retry-safe).
func RetrievalTool(r Retriever, k int) Tool {
	type args struct {
		Query string `json:"query" desc:"what to search the knowledge base for"`
	}
	return Func("retrieve", "Search the knowledge base and return the most relevant documents.",
		Safety{ReadOnly: true},
		func(ctx context.Context, in args) ([]Doc, error) {
			return r.Retrieve(ctx, in.Query, k)
		})
}

// WithRetrieval is model middleware that auto-injects retrieved context (classic RAG):
// when the model is responding to a fresh user turn, it retrieves the top-k documents for
// that user message and prepends them as a system message. It does NOT retrieve on
// tool-result turns (mid-loop). A retrieval error aborts the model call — have your
// Retriever return (nil, nil) instead of an error if you prefer to degrade to no context.
func WithRetrieval(r Retriever, k int) Middleware {
	return func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, req Request) (Message, Usage, error) {
			if q := lastUserQuery(req.Messages); q != "" {
				docs, err := r.Retrieve(ctx, q, k)
				if err != nil {
					return Message{}, Usage{}, fmt.Errorf("retrieval: %w", err)
				}
				if block := formatDocs(docs); block != "" {
					req.Messages = append([]Message{SystemText(block)}, req.Messages...)
				}
			}
			return next(ctx, req)
		}
	}
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
