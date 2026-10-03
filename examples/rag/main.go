// Command rag shows bring-your-own retrieval: a trivial in-memory Retriever wired two
// ways. WithRetrieval is classic RAG (the top-k documents for the user's message are added,
// as a user message of JSON-quoted documents, to every model call of the run, and journaled so
// a resumed run sees the same ones); RetrievalTool is agentic RAG (the model decides when to
// search and with what query).
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

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/model/openai"
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

	// Classic RAG: the WithRetrieval option retrieves the top-2 docs once per run, as a journaled
	// step, and adds them as context to each model call.
	j, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		log.Fatal(err)
	}
	classic, err := agent.New(model, j, agent.WithRetrieval(kb, 2))
	if err != nil {
		log.Fatal(err)
	}
	res, err := classic.Run(ctx, "rag-classic", agent.UserText("What is the warranty on the widget? Answer in one sentence."))
	if err != nil {
		log.Fatalf("classic rag: %v", err)
	}
	out := res.Message
	fmt.Println("=== classic RAG (auto-injected context) ===")
	fmt.Println(out.Text())

	// Agentic RAG: expose retrieval as a tool the model calls on demand. An agent searching
	// several stores gives each tool its own name.
	search := agent.RetrievalTool("search_support_kb",
		"Search the support knowledge base: shipping, warranty, returns, and hours.", kb, 2)
	agentic, err := agent.New(model, j, agent.WithTools(search))
	if err != nil {
		log.Fatal(err)
	}
	res2, err := agentic.Run(ctx, "rag-agentic", agent.UserText("Use the search_support_kb tool to find the return policy, then answer in one sentence."))
	if err != nil {
		log.Fatalf("agentic rag: %v", err)
	}
	out2 := res2.Message
	fmt.Println("\n=== agentic RAG (model-driven search) ===")
	fmt.Println(out2.Text())
}
