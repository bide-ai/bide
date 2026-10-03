// Command recover shows durable resume via named-step memoization: a run journals its
// model turns and tool results to the store, so re-invoking Run with the SAME runID against
// the SAME store reuses the recorded steps rather than re-running them. The tool here
// increments a process-local counter on every real execution; after the run completes, a
// second Run leaves the counter unchanged, demonstrating at-most-once execution across a
// resume (the property a crash-recovery supervisor relies on; see agent.Recover).
//
// It then shows the other side of at-most-once: a side effect whose outcome was lost is not
// fired again. The resumed Step halts with *agent.OutcomeUnknown, an operator records the
// verified outcome with agent.ResolveHaltRef, and the Step then returns it.
//
// It runs with NO API key: the model is a small inline scripted Model.
//
//	go run ./examples/recover
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync/atomic"

	"github.com/bide-ai/bide/agent"
)

// scriptModel calls the charge tool once, then answers in text on the next turn.
type scriptModel struct{ turn int }

func (m *scriptModel) Stream(_ context.Context, _ agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 4)
	if m.turn == 0 {
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "c1", Name: "charge_card", ArgsFragment: []byte(`{}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	} else {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "The card was charged once."}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	}
	m.turn++
	close(ch)
	return agent.NewStream(ch), nil
}

func main() {
	ctx := context.Background()
	store, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		log.Fatal(err)
	}
	const runID = "recover-1"

	// The counter stands in for a real side effect. It increments only when the tool BODY
	// runs; a memoized (replayed) step does not run the body, so the counter is the visible
	// witness of at-most-once execution. Marked Idempotent so a resume is retry-safe.
	var charges atomic.Int64
	charge := agent.Func("charge_card", "Charge the customer's card once",
		agent.Safety{Idempotent: true},
		func(_ context.Context, _ struct{}) (string, error) {
			n := charges.Add(1)
			return fmt.Sprintf("charged (execution #%d)", n), nil
		})

	// A fresh scriptModel per Run: the model is only asked for turns that are NOT already
	// journaled, so on the second Run the recorded model turns are replayed from the store
	// and this fresh model is never called.
	newAgent := func() *agent.Agent {
		ag, err := agent.New(&scriptModel{}, store, agent.WithTools(charge))
		if err != nil {
			log.Fatal(err)
		}
		return ag
	}

	// First Run: drives to completion, journaling the model turns and the tool result.
	out1, err := newAgent().Run(ctx, runID, "Charge the card, then confirm.")
	if err != nil {
		log.Fatalf("first run: %v", err)
	}
	fmt.Printf("first run:  %s\n", out1.Text())
	fmt.Printf("side effect fired %d time(s)\n", charges.Load())

	complete, err := agent.IsComplete(ctx, store, runID)
	if err != nil {
		log.Fatalf("IsComplete: %v", err)
	}
	fmt.Printf("run complete in journal: %v\n", complete)

	// Second Run, SAME runID + SAME store: every step is memoized, so the model is not
	// called and the tool body does not run again. The counter stays at 1.
	out2, err := newAgent().Run(ctx, runID, "Charge the card, then confirm.")
	if err != nil {
		log.Fatalf("resume run: %v", err)
	}
	fmt.Printf("second run: %s\n", out2.Text())
	fmt.Printf("side effect fired %d time(s) total (unchanged: at-most-once across resume)\n", charges.Load())

	haltScene(ctx, store)
}

// haltScene: a Step that is a side effect (no WithSafety) journals an attempt marker before it
// runs. Its connection drops after the request went out, so nothing is recorded but the marker;
// the resumed Step cannot know whether the invoice was sent, and halts instead of sending it
// again. The operator checks the provider and resolves the halt with what really happened.
func haltScene(ctx context.Context, store *agent.Journal) {
	const runID = "recover-2"
	var sends atomic.Int64
	send := func(context.Context) (string, error) {
		sends.Add(1)
		return "", errors.New("connection dropped after the request went out")
	}
	if _, err := agent.Step(ctx, store, runID, "send-invoice", send); err == nil {
		log.Fatal("first attempt: want the lost answer reported")
	}
	_, err := agent.Step(ctx, store, runID, "send-invoice", send)
	halt, ok := errors.AsType[*agent.OutcomeUnknown](err)
	if !ok {
		log.Fatalf("resumed step: want *OutcomeUnknown, got %v", err)
	}
	fmt.Printf("halted: %s %q has an unknown outcome (cause %s); sends so far: %d\n", halt.Op.Kind, halt.Op.ID, halt.Cause, sends.Load())

	// The operator confirms with the provider that invoice INV-7 did go out, and records it.
	if err := agent.ResolveHaltRef(ctx, store, halt.Ref(), agent.Outcome{Result: "INV-7 (operator-confirmed)"}); err != nil {
		log.Fatalf("resolve: %v", err)
	}
	got, err := agent.Step(ctx, store, runID, "send-invoice", send)
	if err != nil {
		log.Fatalf("after resolve: %v", err)
	}
	fmt.Printf("resolved: step returns %q; sends total: %d (never fired twice)\n", got, sends.Load())
}
