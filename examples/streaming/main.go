// Command streaming shows Agent.Stream: range the lifecycle Events to render progress
// (token deltas, turn boundaries, tool start/finish) while the durable loop runs
// underneath, then call Final for the terminal answer. Run is literally Stream(...).Final().
//
//	OPENROUTER_API_KEY=sk-... go run ./examples/streaming
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

type WeatherArgs struct {
	City string `json:"city" desc:"city name"`
}
type Weather struct {
	TempF int    `json:"temp_f"`
	Sky   string `json:"sky"`
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

	weather := agent.Func("get_weather", "Get the current weather for a city",
		agent.Safety{ReadOnly: true},
		func(_ context.Context, in WeatherArgs) (Weather, error) {
			return Weather{TempF: 68, Sky: "sunny"}, nil
		})

	j, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		log.Fatal(err)
	}
	a, err := agent.New(model, j, agent.WithTools(weather))
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stream := a.Stream(ctx, "stream-1",
		"What's the weather in San Francisco? Use the get_weather tool, then answer in one sentence.")

	// Range the semantic lifecycle events. Text deltas arrive inside ModelEvent as the
	// model generates; tool start/finish bracket each call.
	for ev := range stream.Events() {
		switch e := ev.(type) {
		case agent.TurnStarted:
			fmt.Printf("\n[turn %d started]\n", e.Seq)
		case agent.ModelEvent:
			if d, ok := e.Event.(agent.TextDelta); ok {
				fmt.Print(d.Text) // token-by-token feed
			}
		case agent.ToolStarted:
			fmt.Printf("\n[tool start] %s args=%s\n", e.Name, string(e.Args))
		case agent.ToolCompleted:
			fmt.Printf("[tool done]  %s result=%s\n", e.Name, string(e.Result))
		}
	}

	// Final drains anything left and returns the terminal answer (or error), exactly as Run would.
	final, err := stream.Final()
	if err != nil {
		log.Fatalf("stream: %v", err)
	}
	fmt.Println("\n\n=== final answer ===")
	fmt.Println(final.Text())
}
