// Command rag shows bring-your-own retrieval: a trivial in-memory Retriever wired two
// ways. WithRetrieval is classic RAG (top-k auto-injected as context on each user turn);
// RetrievalTool is agentic RAG (the model decides when to search and with what query).
// Bide ships no vector store or embedder: you implement Retrieve against your own
// infrastructure. This example uses naive substring matching to stay offline-runnable.
//
//	OPENROUTER_API_KEY=sk-... go run ./examples/rag
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/blackwell-systems/bide/agent"
	"github.com/blackwell-systems/bide/model/openai"
)

// memRetriever is a stand-in knowledge base. A real Retriever would query pgvector,
// Pinecone, or a file index and map results onto agent.Doc.
type memRetriever struct{ docs []agent.Doc }

func (r memRetriever) Retrieve(_ context.Context, query string, k int) ([]agent.Doc, error) {
	q := strings.ToLower(query)
	var hits []agent.Doc
	for _, d := range r.docs {
		for _, term := range strings.Fields(q) {
			if len(term) > 3 && strings.Contains(strings.ToLower(d.Text), term) {
				hits = append(hits, d)
				break
			}
		}
		if len(hits) >= k {
			break
		}
	}
	return hits, nil
}

func main() {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		log.Fatal("set OPENROUTER_API_KEY")
	}

	model := openai.New(key,
		openai.WithBaseURL("https://openrouter.ai/api/v1"),
		openai.WithModel("openai/gpt-4o-mini"),
		openai.WithMaxTokens(512),
	)

	kb := memRetriever{docs: []agent.Doc{
		{ID: "1", Text: "The Acme Widget ships in 3 business days and includes a 2-year warranty."},
		{ID: "2", Text: "Returns are accepted within 30 days of delivery for a full refund."},
		{ID: "3", Text: "The support hotline is open weekdays 9am to 5pm Pacific time."},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Classic RAG: WithRetrieval middleware injects the top-2 docs as context each user turn.
	classic := agent.New(model, agent.NewMemStore()).Use(agent.WithRetrieval(kb, 2))
	out, err := classic.Run(ctx, "rag-classic",
		"What is the warranty on the widget? Answer in one sentence.")
	if err != nil {
		log.Fatalf("classic rag: %v", err)
	}
	fmt.Println("=== classic RAG (auto-injected context) ===")
	fmt.Println(out.Text())

	// Agentic RAG: expose retrieval as a tool the model calls on demand.
	agentic := agent.New(model, agent.NewMemStore(), agent.RetrievalTool(kb, 2))
	out2, err := agentic.Run(ctx, "rag-agentic",
		"Use the retrieve tool to find the return policy, then answer in one sentence.")
	if err != nil {
		log.Fatalf("agentic rag: %v", err)
	}
	fmt.Println("\n=== agentic RAG (model-driven search) ===")
	fmt.Println(out2.Text())
}
