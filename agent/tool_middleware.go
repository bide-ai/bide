package agent

import (
	"context"
	"encoding/json"
	"sync/atomic"
)

// ToolCall is one tool call as tool middleware sees it: the ToolUse the model sent (ID, Name,
// Args), the Spec of the tool it names, and the run it belongs to. A middleware reads Spec to
// tell a side effect from a read before it retries, caches, or skips a call, and may pass next
// a copy with different Use.Args. It passes next the ToolCall it was given (or a copy): a call
// whose Use.Name or Use.ID a middleware changed, or a ToolCall it built itself, fails with
// ErrConfig and the tool is not called. Spec is the zero ToolSpec (a side effect) for a call that
// names no registered tool.
//
// Spec informs middleware only. The agent decides from its own copy of the registered tool's
// spec: a middleware that changes Spec changes nothing the agent enforces.
type ToolCall struct {
	Use   ToolUse
	Spec  ToolSpec
	RunID string // the run whose model made the call

	// redact is the agent's WithToolErrorRedactor (see ErrorText).
	redact func(tool string, err error) string
	// modelArgs are the arguments as the model sent them, before any middleware changed Use.Args.
	modelArgs json.RawMessage
	// origName and origID are the call as the model made it: the base handler refuses a call a
	// middleware renamed or re-identified.
	origName, origID string
	// state is whether the call reached its tool (see callOpen), changed by compare-and-swap only.
	state *atomic.Int32
	// began is whether any invocation began the tool's Call (see beganNone), by compare-and-swap.
	began *atomic.Int32
	// out is the tool's own outcome (see toolNotRun).
	out *atomic.Int32
}

// ErrorText returns the text the agent journals, and sends to the model, for this call failing
// with err: the text chosen by the agent's WithToolErrorRedactor, if it has one, and otherwise
// err's own text, in either case with every URL in it redacted. A tool middleware that records a
// failed call's error text (a trace span, a log line) uses it to record no more than the journal
// holds. A ToolCall the middleware built itself, rather than one the agent passed it, has no
// redactor: its text is err's own, URLs redacted.
func (c ToolCall) ErrorText(err error) string { return toolErrorText(c.redact, c.Use.Name, err) }

// ToolHandler executes one tool call and returns the raw JSON result or an error. It is the
// tool-side analogue of ModelHandler.
type ToolHandler func(ctx context.Context, call ToolCall) (json.RawMessage, error)

// ToolMiddleware wraps a ToolHandler, the same func(Handler) Handler idiom as model
// Middleware, but around TOOL execution. First added = outermost. A middleware can:
//
//   - observe/log/trace a call (its name, args, duration, error);
//   - MUTATE the outgoing args (validate, redact, inject defaults) by calling next with
//     a copy of the ToolCall whose Use.Args differ;
//   - transform the result before it is journaled (turning a side effect's success into an
//     error halts the run: see below);
//   - SHORT-CIRCUIT: return a result (a cache hit) or an error (a policy denial)
//     WITHOUT calling next, so the tool never runs.
//
// A middleware that may call next more than once (a retry) or not at all (a cache) must check
// the call's Spec.Safety first: repeating or skipping a side effect is not its call to make. The
// agent enforces the first half itself: a tool that is not RetrySafe runs at most once per tool
// call, and a second call to next for it returns ErrToolReinvoked without running the tool.
//
// A middleware reaches the tool only through next: never by calling the tool itself, and never by
// leaving next running after it returns (the agent refuses an invocation of next that comes after
// the chain returned). When it ends a call without calling next (a denial, a rate limiter that
// gives up), it returns an error wrapping ErrToolNotCalled, and it returns that error only then.
// The agent needs positive proof that a side effect was not called: a chain that returns an error
// without calling next, and without ErrToolNotCalled, leaves the side effect's outcome unknown,
// and the run halts for it rather than risk running it twice. It needs positive proof that a side
// effect failed, too: a middleware may transform a result, but one that turns a side effect's
// success into an error (or returns another error for a call whose tool did not itself fail) makes
// the outcome unknown, and the run halts for it. The model is never told a fired effect failed.
//
// The chain runs INSIDE the durable, memoized step, so a short-circuit result or a
// transformed result is what gets journaled: resume replays it and never re-runs the
// middleware or the tool. A tool's Timeout bounds the whole chain. Batteries live in the
// middleware/ package (ToolLog, ToolCache, ToolRetry).
type ToolMiddleware func(ToolHandler) ToolHandler

// UseTool appends tool middleware wrapping every tool call (first added = outermost).
// Returns the agent for chaining. Composes with model middleware (Use) independently:
// Use wraps the model call, UseTool wraps tool calls.
func (a *Agent) UseTool(mw ...ToolMiddleware) *Agent {
	a.toolMW = append(a.toolMW, mw...)
	return a
}
