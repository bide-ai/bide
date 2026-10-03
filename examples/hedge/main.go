// Command hedge shows the hedged-model middleware: race the primary model against a backup and
// take the first good answer, which cuts tail latency and fails over around a down provider. The
// models here are offline stubs with fixed latencies so the effect is visible without an API key;
// with real adapters (anthropic.New, openai.New, gemini.New) the usage is identical.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/middleware"
)

// stub is an offline model: it waits `delay`, then either fails (down) or answers in one turn.
type stub struct {
	name  string
	delay time.Duration
	down  bool
}

func (m stub) Stream(ctx context.Context, _ agent.Request) (*agent.Stream, error) {
	select {
	case <-time.After(m.delay):
	case <-ctx.Done():
		return nil, ctx.Err() // a losing hedge target is cancelled here
	}
	if m.down {
		return nil, &agent.APIError{StatusCode: 503, Body: "unavailable",
			Err: fmt.Errorf("%s is down (%w)", m.name, agent.ErrModel)}
	}
	ch := make(chan agent.Emit, 2)
	ch <- agent.Emit{Event: agent.TextDelta{Text: "answer from " + m.name}}
	ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	close(ch)
	return agent.NewStream(ch), nil
}

func run(label string, a *agent.Agent) {
	start := time.Now()
	res, err := a.Run(context.Background(), "run/"+label, agent.UserText("hello"))
	var msg agent.Message
	if res != nil {
		msg = res.Message
	}
	elapsed := time.Since(start).Round(time.Millisecond)
	if err != nil {
		fmt.Printf("%-28s FAILED after %v: %v\n", label, elapsed, err)
		return
	}
	fmt.Printf("%-28s %-22q in %v\n", label, msg.Text(), elapsed)
}

func main() {
	store, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		log.Fatal(err)
	}

	// 1) Tail latency: the primary is slow (400ms); a backup answers in 30ms. Without hedging you
	// wait for the slow primary; with a 50ms hedge delay you take the backup.
	slowPrimary := stub{name: "primary", delay: 400 * time.Millisecond}
	fastBackup := stub{name: "backup", delay: 30 * time.Millisecond}
	fmt.Println("== tail latency ==")
	ag, err := agent.New(slowPrimary, store)
	if err != nil {
		log.Fatal(err)
	}
	run("no hedge (slow primary)", ag)
	ag2, err := agent.New(
		slowPrimary,
		store,
		agent.WithMiddleware(middleware.Hedge(50*time.Millisecond, fastBackup)),
	)
	if err != nil {
		log.Fatal(err)
	}
	run("hedged (delay 50ms)", ag2)

	// 2) Failover: the primary is down (fails fast). The hedge delay is long (500ms), but a failed
	// target brings the backup forward immediately, so the run does not wait out the delay.
	downPrimary := stub{name: "primary", delay: 20 * time.Millisecond, down: true}
	fmt.Println("\n== provider outage ==")
	ag3, err := agent.New(downPrimary, store)
	if err != nil {
		log.Fatal(err)
	}
	run("no hedge (primary down)", ag3)
	ag4, err := agent.New(
		downPrimary,
		store,
		agent.WithMiddleware(middleware.Hedge(500*time.Millisecond, fastBackup)),
	)
	if err != nil {
		log.Fatal(err)
	}
	run("hedged (fast failover)", ag4)

	fmt.Println("\nThe winning response is journaled once by the durable loop; the losing call is")
	fmt.Println("cancelled and never touches state, so the record stays exactly-once either way.")
}
