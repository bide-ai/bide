// Command interrupt shows human-in-the-loop as a durable pause: a retry-safe tool calls
// agent.Interrupt to ask a human a typed question, Run returns *InterruptPending (the run
// has paused at the interrupt point), the caller records an answer with agent.Journal.AnswerInterrupt,
// and re-invoking Run with the SAME runID resumes past the interrupt with that answer.
//
// It runs with NO API key: the model is a small inline scripted Model that calls the tool
// once, then answers in text once the tool returns.
//
//	go run ./examples/interrupt
package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/bide-ai/bide/agent"
)

// scriptModel is a deterministic Model: turn 0 calls the review tool; turn 1 (after the
// tool result comes back) emits a final text answer. Stream returns scripted Emit values,
// so the example needs no live LLM.
type scriptModel struct{ turn int }

func (m *scriptModel) Stream(_ context.Context, _ agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 4)
	if m.turn == 0 {
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "c1", Name: "publish_review", ArgsFragment: []byte(`{}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	} else {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "Published the review as approved by the human."}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	}
	m.turn++
	close(ch)
	return agent.NewStream(ch), nil
}

// decision is the typed value the human supplies at the interrupt point.
type decision struct {
	Approved bool   `json:"approved"`
	Note     string `json:"note"`
}

func main() {
	ctx := context.Background()
	store, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		log.Fatal(err)
	}
	const runID = "interrupt-1"
	const gate = "review-gate"

	// A retry-safe tool (ReadOnly) that pauses for a typed human decision. On the first
	// encounter Interrupt returns the zero value and an *InterruptPending error that
	// propagates out of Run; after AnswerInterrupt records a decision and Run is re-invoked, it returns that
	// decision and the tool proceeds.
	reviewTool := agent.MustFunc("publish_review", "Publish a review after a human approves it",
		func(ctx context.Context, _ struct{}) (string, error) {
			d, err := agent.Interrupt[decision](ctx, gate, "Approve publishing this review?")
			if err != nil {
				return "", err // *InterruptPending on first pass: propagates out of Run
			}
			if !d.Approved {
				return "human declined; not published", nil
			}
			return "published (" + d.Note + ")", nil
		}, agent.WithSafety(agent.Safety{ReadOnly: true}))

	a, err := agent.New(&scriptModel{}, store, agent.WithTools(reviewTool))
	if err != nil {
		log.Fatal(err)
	}

	// First Run: the tool interrupts, so Run returns *InterruptPending rather than a final answer.
	_, err = a.Run(ctx, runID, agent.UserText("Review and publish the draft."))
	itr, ok := errors.AsType[*agent.InterruptPending](err)
	if !ok {
		log.Fatalf("expected an *InterruptPending pause, got: %v", err)
	}
	fmt.Printf("paused: run %s at %q asking: %v\n", itr.RunID, itr.Name, itr.Prompt)

	// The human answers. AnswerInterrupt records the typed decision durably (first value wins),
	// against the journal the pause lives in (itr.RunID).
	if err := store.AnswerInterrupt(ctx, itr.RunID, itr.Name, decision{Approved: true, Note: "looks good"}); err != nil {
		log.Fatalf("answer: %v", err)
	}
	fmt.Println("recorded human decision; resuming the run")

	// Re-invoke Run with the pause's RootRunID (the same run here): Interrupt now returns the
	// decision, the tool completes, and the model produces its final answer.
	res, err := a.Run(ctx, itr.RootRunID, agent.UserText("Review and publish the draft."))
	if err != nil {
		log.Fatalf("resume run: %v", err)
	}
	out := res.Message
	fmt.Printf("resumed result: %s\n", out.Text())
}
