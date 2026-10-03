// Command session shows a durable multi-turn conversation: each Session.Send is one
// full agent run seeded with the transcript so far, so the agent remembers earlier
// turns. The transcript is journaled turn-by-turn, so a Session reopened from the same
// store resumes the conversation. This program also rebuilds the transcript at the end.
//
//	OPENROUTER_API_KEY=sk-... go run ./examples/session
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/model/openai"
)

func main() {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		log.Fatal("set OPENROUTER_API_KEY")
	}

	model := openai.New(key,
		openai.WithBaseURL("https://openrouter.ai/api/v1"),
		openai.WithModel("openai/gpt-4o-mini"),
		openai.WithMaxTokens(256),
	)

	// One store backs the session; the same id reopened later rebuilds this transcript.
	store, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		log.Fatal(err)
	}
	a, err := agent.New(
		model,
		store,
		agent.WithSystemPrompt("You are a concise assistant. Answer in one short sentence."),
	)
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	sess, err := a.Session(ctx, "chat-1")
	if err != nil {
		log.Fatalf("open session: %v", err)
	}

	for _, turn := range []string{
		"My name is Ada and I am thinking about the number 7.",
		"What is my name?",
		"Double the number I mentioned. What do you get?",
	} {
		fmt.Printf("\nuser> %s\n", turn)
		answer, err := sess.Send(ctx, turn)
		if err != nil {
			log.Fatalf("send: %v", err)
		}
		fmt.Printf("agent> %s\n", answer.Text())
	}

	// Reopen the same id from the same store: the transcript is rebuilt from the journal,
	// demonstrating that conversational memory survives a restart.
	reopened, err := a.Session(ctx, "chat-1")
	if err != nil {
		log.Fatalf("reopen session: %v", err)
	}
	fmt.Printf("\n=== rebuilt transcript (%d turns) ===\n", reopened.Turns())
	for _, m := range reopened.History() {
		fmt.Printf("%-9s %s\n", m.Role+":", m.Text())
	}
}
