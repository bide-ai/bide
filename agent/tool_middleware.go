package agent

import (
	"context"
	"encoding/json"
	"fmt"
)

// ToolHandler executes one tool call: given the ToolUse (ID, Name, Args), it returns
// the raw JSON result or an error. It is the tool-side analogue of ModelHandler.
type ToolHandler func(context.Context, ToolUse) (json.RawMessage, error)

// ToolMiddleware wraps a ToolHandler — the same func(Handler) Handler idiom as model
// Middleware, but around TOOL execution. First added = outermost. A middleware can:
//
//   - observe/log/trace a call (its name, args, duration, error);
//   - MUTATE the outgoing args (validate, redact, inject defaults) by calling next with
//     a modified ToolUse;
//   - transform the result before it is journaled;
//   - SHORT-CIRCUIT — return a result (a cache hit) or an error (a policy denial)
//     WITHOUT calling next, so the tool never runs.
//
// The chain runs INSIDE the durable, memoized step, so a short-circuit result or a
// transformed result is what gets journaled — resume replays it and never re-runs the
// middleware or the tool. Batteries live in the middleware/ package (ToolLog, ToolCache).
type ToolMiddleware func(ToolHandler) ToolHandler

// UseTool appends tool middleware wrapping every tool call (first added = outermost).
// Returns the agent for chaining. Composes with model middleware (Use) independently:
// Use wraps the model call, UseTool wraps tool calls.
func (a *Agent) UseTool(mw ...ToolMiddleware) *Agent {
	a.toolMW = append(a.toolMW, mw...)
	return a
}

// toolHandler builds the wrapped tool-execution chain once per run: a base handler that
// dispatches by name to the registered tool, wrapped by the middleware in order.
func (a *Agent) toolHandler() ToolHandler {
	h := ToolHandler(func(ctx context.Context, tu ToolUse) (json.RawMessage, error) {
		t, ok := a.tools[tu.Name]
		if !ok {
			return nil, fmt.Errorf("call to unknown tool %q: %w", tu.Name, ErrUnknownTool)
		}
		return t.Call(ctx, tu.Args)
	})
	for i := len(a.toolMW) - 1; i >= 0; i-- {
		h = a.toolMW[i](h)
	}
	return h
}
