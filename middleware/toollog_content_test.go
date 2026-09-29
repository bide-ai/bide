package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// logFailedCharge runs a failing tool call whose error embeds its arguments through ToolLog and
// returns what was logged.
func logFailedCharge(t *testing.T, opts ...ToolLogOption) string {
	t.Helper()
	base := agent.ToolHandler(func(_ context.Context, tu agent.ToolUse) (json.RawMessage, error) {
		return nil, fmt.Errorf("charge failed for %s: %w", tu.Args, agent.ErrTool)
	})
	var sb strings.Builder
	h := ToolLog(func(format string, args ...any) { fmt.Fprintf(&sb, format, args...) }, opts...)(base)
	if _, err := h(context.Background(), agent.ToolUse{ID: "c1", Name: "charge", Args: json.RawMessage(`{"card":"4111111111111111"}`)}); err == nil {
		t.Fatal("want error")
	}
	return sb.String()
}

// By default ToolLog records a failed call by its error's summary, not its text, which commonly
// embeds the call's arguments.
func TestToolLog_DefaultOmitsErrorText(t *testing.T) {
	if got := logFailedCharge(t); strings.Contains(got, "4111111111111111") || !strings.Contains(got, "charge (c1) error") || !strings.HasSuffix(got, ": tool") {
		t.Errorf("log = %q, want the tool, its call id, and the error summary, and no arguments", got)
	}
}

// LogErrorText asks for the full text.
func TestToolLog_LogErrorText(t *testing.T) {
	if got := logFailedCharge(t, LogErrorText()); !strings.Contains(got, "charge failed for {\"card\":\"4111111111111111\"}: tool") {
		t.Errorf("log = %q, want the error text", got)
	}
}
