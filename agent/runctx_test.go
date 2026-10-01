package agent

import (
	"context"
	"testing"
)

// asToolCall returns ctx as the agent loop gives it to the call toolUseID of run parent: its
// RunInfo and its NextOnceKey scope, with no store (a test that drives the call's sub-run by hand
// needs neither Interrupt nor Sleep from it).
func asToolCall(ctx context.Context, parent, toolUseID string) context.Context {
	return withOnceScope(withRunContext(ctx, nil, parent, toolUseID, false), SubRunID(parent, toolUseID))
}

// runScope is the sub-agent run ID of the tool call ctx belongs to, or "" outside one: what the
// removed RunScope returned.
func runScope(ctx context.Context) string {
	if info, ok := RunInfoFrom(ctx); ok && info.ToolUseID != "" {
		return SubRunID(info.RunID, info.ToolUseID)
	}
	return ""
}

// buildT builds an agent over m and a fresh MemStore's journal, failing the test on an error.
func buildT(t testing.TB, m Model, opts ...Option) *Agent {
	t.Helper()
	return buildOn(t, m, NewMemStore(), opts...)
}

// buildOn builds an agent over m and store's journal, failing the test on an error.
func buildOn(t testing.TB, m Model, store *MemStore, opts ...Option) *Agent {
	t.Helper()
	a, err := Build(m, store.Journal(), opts...)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return a
}
