package plan

import (
	"context"
	"encoding/json"
	"fmt"

	agent "github.com/dayna/go-agents"
)

// New constructs a flow builder whose input is In and output is Out, both pinned
// here so the flow boundary is checked at construction. name is the flow's label
// (used by Flow.RenderMermaid and conformance). Build later validates that the
// entry step consumes In and every terminal path produces Out.
//
// The first constructor call on the returned Builder (Step, Tool, or Model) sets
// the entry step; New itself records no node.
func New[In, Out any](name string) *Builder[In, Out] {
	return &Builder[In, Out]{core: &builderCore{
		flowName: name,
		inType:   typeOf[In](),
		outType:  typeOf[Out](),
		byName:   make(map[string]*node),
	}}
}

// Builder[In, Out] accumulates a flow spec. In and Out are the pinned flow
// boundary types; the generic-method constructors (Step, Tool, Model) and wiring
// calls (Edge, Switch) mutate the wrapped *builderCore. Construction is pure: it
// records the topology and installs type-erased lowering closures, and runs no
// step. Build freezes the spec into a *Flow.
type Builder[In, Out any] struct {
	core *builderCore
}

// NodeOption configures a node at construction (Step, Tool, Model) or at
// registration (RegisterStep, RegisterTool, RegisterModel). The only property it
// currently sets is the node's retry-on-resume Safety, mirroring agent.Safety for
// a core tool. Options apply in order; a later option overrides an earlier one,
// which is why an explicit ReadOnly/Idempotent on a Tool overrides the Safety
// auto-derived from the wrapped agent.Tool.
type NodeOption func(*node)

// ReadOnly marks a node as read-only: it has no external side effect, so it is
// always safe to re-run from the top on resume. On an ambiguous mid-node crash
// (an attempt marker with no result) Run RE-RUNS the body rather than halting.
// Use it for a node that only reads.
func ReadOnly() NodeOption {
	return func(n *node) { n.safety = agent.Safety{ReadOnly: true} }
}

// Idempotent marks a node as idempotent: it mutates state but a repeat with the
// same input is a no-op downstream, so it is safe to retry. On an ambiguous
// mid-node crash Run RE-RUNS the body rather than halting. Use it for a node whose
// effect de-duplicates downstream (for example an upsert keyed by a stable id).
func Idempotent() NodeOption {
	return func(n *node) { n.safety = agent.Safety{Idempotent: true} }
}

// Retryable is an alias for Idempotent, reading more naturally at some call sites
// (a node the author asserts is safe to retry on resume). It sets the same Safety.
func Retryable() NodeOption { return Idempotent() }

// nodeRetriableOnResume reports whether a node may be safely re-run when a resume
// finds an attempt marker but no result. It mirrors EXACTLY how the core loop
// classifies a retry-safe step (agent.Safety.retriableOnResume, which is
// ReadOnly || Idempotent || IdempotencyKey != nil): a retry-safe node re-runs its
// body from the top; anything else HALTS for out-of-band confirmation. The
// classification lives here because the core's method is unexported; the fields it
// reads are the exported agent.Safety fields, so the two stay in lockstep.
func nodeRetriableOnResume(s agent.Safety) bool {
	return s.ReadOnly || s.Idempotent || s.IdempotencyKey != nil
}

// applyNodeOptions applies opts to n in order (a later option wins), then returns
// n so a constructor can inline it into register. It is the single place both the
// builder constructors and the registry share, so their Safety handling is identical.
func applyNodeOptions(n *node, opts []NodeOption) *node {
	for _, opt := range opts {
		if opt != nil {
			opt(n)
		}
	}
	return n
}

// safetyFromOptions resolves opts to the agent.Safety they set, starting from base
// (a Tool's auto-derived Safety, or the zero value for a Step/Model). It applies
// opts to a scratch node so the registry gets exactly the same Safety a builder
// constructor would, keeping the two paths in lockstep.
func safetyFromOptions(base agent.Safety, opts []NodeOption) agent.Safety {
	return applyNodeOptions(&node{safety: base}, opts).safety
}

// register appends n to the core, enforcing name uniqueness. A duplicate name is
// recorded as a deferred error on core.errs (surfaced at Build) rather than
// panicking, so authoring never aborts mid-construction. The first registered
// node becomes the entry step.
func (c *builderCore) register(n *node) {
	if _, dup := c.byName[n.name]; dup {
		c.errs = append(c.errs, fmt.Errorf("plan: duplicate step name %q", n.name))
		return
	}
	if c.entry == "" {
		c.entry = n.name
	}
	c.byName[n.name] = n
	c.nodes = append(c.nodes, n)
}

