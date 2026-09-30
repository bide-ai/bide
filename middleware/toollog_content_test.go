package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// logFailedCharge runs a failing tool call whose error embeds its arguments through ToolLog and
// returns what was logged.
func logFailedCharge(t *testing.T, opts ...ToolLogOption) string {
	t.Helper()
	base := agent.ToolHandler(func(_ context.Context, call agent.ToolCall) (json.RawMessage, error) {
		return nil, fmt.Errorf("charge failed for %s: %w", call.Use.Args, agent.ErrTool)
	})
	var sb strings.Builder
	h := ToolLog(func(format string, args ...any) { fmt.Fprintf(&sb, format, args...) }, opts...)(base)
	if _, err := h(context.Background(), agent.ToolCall{Use: agent.ToolUse{ID: "c1", Name: "charge", Args: json.RawMessage(`{"card":"4111111111111111"}`)}}); err == nil {
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

// LogErrorText logs the text the agent journals for the failed call, not the error's raw text:
// the agent's WithToolErrorRedactor applies, and every URL is redacted, so a log line holds no
// credential the journal keeps out.
func TestToolLog_LogErrorTextIsTheJournaledText(t *testing.T) {
	type in struct{}
	tool := agent.Func("fetch", "fetch", agent.Safety{ReadOnly: true}, func(context.Context, in) (string, error) {
		return "", fmt.Errorf("account ACCT-998877: %w", &url.Error{Op: "Get", URL: "https://u:PASSWORD-1@h.test/x?key=SECRET-KEY-123", Err: errors.New("timeout")})
	})
	var sb strings.Builder
	st := agent.NewMemStore()
	a := agent.New(agent.NewScriptedModel(agent.ToolTurn("c1", "fetch", `{}`), agent.TextTurn("done")), st, tool).
		WithToolErrorRedactor(func(_ string, err error) string { return strings.ReplaceAll(err.Error(), "ACCT-998877", "ACCT") }).
		UseTool(ToolLog(func(format string, args ...any) { fmt.Fprintf(&sb, format, args...) }, LogErrorText()))
	if _, err := a.Run(context.Background(), "r", "go"); err != nil {
		t.Fatal(err)
	}
	var journaled string
	recs, _ := st.History(context.Background(), "r")
	for _, r := range recs {
		if r.Kind == agent.StepToolResult {
			_ = json.Unmarshal(r.Result, &journaled)
		}
	}
	got := sb.String()
	for _, c := range []string{"ACCT-998877", "PASSWORD-1", "SECRET-KEY-123"} {
		if strings.Contains(got, c) {
			t.Errorf("log carries %q: %s", c, got)
		}
	}
	if journaled == "" || !strings.HasSuffix(got, ": "+journaled) {
		t.Errorf("log = %q, want it to end with the journaled text %q", got, journaled)
	}
}
