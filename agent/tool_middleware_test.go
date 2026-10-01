package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// recordTool captures the args it was actually called with, and counts executions.
type recordTool struct {
	name    string
	safety  Safety
	calls   *int
	lastArg *string
}

func (t *recordTool) Name() string                { return t.name }
func (t *recordTool) Description() string         { return "" }
func (t *recordTool) Safety() Safety              { return t.safety }
func (t *recordTool) ArgsSchema() json.RawMessage { return nil }
func (t *recordTool) Call(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	*t.calls++
	if t.lastArg != nil {
		*t.lastArg = string(args)
	}
	return json.RawMessage(`{"ran":true}`), nil
}

// Tool middleware runs outermost-first, can mutate outgoing args, and can transform the
// result the model sees.
func TestToolMiddleware_OrderMutateTransform(t *testing.T) {
	var calls int
	var seenArgs string
	tool := &recordTool{name: "act", safety: Safety{ReadOnly: true}, calls: &calls, lastArg: &seenArgs}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "act", `{"q":"orig"}`), textTurn("done")}}

	var order []string
	mw := func(tag string) ToolMiddleware {
		return func(next ToolHandler) ToolHandler {
			return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
				order = append(order, tag)
				return next(ctx, call)
			}
		}
	}
	// Inner middleware rewrites the args; another transforms the result.
	rewrite := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			call.Use.Args = json.RawMessage(`{"q":"rewritten"}`)
			res, err := next(ctx, call)
			if err != nil {
				return res, err
			}
			return json.RawMessage(strings.Replace(string(res), "true", "false", 1)), nil
		}
	}

	a := New(m, NewMemStore(), tool).UseTool(mw("outer"), mw("inner"), rewrite)
	out, err := a.Run(context.Background(), "r", "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := textOf(out); got != "done" {
		t.Fatalf("answer = %q", got)
	}
	if calls != 1 {
		t.Fatalf("tool ran %d times, want 1", calls)
	}
	if want := []string{"outer", "inner"}; strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", order, want)
	}
	if seenArgs != `{"q":"rewritten"}` {
		t.Fatalf("tool saw args %q, want rewritten", seenArgs)
	}
}

// A short-circuiting middleware returns a result WITHOUT calling the tool, and that
// result is what the run records and the model sees.
func TestToolMiddleware_ShortCircuits(t *testing.T) {
	var calls int
	tool := &recordTool{name: "act", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "act", `{}`), textTurn("done")}}

	deny := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			return json.RawMessage(`"cached"`), nil // never calls next
		}
	}
	a := New(m, NewMemStore(), tool).UseTool(deny)
	if _, err := a.Run(context.Background(), "r", "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 0 {
		t.Fatalf("tool ran %d times, want 0 (short-circuited)", calls)
	}
}

// The middleware chain runs INSIDE the durable step: a short-circuit result is
// journaled, so on resume it replays without re-running the middleware or the tool.
func TestToolMiddleware_ResultIsJournaled(t *testing.T) {
	store := NewMemStore()
	var calls, mwHits int
	tool := &recordTool{name: "act", safety: Safety{ReadOnly: true}, calls: &calls}

	counting := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			mwHits++
			return next(ctx, call)
		}
	}

	// First run: model asks for the tool, then crashes before answering.
	crashy := &scriptModel{turns: [][]Emit{toolTurn("c1", "act", `{}`), errTurn(errCrash)}}
	if _, err := New(crashy, store, tool).UseTool(counting).Run(context.Background(), "r", "go"); err == nil {
		t.Fatal("expected crash on first attempt")
	}
	if calls != 1 || mwHits != 1 {
		t.Fatalf("after first run: calls=%d mwHits=%d, want 1,1", calls, mwHits)
	}

	// Resume with a healthy model. The journaled tool result replays — neither the tool
	// nor the middleware runs again.
	recovered := &scriptModel{turns: [][]Emit{textTurn("done")}}
	out, err := New(recovered, store, tool).UseTool(counting).Run(context.Background(), "r", "go")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := textOf(out); got != "done" {
		t.Fatalf("answer = %q", got)
	}
	if calls != 1 {
		t.Fatalf("tool ran %d times across crash+resume, want 1", calls)
	}
	if mwHits != 1 {
		t.Fatalf("middleware ran %d times across crash+resume, want 1 (journaled result replays)", mwHits)
	}
}
