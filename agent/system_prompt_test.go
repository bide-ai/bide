package agent

import (
	"context"
	"encoding/json"
	"testing"
)

// TestSystemPrompt_SeededAsFirstMessage verifies that WithSystemPrompt causes
// the first message in the model Request to be a RoleSystem message with the
// configured text, followed by the RoleUser input.
func TestSystemPrompt_SeededAsFirstMessage(t *testing.T) {
	var got Request
	m := &captureModel{inner: &scriptModel{turns: [][]Emit{textTurn("ok")}}, got: &got}
	a := mustNew(m, memJournal(), WithSystemPrompt("you are terse"))

	if _, err := a.Run(context.Background(), "r1", "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(got.Messages) < 2 {
		t.Fatalf("expected at least 2 messages, got %d", len(got.Messages))
	}
	sys := got.Messages[0]
	if sys.Role != RoleSystem {
		t.Fatalf("Messages[0].Role = %q, want %q", sys.Role, RoleSystem)
	}
	if len(sys.Parts) == 0 {
		t.Fatal("Messages[0] has no parts")
	}
	txt, ok := sys.Parts[0].(Text)
	if !ok {
		t.Fatalf("Messages[0].Parts[0] type = %T, want Text", sys.Parts[0])
	}
	if txt.Text != "you are terse" {
		t.Fatalf("system text = %q, want %q", txt.Text, "you are terse")
	}

	user := got.Messages[1]
	if user.Role != RoleUser {
		t.Fatalf("Messages[1].Role = %q, want %q", user.Role, RoleUser)
	}
}

// TestSystemPrompt_AbsentByDefault verifies that without WithSystemPrompt the
// first message is the RoleUser input (no system message prepended).
func TestSystemPrompt_AbsentByDefault(t *testing.T) {
	var got Request
	m := &captureModel{inner: &scriptModel{turns: [][]Emit{textTurn("ok")}}, got: &got}
	a := mustNew(m, memJournal())

	if _, err := a.Run(context.Background(), "r2", "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(got.Messages) == 0 {
		t.Fatal("expected at least 1 message, got 0")
	}
	if got.Messages[0].Role != RoleUser {
		t.Fatalf("Messages[0].Role = %q, want %q (no system prompt)", got.Messages[0].Role, RoleUser)
	}
}

// TestSystemPrompt_PersistsAcrossToolTurns verifies that the system message
// appears at position 0 on the second model call (after a tool turn), confirming
// it is re-seeded for every call.
func TestSystemPrompt_PersistsAcrossToolTurns(t *testing.T) {
	var requests []Request
	inner := &scriptModel{turns: [][]Emit{
		toolTurn("c1", "noop", `{}`),
		textTurn("done"),
	}}
	capture := &multiCaptureModel{inner: inner, requests: &requests}

	noopTool := Func("noop",
		"does nothing",
		Safety{ReadOnly: true},
		func(_ context.Context, _ struct{}) (struct{}, error) { return struct{}{}, nil })

	a := mustNew(capture, memJournal(), WithTools(noopTool), WithSystemPrompt("be brief"))

	if _, err := a.Run(context.Background(), "r3", "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(requests) < 2 {
		t.Fatalf("expected at least 2 model calls, got %d", len(requests))
	}
	for i, req := range requests {
		if len(req.Messages) == 0 {
			t.Fatalf("turn %d: no messages", i)
		}
		if req.Messages[0].Role != RoleSystem {
			t.Fatalf("turn %d: Messages[0].Role = %q, want system", i, req.Messages[0].Role)
		}
		txt, ok := req.Messages[0].Parts[0].(Text)
		if !ok || txt.Text != "be brief" {
			t.Fatalf("turn %d: system text = %q, want %q", i, txt.Text, "be brief")
		}
	}
}

// multiCaptureModel records every Request passed to Stream.
type multiCaptureModel struct {
	inner    Model
	requests *[]Request
}

func (m *multiCaptureModel) Stream(ctx context.Context, req Request) (*Stream, error) {
	// deep-copy Messages slice so journal replay appends don't mutate our snapshot
	cp := make([]Message, len(req.Messages))
	copy(cp, req.Messages)
	*m.requests = append(*m.requests, Request{
		Messages: cp,
		Tools:    req.Tools,
		Sampling: req.Sampling,
	})
	return m.inner.Stream(ctx, req)
}

// Ensure multiCaptureModel satisfies the tool-args schema for the noop tool by
// providing a minimal ArgsSchema implementation via the Func helper. The test
// already uses Func, so this comment is for clarity only.
var _ json.RawMessage = nil // suppress unused-import lint
