// Command eval shows how to use the statistical evaluation harness. It builds an agent, defines
// labeled cases and metrics, runs each case several times, and prints a pass-rate report.
//
// The model here is a stub sentiment classifier that is deliberately flaky (it gets one in five
// calls wrong) so the report shows a rate below 100%, which is the point: the model is stochastic,
// so eval reports a distribution, not a verdict. To evaluate a real agent, replace the stub with
// anthropic.New / openai.New / gemini.New and keep everything else the same.
//
// Remember the boundary (see docs/testing/testing.md): eval measures the model statistically; the provable
// guarantees (governed convergence, cryptographic audit) live in the govern and audit packages. A
// pass rate is a signal, not a proof.
package main

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/eval"
)

// classifier is a stub model: it labels input sentiment, but flips its answer on every fifth call
// to simulate a real model's variance. Swap it for a real provider adapter to evaluate for real.
type classifier struct{ calls int64 }

func (c *classifier) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	in := ""
	for _, m := range req.Messages {
		if m.Role == agent.RoleUser {
			in = strings.ToLower(m.Text())
		}
	}
	label := "negative"
	if strings.Contains(in, "love") || strings.Contains(in, "great") || strings.Contains(in, "excellent") {
		label = "positive"
	}
	if atomic.AddInt64(&c.calls, 1)%5 == 0 { // flake
		if label == "positive" {
			label = "negative"
		} else {
			label = "positive"
		}
	}
	ch := make(chan agent.Emit, 2)
	ch <- agent.Emit{Event: agent.TextDelta{Text: label}}
	ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	close(ch)
	return agent.NewStream(ch), nil
}

func main() {
	ctx := context.Background()

	// 1) The agent under test. For a real evaluation, swap classifier{} for a provider adapter.
	store := agent.NewMemStore()
	a := agent.New(&classifier{}, store).
		WithSystemPrompt("Classify the sentiment of the user's message as 'positive' or 'negative'.")

	// 2) Labeled cases. Want carries the expected label a metric checks.
	cases := []eval.Case{
		{Name: "clearly_positive", Input: "I love this product, it is great", Want: "positive"},
		{Name: "clearly_negative", Input: "I hate this, a terrible experience", Want: "negative"},
	}

	// 3) Metrics: a rule-based label check, plus that the run did not error.
	correct := eval.Custom("correct_label", func(_ context.Context, c eval.Case, out eval.RunOutput) bool {
		want, _ := c.Want.(string)
		return strings.Contains(strings.ToLower(out.Final.Text()), want)
	})
	// MaxSteps reads the trajectory from the journal: a classifier should answer in one turn.
	metrics := []eval.Metric{eval.NoError(), correct, eval.MaxSteps(1)}

	// 4) Run each case 5 times (sampling the stochastic model) and print the pass-rate report.
	run, err := eval.AgentRunner(a, store, "sentiment")
	if err != nil {
		panic(err)
	}
	rep, err := eval.Run(ctx, run, cases, metrics, eval.Options{Runs: 5, Concurrency: 4})
	if err != nil {
		panic(err)
	}
	fmt.Print(rep.String())
	fmt.Printf("\noverall correct_label rate: %.0f%% (a distribution over runs, not a guarantee)\n",
		rep.Overall["correct_label"].Rate*100)
}
