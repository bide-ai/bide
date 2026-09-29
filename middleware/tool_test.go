package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

func TestToolCache_ShortCircuitsRepeat(t *testing.T) {
	var calls int
	base := agent.ToolHandler(func(_ context.Context, tu agent.ToolUse) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{"n":` + string(tu.Args) + `}`), nil
	})
	h := ToolCache()(base)
	ctx := agent.WithToolSafety(context.Background(), agent.Safety{ReadOnly: true}) // ToolCache caches only ReadOnly tools

	call := func(args string) string {
		res, err := h(ctx, agent.ToolUse{Name: "f", Args: json.RawMessage(args)})
		if err != nil {
			t.Fatal(err)
		}
		return string(res)
	}

	a1 := call("1")
	a2 := call("1") // identical args -> cache hit, base not called again
	if a1 != a2 {
		t.Fatalf("cached result differs: %q vs %q", a1, a2)
	}
	if calls != 1 {
		t.Fatalf("base ran %d times for identical args, want 1", calls)
	}
	call("2") // different args -> miss
	if calls != 2 {
		t.Fatalf("base ran %d times total, want 2", calls)
	}
}

func TestToolCache_DoesNotCacheErrors(t *testing.T) {
	var calls int
	base := agent.ToolHandler(func(_ context.Context, _ agent.ToolUse) (json.RawMessage, error) {
		calls++
		return nil, errors.New("boom")
	})
	h := ToolCache()(base)
	ctx := agent.WithToolSafety(context.Background(), agent.Safety{ReadOnly: true}) // ToolCache caches only ReadOnly tools

	for i := 0; i < 3; i++ {
		if _, err := h(ctx, agent.ToolUse{Name: "f", Args: json.RawMessage(`{}`)}); err == nil {
			t.Fatal("want error")
		}
	}
	if calls != 3 {
		t.Fatalf("errored call ran %d times, want 3 (errors not cached)", calls)
	}
}

func TestToolLog_LogsOutcome(t *testing.T) {
	var sb strings.Builder
	logf := func(format string, args ...any) {
		fmt.Fprintf(&sb, format, args...)
	}
	base := agent.ToolHandler(func(_ context.Context, _ agent.ToolUse) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	})
	h := ToolLog(logf)(base)

	if _, err := h(context.Background(), agent.ToolUse{ID: "c1", Name: "lookup", Args: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if got := sb.String(); !strings.Contains(got, "lookup") || !strings.Contains(got, "ok") {
		t.Fatalf("log = %q, want it to mention the tool and ok", got)
	}
}
