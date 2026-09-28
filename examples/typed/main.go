// Command typed shows RunTyped[T]: the agent does its work with real tools, then
// returns a strongly-typed Go struct instead of free-form text. RunTyped injects a
// synthetic final_answer tool whose JSON schema is derived from T and decodes the
// result from the journaled call, so it is resume-safe and provider-agnostic.
//
//	OPENROUTER_API_KEY=sk-... go run ./examples/typed
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/blackwell-systems/bide/agent"
	"github.com/blackwell-systems/bide/model/openai"
)

type CityArgs struct {
	City string `json:"city" desc:"city name"`
}

// Population is the raw fact the tool returns.
type Population struct {
	Count int `json:"count"`
}

// CityReport is the typed shape we want the whole run to produce.
type CityReport struct {
	City       string `json:"city" desc:"the city the report is about"`
	Population int    `json:"population" desc:"the population figure"`
	Summary    string `json:"summary" desc:"a one-sentence summary"`
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

	lookup := agent.Func("get_population", "Get the population of a city",
		agent.Safety{ReadOnly: true},
		func(_ context.Context, in CityArgs) (Population, error) {
			log.Printf("[tool] get_population(%q) called", in.City)
			return Population{Count: 8_336_000}, nil
		})

	a := agent.New(model, agent.NewMemStore(), lookup)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// RunTyped drives the loop to completion and decodes the answer into CityReport.
	// (On an OpenAI-compatible provider with strict structured outputs, prefer
	// agent.RunTypedNative[CityReport] instead: the schema is enforced provider-side
	// with no final_answer tool round-trip.)
	report, err := agent.RunTyped[CityReport](ctx, a, "typed-1",
		"Look up the population of New York City with the get_population tool, then produce the report.")
	if err != nil {
		log.Fatalf("run typed: %v", err)
	}

	fmt.Println("\n=== typed result ===")
	fmt.Printf("City:       %s\n", report.City)
	fmt.Printf("Population: %d\n", report.Population)
	fmt.Printf("Summary:    %s\n", report.Summary)
}
