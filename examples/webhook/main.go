// Command webhook shows how to drive an agent from an inbound messenger webhook
// (Slack, Telegram, Twilio, Meta, Discord) without shipping any messenger-specific code in
// the SDK. The transport lives in your webhook handler; the SDK supplies the durable run.
//
// The point of the example is idempotency. Every messenger redelivers: Slack retries any
// event you do not acknowledge within 3s (up to three times), Twilio and Telegram and Meta
// all retry on timeout. A naive handler re-runs the agent on the redelivery and fires its
// side-effecting tools twice (a double ticket, a double charge). Here the durable journal's
// at-most-once semantics make a redelivered event replay the recorded run instead of
// re-executing it, so the side effect fires exactly once. That is the same property the
// chaos benchmark proves (maxFired=1), applied to an inbound channel.
//
// Two shapes are shown, matching the two real messenger bots:
//   - Stateless command bot: key the run by the provider's event id. A redelivery hits the
//     same runID and replays. No conversation memory, simplest to reason about.
//   - Conversational bot: a per-conversation Session for memory, guarded by a Step keyed by
//     the event id so a redelivered inbound message does not open a second turn.
//
// The model here is a stub (offline, deterministic) so the example runs with no API key; the
// mechanism is identical with anthropic.New / openai.New / gemini.New.
package main

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/blackwell-systems/bide/agent"
)

// tickets counts how many times the side-effecting tool actually executed, so the example can
// prove a redelivered webhook does not double-fire it.
var tickets int64

// botModel is a stub: on the first turn of a run it calls create_ticket, then on the next turn
// (once it sees the tool result) it answers. Swap for a real provider adapter unchanged.
type botModel struct{}

func (botModel) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 2)
	// Inspect the conversation so far: a tool result means we are mid-turn (answer now); a prior
	// assistant message means this is a later conversational turn (the ticket already exists).
	hasToolResult, hasAssistant := false, false
	for _, m := range req.Messages {
		switch m.Role {
		case agent.RoleTool:
			hasToolResult = true
		case agent.RoleAssistant:
			hasAssistant = true
		}
	}
	switch {
	case hasToolResult:
		ch <- agent.Emit{Event: agent.TextDelta{Text: "Ticket opened. Anything else?"}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	case hasAssistant: // a follow-up in an existing conversation: use memory, no new ticket
		ch <- agent.Emit{Event: agent.TextDelta{Text: "Your ticket is still open and in the queue."}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	default: // first turn of the conversation: open the ticket
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "call-1", Name: "create_ticket", ArgsFragment: []byte(`{"summary":"user request"}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

func newAgent(store agent.Durable) *agent.Agent {
	// create_ticket is a genuine side effect: NOT ReadOnly, so a naive re-run would open a
	// second ticket. The durable journal is what prevents that on redelivery.
	createTicket := agent.Func("create_ticket", "open a support ticket", agent.Safety{},
		func(context.Context, struct {
			Summary string `json:"summary"`
		}) (string, error) {
			atomic.AddInt64(&tickets, 1)
			return "ticket-4711", nil
		})
	return agent.New(botModel{}, store, createTicket)
}

// statelessCommand handles a command-style event with no conversation memory. The run is keyed
// by the provider's stable event id, so a redelivery replays the recorded journal rather than
// re-running the agent. Returns the reply to post back to the channel.
func statelessCommand(ctx context.Context, a *agent.Agent, channelID, eventID, text string) (string, error) {
	runID := "msg/" + channelID + "/" + eventID // stable across redeliveries of the same event
	msg, err := a.Run(ctx, runID, text)
	if err != nil {
		return "", err
	}
	return msg.Text(), nil
}

// handleConversational handles an event in a multi-turn conversation. The Session (keyed by the
// conversation/thread id) carries memory across messages; wrapping Send in a Step keyed by the
// event id makes a redelivered inbound message return the recorded reply instead of opening a
// second turn. Session alone is NOT enough: it keys turns by index, so a redelivery would
// advance the transcript. The event-id Step is the idempotency guard.
func handleConversational(ctx context.Context, a *agent.Agent, store agent.Durable, conversationID, eventID, text string) (string, error) {
	return agent.Step(ctx, store, "inbox/"+conversationID, eventID, func(ctx context.Context) (string, error) {
		sess, err := a.Session(ctx, conversationID)
		if err != nil {
			return "", err
		}
		msg, err := sess.Send(ctx, text)
		if err != nil {
			return "", err // a pause/error is not recorded, so the next redelivery retries the turn
		}
		return msg.Text(), nil
	})
}

func main() {
	ctx := context.Background()
	store := agent.NewMemStore()
	a := newAgent(store)

	fmt.Println("== stateless command bot ==")
	// The messenger delivers event "evt-1" and, because our handler was slow to 200, redelivers
	// the identical event. Both calls go through the same runID.
	r1, _ := statelessCommand(ctx, a, "C123", "evt-1", "open a ticket please")
	r2, _ := statelessCommand(ctx, a, "C123", "evt-1", "open a ticket please") // redelivery
	fmt.Printf("delivery 1 reply: %q\n", r1)
	fmt.Printf("delivery 2 reply: %q (identical, replayed from the journal)\n", r2)
	fmt.Printf("tickets actually opened: %d (redelivery did not double-fire the side effect)\n\n", atomic.LoadInt64(&tickets))

	fmt.Println("== conversational bot ==")
	atomic.StoreInt64(&tickets, 0)
	// First inbound message, delivered twice (redelivery).
	c1, _ := handleConversational(ctx, a, store, "thread-42", "evt-A", "I need help")
	c1dup, _ := handleConversational(ctx, a, store, "thread-42", "evt-A", "I need help") // redelivery
	// A genuinely new inbound message in the same conversation.
	c2, _ := handleConversational(ctx, a, store, "thread-42", "evt-B", "what is the status")
	fmt.Printf("event A reply:            %q\n", c1)
	fmt.Printf("event A redelivery reply: %q (replayed, no second turn)\n", c1dup)
	fmt.Printf("event B reply:            %q\n", c2)

	sess, _ := a.Session(ctx, "thread-42")
	fmt.Printf("conversation turns recorded: %d (redelivery of A did not advance the transcript)\n", sess.Turns())
	fmt.Printf("tickets actually opened: %d\n", atomic.LoadInt64(&tickets))
}
