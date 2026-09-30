// Command signals shows the externally-pushed durable pauses: Await for a single-shot
// signal delivered from outside the run with agent.Signal, AwaitFor with a durable timeout
// (the signal-vs-deadline race), and an ordered per-run channel consumed exactly-once with
// Send / Receive / Ack. All three ride the same durable journal, so a signal is applied at
// most once and channel consumption is replay-safe.
//
// It runs with NO API key: each scene uses a small inline scripted Model.
//
//	go run ./examples/signals
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/bide-ai/bide/agent"
)

// oneTool calls the named tool once, then answers in text once the tool's result is in the
// conversation. It decides from the request rather than counting its own turns, so a resumed
// run, driven by a fresh agent and model, gets the answer turn and not a second tool call.
// Reused by the three scenes with a different tool name each.
type oneTool struct {
	tool string
}

func (m *oneTool) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 4)
	answered := false
	for _, msg := range req.Messages {
		answered = answered || msg.Role == agent.RoleTool
	}
	if !answered {
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "c1", Name: m.tool, ArgsFragment: []byte(`{}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	} else {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "done"}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

func main() {
	awaitScene()
	awaitForScene()
	channelScene()
}

// awaitScene: a retry-safe tool Awaits a signal. The first Run pauses (*Awaiting); after an
// outside caller delivers the signal, re-running resolves the await with the payload.
func awaitScene() {
	fmt.Println("== Await: wait for an external signal ==")
	ctx := context.Background()
	store := agent.NewMemStore()
	const runID, sig = "await-1", "approval"

	tool := agent.Func("wait_for_approval", "Wait for an external approval signal",
		agent.Safety{ReadOnly: true},
		func(ctx context.Context, _ struct{}) (string, error) {
			who, err := agent.Await[string](ctx, sig)
			if err != nil {
				return "", err // *Awaiting on the first pass
			}
			return "approved by " + who, nil
		})
	a := agent.New(&oneTool{tool: "wait_for_approval"}, store, tool)

	_, err := a.Run(ctx, runID, "Wait for approval, then confirm.")
	var awt *agent.Awaiting
	if !errors.As(err, &awt) {
		log.Fatalf("expected *Awaiting, got %v", err)
	}
	fmt.Printf("  paused awaiting signal %q\n", awt.Name)

	// Delivered from outside the run (another process, a webhook). Journaled at-most-once.
	if err := agent.Signal(ctx, store, runID, sig, "alice"); err != nil {
		log.Fatalf("signal: %v", err)
	}
	out, err := agent.New(&oneTool{tool: "wait_for_approval"}, store, tool).Run(ctx, runID, "Wait for approval, then confirm.")
	if err != nil {
		log.Fatalf("resume: %v", err)
	}
	fmt.Printf("  resumed: tool observed the signal, final answer: %s\n\n", out.Text())
}

// awaitForScene: AwaitFor races a signal against a durable deadline. Here no signal is
// delivered and the deadline is in the past, so the timeout branch wins deterministically.
func awaitForScene() {
	fmt.Println("== AwaitFor: signal-or-timeout race ==")
	ctx := context.Background()
	store := agent.NewMemStore()
	const runID = "awaitfor-1"

	tool := agent.Func("wait_briefly", "Wait for a signal but give up quickly",
		agent.Safety{ReadOnly: true},
		func(ctx context.Context, _ struct{}) (string, error) {
			payload, ok, err := agent.AwaitFor[string](ctx, "late-signal", time.Millisecond)
			if err != nil {
				return "", err
			}
			if ok {
				return "got signal: " + payload, nil
			}
			return "timed out waiting for the signal", nil
		})
	a := agent.New(&oneTool{tool: "wait_briefly"}, store, tool)

	// First Run journals the 1ms deadline and pauses; by the resume the deadline has passed
	// and no signal arrived, so AwaitFor returns (zero, false, nil): the timeout wins.
	if _, err := a.Run(ctx, runID, "Wait briefly."); err != nil {
		var awt *agent.Awaiting
		if !errors.As(err, &awt) {
			log.Fatalf("first run: %v", err)
		}
	}
	time.Sleep(5 * time.Millisecond) // let the durable deadline elapse before resuming
	if _, err := agent.New(&oneTool{tool: "wait_briefly"}, store, tool).Run(ctx, runID, "Wait briefly."); err != nil {
		log.Fatalf("resume: %v", err)
	}
	recs, _ := store.History(ctx, runID)
	for _, r := range recs {
		if r.Kind == agent.StepToolResult && !r.IsError {
			fmt.Printf("  tool observed: %s\n\n", string(r.Result))
		}
	}
}

// channelScene: an ordered per-run channel. Messages are Sent from outside, then a tool
// consumes them exactly-once by looping Receive -> handle -> Ack until the channel drains.
func channelScene() {
	fmt.Println("== Channel: ordered Send / Receive / Ack ==")
	ctx := context.Background()
	store := agent.NewMemStore()
	const runID, chName = "channel-1", "jobs"

	// Deliver three ordered messages before the run consumes them. Send dedups by key.
	for _, m := range []struct{ key, body string }{{"k1", "first"}, {"k2", "second"}, {"k3", "third"}} {
		if err := agent.Send(ctx, store, runID, chName, m.key, m.body); err != nil {
			log.Fatalf("send %s: %v", m.key, err)
		}
	}

	tool := agent.Func("drain_channel", "Consume every queued message in order, exactly once",
		agent.Safety{ReadOnly: true},
		func(ctx context.Context, _ struct{}) ([]string, error) {
			var consumed []string
			for {
				msg, err := agent.Receive[string](ctx, chName)
				if err != nil {
					var awt *agent.Awaiting
					if errors.As(err, &awt) {
						return consumed, nil // channel drained: no more unacked messages
					}
					return nil, err
				}
				consumed = append(consumed, msg.Payload)
				// Ack marks it consumed so Receive advances; until Ack, Receive keeps
				// returning the SAME message, which is what makes handling replay-safe.
				if err := agent.Ack(ctx, store, runID, chName, msg.Key); err != nil {
					return nil, err
				}
			}
		})

	a := agent.New(&oneTool{tool: "drain_channel"}, store, tool)
	if _, err := a.Run(ctx, runID, "Drain the channel."); err != nil {
		log.Fatalf("run: %v", err)
	}
	recs, _ := store.History(ctx, runID)
	for _, r := range recs {
		if r.Kind == agent.StepToolResult && !r.IsError {
			fmt.Printf("  consumed in order: %s\n", string(r.Result))
		}
	}
}
