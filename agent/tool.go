package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/bide-ai/bide/internal/strictjson"
	"github.com/bide-ai/bide/schema"
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
// coarse Read/Write flag. For a trusted MCP server, its tool annotations
// (readOnlyHint / idempotentHint) map onto it (see mcp.TrustAnnotations).
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
	// Approval, when non-nil, upgrades the approval gate from one
	// decision to an m-of-n human gate over a bounded, named set of
	// approvers. nil = the existing 1-of-1 RequiresApproval behavior.
	Approval *ApprovalPolicy
}

// ApprovalPolicy declares a k-of-n human gate. Need is k; the eligible
// approvers are the bounded set (n = len(Approvers)). A tool with a
// non-nil Approval requires approval whether or not RequiresApproval is
// also set.
type ApprovalPolicy struct {
	Need      int      // decisions required to proceed (k), 1 <= Need <= len(Approvers)
	Approvers []string // eligible approver ids; the bounded set (n)
}

// retriableOnResume reports whether an unknown-outcome call may be safely re-run.
// Anything else halts the run for confirmation rather than risk a double side effect.
//
// A declared IdempotencyKey counts as retry-safe: the tool asserts that a retried call
// with the same args de-duplicates downstream, so on an unknown outcome the run may
// safely retry it instead of firing *ResumeHalt. The contract is the tool's to keep: it
// must send that key to the downstream. The SDK derives the same key from the same args
// on retry, but does not itself call the downstream, so the de-duplication happens only if
// the tool forwards the key. This turns halt-for-a-human stops into automatic retries for
// autonomous and ambient agents whose tools carry idempotency keys.
func (s Safety) retriableOnResume() bool {
	return s.ReadOnly || s.Idempotent || s.IdempotencyKey != nil
}

// RetrySafe reports whether a call to the tool may run more than once for one tool call: it
// is ReadOnly, Idempotent, or carries an IdempotencyKey. It is the test the agent applies on
// resume, and the one tool middleware applies before retrying a call (see ToolSafety).
func (s Safety) RetrySafe() bool { return s.retriableOnResume() }

// Func wraps a typed Go function into a Tool. In is decoded from the args strictly, so the
// tool reads exactly what the model sent: a missing required field (one schema.For lists as
// required), an unknown name or a case variant of a field's name, a duplicate name, data after
// the value, invalid UTF-8, or an escaped lone surrogate is ErrToolArgs, which goes back to the
// model as a tool error to correct. Empty args are the empty object. The return value is
// JSON-encoded. This is the compile-time-typed ergonomic: change In and the
// handler won't compile. The Tool interface itself stays untyped so a map of mixed tools (and
// runtime MCP tools) works.
//
// Func panics, as New does for a missing model, if schema.For cannot describe In: such a type
// (a field reached through an embedded pointer to an unexported struct) could never be decoded
// from a call's arguments, so the tool would fail every call.
func Func[In, Out any](name, description string, safety Safety, fn func(context.Context, In) (Out, error)) Tool {
	// Derive the provider-neutral argument schema from In once, at construction. Adapters
	// dialectize it (schema.OpenAIStrict etc.) at request time.
	argsSchema, err := schema.For[In]()
	if err != nil {
		panic(fmt.Errorf("agent: Func %q: argument type: %w", name, err))
	}
	return &funcTool[In, Out]{name: name, description: description, safety: safety, fn: fn, argsSchema: argsSchema}
}

type funcTool[In, Out any] struct {
	name, description string
	safety            Safety
	argsSchema        json.RawMessage
	fn                func(context.Context, In) (Out, error)
}

func (t *funcTool[In, Out]) Name() string                { return t.name }
func (t *funcTool[In, Out]) Description() string         { return t.description }
func (t *funcTool[In, Out]) Safety() Safety              { return t.safety }
func (t *funcTool[In, Out]) ArgsSchema() json.RawMessage { return t.argsSchema }

func (t *funcTool[In, Out]) Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var in In
	if err := decodeArgs(args, &in); err != nil {
		return nil, fmt.Errorf("decode args for tool %q: %w (%w)", t.name, err, ErrToolArgs)
	}
	out, err := t.fn(ctx, in)
	if err != nil {
		return nil, err
	}
	return marshalJournal(out) // not HTML-escaped: the model reads this JSON text as written
}

// argsOptions check tool arguments against the fields schema.For describes: exact names, and
// every field the schema lists as required.
var argsOptions = &strictjson.Options{Fields: strictjson.SchemaFields}

// decodeArgs decodes a tool call's arguments into v (a pointer) strictly, so the value holds
// exactly what the arguments say. It rejects what encoding/json would accept loosely: a missing
// required field (one schema.For lists as required: not a pointer, and no omitempty or omitzero
// in its json tag), a name that is not a field (an unknown name, or a case variant of a field's
// name), a duplicate name, data after the value, invalid UTF-8, and an escaped lone surrogate.
// Empty arguments are the empty object. Func, SubAgent, and RunTyped's final_answer decode their
// arguments with it.
func decodeArgs(args json.RawMessage, v any) error {
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage("{}")
	}
	return strictjson.Unmarshal(args, v, argsOptions)
}
