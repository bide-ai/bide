package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/bide-ai/bide/agent"
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

// BlockName names the behaviour a node built in Go runs, the way a config node names its
// registered block. Digest commits to it; without it a node's block is its own name. Give a
// node the block name its config counterpart references (for a join, the merge block) so the
// Go flow and the config flow have one digest. A Registry ignores it: a registered block's
// name is the one it was registered under.
func BlockName(name string) NodeOption {
	return func(n *node) { n.block = name }
}

// ReadOnly marks a node as read-only: it has no external side effect, so it is
// always safe to re-run from the top on resume. On an ambiguous mid-node crash
// (an attempt marker with no result) Run RE-RUNS the body rather than halting.
// Use it for a node that only reads.
//
// It sets only the retry classification: an approval gate or IdempotencyKey the node already
// carries (a wrapped agent tool's) is kept, so the option cannot switch a gate off.
func ReadOnly() NodeOption {
	return func(n *node) { n.safety.ReadOnly, n.safety.Idempotent = true, false }
}

// Idempotent marks a node as idempotent: it mutates state but a repeat with the
// same input is a no-op downstream, so it is safe to retry. On an ambiguous
// mid-node crash Run RE-RUNS the body rather than halting. Use it for a node whose
// effect de-duplicates downstream (for example an upsert keyed by a stable id).
// Like ReadOnly, it sets only the retry classification and keeps any approval gate or
// IdempotencyKey the node already carries.
func Idempotent() NodeOption {
	return func(n *node) { n.safety.ReadOnly, n.safety.Idempotent = false, true }
}

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

// register appends n to the core, enforcing name uniqueness.
// A bad name is recorded as a deferred error on core.errs (surfaced at Build) rather
// than panicking, so authoring never aborts mid-construction. The first registered
// node becomes the entry step.
//
// A name checkStepName refuses is recorded the same way.
func (c *builderCore) register(n *node) {
	if err := checkStepName(n.name); err != nil {
		c.errs = append(c.errs, fmt.Errorf("plan: %w", err))
		return
	}
	if _, dup := c.byName[n.name]; dup {
		c.errs = append(c.errs, fmt.Errorf("plan: duplicate step name %q", n.name))
		return
	}
	if c.entry == "" {
		c.entry = n.name
	}
	if n.block == "" {
		n.block = n.name // a node built in Go with no BlockName is its own block
	}
	c.byName[n.name] = n
	c.nodes = append(c.nodes, n)
}

// checkStepName refuses a step name with a ':'. A node's name is its journal key, and Run
// derives every other key it writes with ':' ("attempt:<key>", "iter:<n>:<node>",
// "switch:<node>", "flow:digest"), so such a name could name another step's record.
func checkStepName(name string) error {
	if strings.ContainsRune(name, ':') {
		return fmt.Errorf("step name %q contains ':', which Run reserves for the journal keys it derives", name)
	}
	return nil
}

