package agent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// Rule 16 (S3, model 12's findings/s3-cancel-wedge): Cancel of an open Send turn's run does not
// block the session for ever. The cancelled message's caller gets ErrRunCancelled; the next
// message's Send reads the open turn's run's end markers, records the cancelled turn closed, and
// is answered.
func TestP14Rule16_CancelledTurnIsClosed(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	var pay counter
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "pay")}}, {text: "answer"}}}
	a := p14Build(t, model, j, agent.WithTools(pay.tool("pay", agent.Safety{}, agent.WithApproval(agent.SingleApproval()))))
	s, err := a.Session(ctx, "chat")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Send(ctx, "x")
	pa, ok := errors.AsType[*agent.ApprovalPending](err)
	if !ok {
		t.Fatalf("Send(x) = %v, want the approval pause", err)
	}
	if err := agent.Cancel(ctx, j, pa.RunID, "operator"); err != nil {
		t.Fatalf("Cancel of the turn's run = %v", err)
	}
	if _, err := s.Send(ctx, "x"); !errors.Is(err, agent.ErrRunCancelled) {
		t.Fatalf("Send(x) again = %v, want ErrRunCancelled", err)
	}
	// "y" is a new message, and its model turn answers at once (the model has seen no assistant
	// turn in y's run).
	model.turns = []p14Turn{{text: "answer to y"}}
	got, err := s.Send(ctx, "y")
	if err != nil || got.Text() != "answer to y" {
		t.Fatalf("Send(y) = %q, %v; want y answered once x's cancelled turn is closed", got.Text(), err)
	}
	if pay.n.Load() != 0 {
		t.Fatal("the cancelled turn's call ran")
	}
	// A fresh handle sees the same journal: y is answered, and x's cancelled turn holds no answer
	// in the transcript.
	s2, err := a.Session(ctx, "chat")
	if err != nil {
		t.Fatal(err)
	}
	h := s2.History()
	if len(h) != 2 || h[0].Text() != "y" || h[1].Text() != "answer to y" {
		t.Fatalf("history = %v, want only y's turn", h)
	}
	// x's turn is closed: sending x again opens a new turn, which the model answers.
	if got, err := s2.Send(ctx, "x"); err != nil || got.Text() != "answer to y" {
		t.Fatalf("Send(x) after the close = %q, %v; want a new turn answered", got.Text(), err)
	}
}

// SendMessage takes a Message and run options, and returns a Result.
func TestP14_SessionSendMessage(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	model := &p14Model{turns: []p14Turn{{text: "seen"}}}
	a := p14Build(t, model, j)
	s, err := a.Session(ctx, "chat")
	if err != nil {
		t.Fatal(err)
	}
	in := agent.UserParts(agent.Text{Text: "look"}, agent.ImageData("image/png", []byte{1, 2}))
	res, err := s.SendMessage(ctx, in, agent.WithSystemPrompt("be brief"))
	if err != nil || res == nil || res.Message.Text() != "seen" {
		t.Fatalf("SendMessage = %+v, %v", res, err)
	}
	if systemText(model.lastReq(t)) != "be brief" {
		t.Fatal("the turn did not run under its run option")
	}
	res, err = s.SendMessageOnce(ctx, "evt-1", agent.UserText("hi"))
	if err != nil || res.Message.Text() != "seen" {
		t.Fatalf("SendMessageOnce = %+v, %v", res, err)
	}
	h := s.History()
	if len(h) != 4 || len(h[0].Parts) != 2 {
		t.Fatalf("history = %v, want both turns, the first with its image", h)
	}
}
