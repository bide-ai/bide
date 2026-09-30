package agent

import (
	"context"
	"encoding/json"
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
// A middleware that may call next more than once (a retry) or not at all (a cache) must check
// the tool's Safety first, with ToolSafety(ctx): repeating or skipping a side effect is not
// its call to make. The agent enforces the first half itself: a tool that is not RetrySafe
// runs at most once per tool call, and a second call to next for it returns
// ErrToolReinvoked without running the tool.
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

type toolSafetyKey struct{}

// ToolSafety returns the Safety of the tool that a tool-middleware call is for, so a
// middleware can tell a side effect from a read before it retries, caches, or skips a call.
// ok is false outside a tool call, or when the call names no registered tool; a middleware
// should then treat the call as a side effect.
func ToolSafety(ctx context.Context) (s Safety, ok bool) {
	s, ok = ctx.Value(toolSafetyKey{}).(Safety)
	return s, ok
}

// WithToolSafety returns ctx carrying s as the Safety that ToolSafety reports. The agent sets
// it for every tool call; call it yourself to run tool middleware outside an agent, as in a
// test. It informs middleware only: the agent's at-most-once check reads the registered
// tool's Safety, never this value.
func WithToolSafety(ctx context.Context, s Safety) context.Context {
	return context.WithValue(ctx, toolSafetyKey{}, s)
}

// modelArgsKey carries a call's arguments as the model sent them, before tool middleware.
type modelArgsKey struct{}