// Step registers an arbitrary func(ctx, I) (O, error) as a durable step named name. name
// must be unique across the flow; a duplicate is recorded as a deferred error
// surfaced at Build. Step infers I and O from fn.
//
// The func body is the escape hatch: arbitrary Go. Run drives every Step under an
// at-most-once guard (an attempt marker written before the body, the result after),
// so a crash whose outcome was never recorded HALTS the run (*HaltAmbiguous) rather
// than re-firing the body. A non-idempotent side effect is therefore safe by default,
// with no per-step opt-in. See docs/guides/flows.md.
//
// Pass plan.ReadOnly() or plan.Idempotent() to opt
// a node OUT of that halt: a retry-safe node RE-RUNS its body from the top on an
// ambiguous mid-node crash instead of halting. Omit the option to keep the
// conservative halt behavior unchanged.
//
// A Step is non-retry-safe by DEFAULT, so this applies even to a step with no side
// effect: a pure-compute or read-only step still HALTS on an ambiguous crash unless
// annotated. Mark such steps plan.ReadOnly() (or plan.Idempotent()) so they re-run on
// resume instead of stalling the flow; reserve the default halt for steps whose body
// must not repeat.
func (b *Builder[In, Out]) Step[I, O any](name string, fn func(context.Context, I) (O, error), opts ...NodeOption) Handle[I, O] {
	b.core.register(applyNodeOptions(&node{
		name:    name,
		kind:    kindStep,
		inType:  typeOf[I](),
		outType: typeOf[O](),
		// Decode the type-erased input as I, call fn, box the O result back as any.
		// A wrong dynamic type is a construction-vs-wiring bug and surfaces here as
		// an error rather than a panic.
		run: func(ctx context.Context, in any) (any, error) {
			typed, ok := in.(I)
			if !ok {
				return nil, fmt.Errorf("plan: step %q got input of type %T, want %s", name, in, typeOf[I]())
			}
			out, err := fn(ctx, typed)
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

// Join2 registers a fixed-arity fan-in named name that consumes two upstream
// producers a and b and merges them with fn. It is the rung-1 way to reconverge
// two branches that BOTH execute (for example a fan-out X -> {Y, Z} whose results
// Join2(Y, Z) merges), as opposed to a Switch, whose arms are mutually exclusive.
//
// The input types are unified at the call site through the generic parameters,
// exactly as Edge unifies M: a is a Producer[A] and b a Producer[B], so the
// merge fn func(context.Context, A, B) (O, error) receives the two producers' outputs in order and
// a mismatch does not compile. Join2 wires one input edge per producer (a -> name,
// b -> name) and records the ordered input source names and their reflect types, so
// Build can re-check the port types by reflect identity and Run can decode each
// journaled input into its concrete Go type before calling fn.
//
// The returned Handle names the join and produces O, so it composes downstream
// (Edge from it, Switch over it, or as an input to another Join). Its input type
// parameter is O and carries no meaning: a join has several inputs, wired through
// its arguments rather than through a single Edge, so nothing consumes the handle's
// Consumer side.
//
// SAFETY: like a Step or Model, a Join defaults to the conservative
// halt-on-ambiguous-crash (its merge fn may have a side effect). Because a join
// runs only AFTER all its inputs are journaled (Run executes the reachable DAG in
// topological order), the merge is a plain sequential step under the same
// attempt/result guard as every other node; the fan-in is a topological barrier,
// not concurrency. Pass plan.ReadOnly()/plan.Idempotent() to
// opt the join into re-run on an ambiguous crash instead of halting.
func (b *Builder[In, Out]) Join2[A, B, O any](name string, a Producer[A], bb Producer[B], fn func(context.Context, A, B) (O, error), opts ...NodeOption) Handle[O, O] {
	aName, bName := endpointName(a), endpointName(bb)
	b.core.register(applyNodeOptions(&node{
		name:        name,
		kind:        kindJoin,
		outType:     typeOf[O](),
		joinInputs:  []string{aName, bName},
		joinInTypes: []reflect.Type{typeOf[A](), typeOf[B]()},
		merge: func(ctx context.Context, inputs []any) (any, error) {
			if len(inputs) != 2 {
				return nil, fmt.Errorf("plan: join %q expected 2 inputs, got %d", name, len(inputs))
			}
			av, ok := inputs[0].(A)
			if !ok {
				return nil, fmt.Errorf("plan: join %q input 0 got type %T, want %s", name, inputs[0], typeOf[A]())
			}
			bv, ok := inputs[1].(B)
			if !ok {
				return nil, fmt.Errorf("plan: join %q input 1 got type %T, want %s", name, inputs[1], typeOf[B]())
			}
			out, err := fn(ctx, av, bv)
			if err != nil {
				return nil, err
			}
			return out, nil
		},
	}, opts))
	b.core.edges = append(b.core.edges, edge{from: aName, to: name}, edge{from: bName, to: name})
	return Handle[O, O]{name: name, b: b.core}
}

// Join3 is Join2 for arity three: it consumes three upstream producers a, b, c and
// merges them with fn func(context.Context, A, B, C) (O, error). Everything else matches Join2: the
// input types unify at the call site, it wires one input edge per producer in order,
// records the ordered input source names and their reflect types for Build to
// re-check, defaults to halt-on-ambiguous-crash, and returns a Handle producing O.
func (b *Builder[In, Out]) Join3[A, B, C, O any](name string, a Producer[A], bb Producer[B], cc Producer[C], fn func(context.Context, A, B, C) (O, error), opts ...NodeOption) Handle[O, O] {
	aName, bName, cName := endpointName(a), endpointName(bb), endpointName(cc)
	b.core.register(applyNodeOptions(&node{
		name:        name,
		kind:        kindJoin,
		outType:     typeOf[O](),
		joinInputs:  []string{aName, bName, cName},
		joinInTypes: []reflect.Type{typeOf[A](), typeOf[B](), typeOf[C]()},
		merge: func(ctx context.Context, inputs []any) (any, error) {
			if len(inputs) != 3 {
				return nil, fmt.Errorf("plan: join %q expected 3 inputs, got %d", name, len(inputs))
			}
			av, ok := inputs[0].(A)
			if !ok {
				return nil, fmt.Errorf("plan: join %q input 0 got type %T, want %s", name, inputs[0], typeOf[A]())
			}
			bv, ok := inputs[1].(B)
			if !ok {
				return nil, fmt.Errorf("plan: join %q input 1 got type %T, want %s", name, inputs[1], typeOf[B]())
			}
			cv, ok := inputs[2].(C)
			if !ok {
				return nil, fmt.Errorf("plan: join %q input 2 got type %T, want %s", name, inputs[2], typeOf[C]())
			}
			out, err := fn(ctx, av, bv, cv)
			if err != nil {
				return nil, err
			}
			return out, nil
		},
	}, opts))
	b.core.edges = append(b.core.edges,
		edge{from: aName, to: name}, edge{from: bName, to: name}, edge{from: cName, to: name})
	return Handle[O, O]{name: name, b: b.core}
}

// WithModel binds an agent.Model to the flow, so a Model step can render its prompt,
// call the model, and decode the structured result. The binding is read at RUN time
// by every Model node (a Model node does not know the model at construction), so one
// bound model serves the whole flow. A later WithModel overrides an earlier one (last
// write wins). Build rejects a flow that declares a Model step with no bound model,
// naming the node. Returns the builder for chaining:
// New[I,O](name).WithModel(m). It binds identically to a flow produced by Load, so a
// config-loaded flow with a Model node runs once a model is bound.
func (b *Builder[In, Out]) WithModel(m agent.Model) *Builder[In, Out] {
	b.core.model = m
	return b
}

// Model registers a model call built from prompt as a durable step named name. name
// must be unique across the flow; a duplicate is recorded as a deferred error
// surfaced at Build. I and O are explicit because a prompt string infers nothing: I
// is the input rendered into the prompt and O is the structured result.
//
// At run time the node (see runModel): renders prompt as a Go text/template with the
// decoded input I as its data (so {{.Field}} references the input's fields), sends
// the rendered text as a single user message to the model bound with WithModel under
// a JSON-schema response format derived from O, then decodes the model's text
// response into O. The model must therefore be bound with Builder.WithModel before
// Build; a flow declaring a Model step with no bound model fails Build, naming the
// node. O must be a JSON-shaped type (a struct is the usual structured-output shape),
// because the model's response is decoded as JSON into O.
//
// SAFETY: a model call is non-idempotent by default (it may cost tokens and its
// output can vary between calls), so a Model node keeps the conservative
// halt-on-ambiguous-crash default: on a mid-node crash whose result was lost, Run
// HALTS rather than re-call the model. Pass plan.ReadOnly() or plan.Idempotent()
// only if you know a re-call is safe; that opts the node
// into re-run on the ambiguous crash instead of halting.
func (b *Builder[In, Out]) Model[I, O any](name, prompt string, opts ...NodeOption) Handle[I, O] {
	b.core.register(applyNodeOptions(&node{
		name:    name,
		kind:    kindModel,
		inType:  typeOf[I](),
		outType: typeOf[O](),
		prompt:  prompt,
		// run is nil for a kindModel node: runNode dispatches to runModel, which reads
		// the flow's bound model at run time (it is not known here at construction).
	}, opts))
	return Handle[I, O]{name: name, b: b.core}
}
