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
// The func body is the escape hatch: a non-idempotent side effect inside it must
// declare its own Safety or wrap its own agent.Do, because a plain Step carries no
// Safety classification of its own. See docs/design/expression-surfaces.md.
func (b *Builder[In, Out]) Step[I, O any](name string, fn func(I) (O, error)) Handle[I, O] {
	b.core.register(&node{
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
	})
	return Handle[I, O]{name: name, b: b.core}
}

// Tool registers an agent.Tool as a durable step named name. name must be unique
// across the flow; a duplicate is recorded as a deferred error surfaced at Build.
// Tool infers I and O: the I input is JSON-encoded into the tool's args and the
// tool's JSON result is decoded into O.
//
// Safety follow-up (rung-1): the frozen node spec (plan/spec.go, Agent A) carries
// no Safety field, so this lowering cannot yet forward t.Safety() to the journal
// boundary. Rung-1 therefore lowers a Tool as a plain step; the tool still runs,
// but its retry-on-resume classification is not propagated. Threading Safety
// through Build (so a destructive tool wraps its own agent.Do) is deferred: it
// needs a `safety agent.Safety` field on node, which is Agent A's scaffold to
// extend and Agent D's Build to honor. Reported as an out-of-scope dependency.
func (b *Builder[In, Out]) Tool[I, O any](name string, t agent.Tool) Handle[I, O] {
	b.core.register(&node{
		name:    name,
		kind:    kindTool,
		inType:  typeOf[I](),
		outType: typeOf[O](),
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
	})
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
func (b *Builder[In, Out]) Model[I, O any](name, prompt string) Handle[I, O] {
	_ = prompt // recorded intent; rung-1 does not render it (see doc comment)
	b.core.register(&node{
		name:    name,
		kind:    kindModel,
		inType:  typeOf[I](),
		outType: typeOf[O](),
		run: func(_ context.Context, _ any) (any, error) {
			var zero O
			return zero, fmt.Errorf("%s", modelStubMessage)
		},
	})
	return Handle[I, O]{name: name, b: b.core}
}