// Step registers an arbitrary func(I)(O,error) as a durable step named name. name
// must be unique across the flow; a duplicate is recorded as a deferred error
// surfaced at Build. Step infers I and O from fn.
//
// The func body is the escape hatch: arbitrary Go. Run drives every Step under an
// at-most-once guard (an attempt marker written before the body, the result after),
// so a crash whose outcome was never recorded HALTS the run (*HaltAmbiguous) rather
// than re-firing the body. A non-idempotent side effect is therefore safe by default,
// with no per-step opt-in. See docs/design/expression-surfaces.md.
//
// Pass plan.ReadOnly() or plan.Idempotent() (or the plan.Retryable() alias) to opt
// a node OUT of that halt: a retry-safe node RE-RUNS its body from the top on an
// ambiguous mid-node crash instead of halting. Omit the option to keep the
// conservative halt behavior unchanged.
func (b *Builder[In, Out]) Step[I, O any](name string, fn func(I) (O, error), opts ...NodeOption) Handle[I, O] {
	b.core.register(applyNodeOptions(&node{
		name:    name,
		kind:    kindStep,
		inType:  typeOf[I](),
		outType: typeOf[O](),
		// Decode the type-erased input as I, call fn, box the O result back as any.
		// A wrong dynamic type is a construction-vs-wiring bug and surfaces here as
		// an error rather than a panic.
		run: func(_ context.Context, in any) (any, error) {
			typed, ok := in.(I)
			if !ok {
				return nil, fmt.Errorf("plan: step %q got input of type %T, want %s", name, in, typeOf[I]())
			}
			out, err := fn(typed)
			if err != nil {
				return nil, err
			}
			return out, nil
		},
	}, opts))
	return Handle[I, O]{name: name, b: b.core}
}

// Tool registers an agent.Tool as a durable step named name. name must be unique
// across the flow; a duplicate is recorded as a deferred error surfaced at Build.
// Tool infers I and O: the I input is JSON-encoded into the tool's args and the
// tool's JSON result is decoded into O.
//
// Safety AUTO-DERIVES from the wrapped agent.Tool: Tool records t.Safety() on the
// node, so a tool the core classifies as retry-safe (ReadOnly, Idempotent, or
// carrying an IdempotencyKey) RE-RUNS on an ambiguous mid-node crash while a
// non-idempotent tool HALTS, matching the core loop's own resume decision. An
// explicit plan.ReadOnly()/plan.Idempotent() option OVERRIDES the derived Safety
// (options apply after the literal), for the rare case the author knows better
// than the tool's own declaration.
func (b *Builder[In, Out]) Tool[I, O any](name string, t agent.Tool, opts ...NodeOption) Handle[I, O] {
	b.core.register(applyNodeOptions(&node{
		name:    name,
		kind:    kindTool,
		inType:  typeOf[I](),
		outType: typeOf[O](),
		safety:  t.Safety(), // auto-derived; an explicit option below overrides it
		run: func(ctx context.Context, in any) (any, error) {
			typed, ok := in.(I)
			if !ok {
				return nil, fmt.Errorf("plan: tool %q got input of type %T, want %s", name, in, typeOf[I]())
			}
			args, err := json.Marshal(typed)
			if err != nil {
				return nil, fmt.Errorf("plan: tool %q encode input: %w", name, err)
			}
			raw, err := t.Call(ctx, args)
			if err != nil {
				return nil, err
			}
			var out O
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &out); err != nil {
					return nil, fmt.Errorf("plan: tool %q decode result: %w", name, err)
				}
			}
			return out, nil
		},
	}, opts))
	return Handle[I, O]{name: name, b: b.core}
}

// modelStubMessage is the error a rung-1 Model step returns when run. Model is a
// declared node so it renders and validates, but rung-1 does not bind a model
// call; see the Model doc comment.
const modelStubMessage = "plan: Model requires a bound model (rung-1 stub)"

// Model registers a model call built from prompt as a durable step named name.
// name must be unique across the flow; a duplicate is recorded as a deferred error
// surfaced at Build. I and O are explicit because a prompt string infers nothing:
// I is the input rendered into the prompt and O is the structured result.
//
// rung-1 shape (documented open design point): binding a model call would need a
// model supplied on the Builder plus prompt rendering and structured decoding of
// agent.Generate output, which is beyond the rung-1 budget. So rung-1 lowers Model
// to a declared step whose body returns a stub error (modelStubMessage). The node
// still participates in topology rendering, name-uniqueness, and type unification;
// only its run body is a stub. Binding a real model (a WithModel Builder option
// feeding agent.Generate) is deferred to a later rung.
//
// Pass plan.ReadOnly() or plan.Idempotent() to opt a Model node into re-run on an
// ambiguous mid-node crash (a model generation is typically read-only). Omit the
// option to keep the conservative halt behavior.
func (b *Builder[In, Out]) Model[I, O any](name, prompt string, opts ...NodeOption) Handle[I, O] {
	_ = prompt // recorded intent; rung-1 does not render it (see doc comment)
	b.core.register(applyNodeOptions(&node{
		name:    name,
		kind:    kindModel,
		inType:  typeOf[I](),
		outType: typeOf[O](),
		run: func(_ context.Context, _ any) (any, error) {
			var zero O
			return zero, fmt.Errorf("%s", modelStubMessage)
		},
	}, opts))
	return Handle[I, O]{name: name, b: b.core}
}
