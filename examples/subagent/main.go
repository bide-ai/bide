// Command subagent shows a parent agent delegating to a SubAgent exposed as a tool.
// The sub-agent shares the parent's Durable store, so the whole tree journals under one
// unified log (parentRunID/toolUseID): a crash anywhere in the tree resumes precisely.
//
//	OPENROUTER_API_KEY=sk-... go run ./examples/subagent
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	agent "github.com/blackwell-systems/bide"
	"github.com/blackwell-systems/bide/model/openai"
)

type CityArgs struct {
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

	// One store, shared by parent and sub-agent, so the tree journals under one log.
	store := agent.NewMemStore()

	// The specialist sub-agent: it owns the weather tool and answers weather questions.
	weather := agent.Func("get_weather", "Get the current weather for a city",
		agent.Safety{ReadOnly: true},
		func(_ context.Context, in CityArgs) (Weather, error) {
			log.Printf("[tool] get_weather(%q) called", in.City)
			return Weather{TempF: 68, Sky: "sunny"}, nil
		})
	weatherAgent := agent.New(model, store, weather).
		WithSystemPrompt("You are a weather specialist. Use get_weather, then answer briefly.")

	// Expose the sub-agent to the parent as a tool it can delegate to.
	weatherTool := agent.SubAgent("weather_agent",
		"Delegate any weather question to the weather specialist sub-agent.", weatherAgent)

	parent := agent.New(model, store, weatherTool).
		WithSystemPrompt("You are a trip planner. Delegate weather questions to the weather_agent tool.")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	out, err := parent.Run(ctx, "trip-1",
		"I'm visiting San Francisco. Ask the weather specialist what to expect, then suggest what to pack in one sentence.")
	if err != nil {
		log.Fatalf("run: %v", err)
	}

	fmt.Println("\n=== planner answer ===")
	fmt.Println(out.Text())
}
