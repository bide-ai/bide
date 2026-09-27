// Command parallel shows agent.Parallel: a durable, auditable fan-out/fan-in. Several
// independent checks run concurrently, each as a journaled named step, so a resumed run
// returns a completed check's recorded result without re-running it. All tasks run even
// if some fail; the returned error joins every task's error.
//
// Offline, no LLM or network: the tasks stand in for real checks (sanctions, credit, fraud).
//
//	go run ./examples/parallel
package main

import (
	"context"
	"fmt"
	"log"

	agent "github.com/blackwell-systems/bide"
)

// Check is the result of one independent screening step.
type Check struct {
	Name   string
	Passed bool
	Detail string
}

func main() {
	ctx := context.Background()
	store := agent.NewMemStore()

	// Each Task has a unique Name (its durable memoization key within the run) and a Fn
	// returning a T. Here T is Check. maxConcurrency 0 means one goroutine per task.
	results, err := agent.Parallel[Check](ctx, store, "screen-1", 0,
		agent.Task[Check]{Name: "sanctions", Fn: func(_ context.Context) (Check, error) {
			return Check{Name: "sanctions", Passed: true, Detail: "no OFAC match"}, nil
		}},
		agent.Task[Check]{Name: "credit", Fn: func(_ context.Context) (Check, error) {
			return Check{Name: "credit", Passed: true, Detail: "score 780"}, nil
		}},
		agent.Task[Check]{Name: "fraud", Fn: func(_ context.Context) (Check, error) {
			return Check{Name: "fraud", Passed: false, Detail: "velocity anomaly"}, nil
		}},
	)
	if err != nil {
		log.Printf("one or more checks failed: %v", err)
	}

	fmt.Println("=== parallel checks (results in task order) ===")
	allPassed := true
	for _, c := range results {
		status := "PASS"
		if !c.Passed {
			status = "FAIL"
			allPassed = false
		}
		fmt.Printf("  [%s] %-10s %s\n", status, c.Name, c.Detail)
	}
	fmt.Printf("\ndecision: approved=%v\n", allPassed)
}
