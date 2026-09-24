package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dayna/go-agents/schema"
)

// Tool is an action the agent can take. The interface is untyped (json.RawMessage)
// so heterogeneous tools share one type — including runtime MCP tools whose schema
// is only known at runtime and can't be a Go struct. Native tools get compile-time
// typing via Func (below); that's the primary ergonomic.
type Tool interface {
	Name() string
	Description() string
	// ArgsSchema is the provider-NEUTRAL argument schema. The schema/ package emits
	// per-provider dialects (OpenAI-strict / Gemini / Anthropic) from it — so this is
	// never a single frozen blob handed straight to a provider.
	ArgsSchema() json.RawMessage
	// Safety declares how the tool may be retried on resume.
	Safety() Safety
	Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}

// Safety declares how a tool may be retried when a run resumes after a crash and the
// call's outcome is unknown (it was invoked, but no result was journaled). Replaces a
// coarse Read/Write flag. Maps directly onto MCP tool annotations
// (readOnlyHint / idempotentHint / destructiveHint) for MCP-sourced tools.
type Safety struct {
	// ReadOnly: no external side effects — always safe to re-run.
	ReadOnly bool
	// Idempotent: mutates state but is safe to retry, because a repeat with the same
	// key is a no-op downstream. Set IdempotencyKey to make that concrete.
	Idempotent bool
	// IdempotencyKey derives a stable key from the args so a retried call can be
	// de-duplicated. Empty result => not derivable.
	IdempotencyKey func(args json.RawMessage) string
	// RequiresApproval pauses the run for a durable human decision (HITL) before the
	// tool executes — surfaced as *PendingApproval; resume after agent.Approve.
	RequiresApproval bool
}

// retriableOnResume reports whether an unknown-outcome call may be safely re-run.
// Anything else halts the run for confirmation rather than risk a double side effect.
func (s Safety) retriableOnResume() bool { return s.ReadOnly || s.Idempotent }

// Func wraps a typed Go function into a Tool. In is JSON-decoded from the args; the
// return value is JSON-encoded. This is the compile-time-typed ergonomic: change In
// and the handler won't compile. The Tool interface itself stays untyped so a map of
// mixed tools (and runtime MCP tools) works.
func Func[In, Out any](name, description string, safety Safety, fn func(context.Context, In) (Out, error)) Tool {
	// Derive the provider-neutral argument schema from In once, at construction. Adapters
	// dialectize it (schema.OpenAIStrict etc.) at request time.
	argsSchema, _ := schema.For[In]()
	return &funcTool[In, Out]{name: name, description: description, safety: safety, fn: fn, argsSchema: argsSchema}
}

type funcTool[In, Out any] struct {
	name, description string
	safety            Safety
	argsSchema        json.RawMessage
	fn                func(context.Context, In) (Out, error)
}

func (t *funcTool[In, Out]) Name() string               { return t.name }
func (t *funcTool[In, Out]) Description() string         { return t.description }
func (t *funcTool[In, Out]) Safety() Safety              { return t.safety }
func (t *funcTool[In, Out]) ArgsSchema() json.RawMessage { return t.argsSchema }

func (t *funcTool[In, Out]) Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var in In
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("decode args for tool %q: %w (%w)", t.name, err, ErrToolArgs)
		}
	}
	out, err := t.fn(ctx, in)
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}
