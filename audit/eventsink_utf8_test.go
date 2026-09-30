package audit_test

import (
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// An event leaf is a JSON encoding, which encoding/json makes lossy over invalid UTF-8: every
// invalid byte becomes U+FFFD. Events that differ only in invalid bytes would share one leaf, so
// a proof of one would verify another. The log refuses such an event, as the journal and anchor
// leaves do, and so does the verifier.
func TestEventLog_InvalidUTF8IsRefused(t *testing.T) {
	for name, e := range map[string]agent.AgentEvent{
		"tool name":        agent.ToolStarted{ToolUseID: "c", Name: "\xff"},
		"model text delta": agent.ModelEvent{Event: agent.TextDelta{Text: "ok \xfe"}},
		"assistant text":   agent.AssistantTurn{Message: agent.Message{Role: agent.RoleAssistant, Parts: []agent.Part{agent.Text{Text: "\xc3"}}}},
		"approval name":    agent.ApprovalRequired{ToolUseID: "c", Name: "pay\x80", Args: json.RawMessage(`{}`)},
	} {
		if err := audit.NewEventLog().Add(e); err == nil {
			t.Errorf("%s: Add accepted invalid UTF-8", name)
		}
	}

	log := audit.NewEventLog()
	if err := log.Add(agent.ToolStarted{ToolUseID: "c", Name: "�"}); err != nil {
		t.Fatal(err)
	}
	p, err := log.Prove(0)
	if err != nil {
		t.Fatal(err)
	}
	err = audit.VerifyEventInclusion(log.Root(), agent.ToolStarted{ToolUseID: "c", Name: "\xfe"}, p)
	if err == nil {
		t.Fatalf("VerifyEventInclusion of a different event (invalid UTF-8 for U+FFFD) = %v; want an error", err)
	}
}
