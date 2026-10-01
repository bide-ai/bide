package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/bide-ai/bide/agent"
)

// Registry maps config names to typed Go behavior, so a rung-2 config can
// reference blocks and predicates by name. It is the bridge between a data
// config (which carries no types) and the compile-time-typed rung-1 builder: a
// registered entry captures its I/O reflect.Type so Load can validate the
// topology at load time.
//
// A Registry holds two separate namespaces: blocks (Step/Tool/Model nodes) and
// predicates (Switch arm tests). They are separate because a block lowers to a
// node.run closure while a predicate lowers to a Switch arm's erased func; a name
// may appear once in each namespace without conflict. A Registry is per-Load,
// explicit, and created fresh by NewRegistry; there is no global mutable default.
//
// A Registry is not safe for concurrent registration. Register all behavior on
// one goroutine before calling Load.
type Registry struct {
	blocks map[string]*regBlock
	preds  map[string]*regPred
	merges map[string]*regMerge
	// errs collects duplicate-registration errors so they surface at Load with the
	// config's other drift, rather than being lost at a Register call the caller may
	// not check. Register also returns the error for callers that check inline.
	errs []error
}

// regBlock is a registered Step/Tool/Model. It mirrors the fields of node that a
// block populates: kind, inType/outType captured via reflect.TypeFor, and the
// type-erased run closure Load installs on the built node. pred is always nil for
// a block.
type regBlock struct {
	kind    nodeKind
	inType  reflect.Type
	outType reflect.Type
	run     func(ctx context.Context, in any) (any, error)
	// prompt is the model prompt template of a kindModel block, recorded by
	// RegisterModel and threaded onto the assembled node so a loaded Model node
	// renders identically to a hand-built one. Empty for a Step or Tool block, whose
	// behavior lives in run instead.
	prompt string
	// safety is the retry-on-resume classification the loaded node carries, mirroring
	// Builder-side node.safety. RegisterTool auto-derives it from the wrapped
	// agent.Tool; RegisterStep/RegisterModel take it from a NodeOption at
	// registration. It is threaded into the built node by assemble, so a loaded flow
	// resumes identically to a hand-built one. A config node's "safety" field may
	// keep or lower its retry classification, never raise it, and keeps the approval
	// gate recorded here (see safetyFromConfig).
	safety agent.Safety
	// approval is the approval gate of a wrapped agent tool (its ToolSpec.Approval),
	// carried onto the loaded node so Build refuses it, as for a hand-built node.
	approval *agent.ApprovalPolicy
}

// regPred is a registered Switch predicate. mType is the switched value's type M
// captured via reflect.TypeFor[M], checked at load against the switched node's
// output type. pred is the erased func(any)bool that asserts v.(M) exactly like
// wiring.go's Switch does; run is always nil for a predicate.
type regPred struct {
	mType reflect.Type
	pred  func(v any) bool
}

// regMerge is a registered fan-in merge, the config counterpart of a
// Builder.Join2/Join3 node. arity is the number of ordered inputs (2 for a
// RegisterJoin2, 3 for a RegisterJoin3); inTypes are those inputs' reflect.Types
// in declared order, captured via reflect.TypeFor so assemble can pin
// node.joinInTypes and Build can re-check each input producer's output type by
// identity; outType is the merged result type; merge is the type-erased fan-in
// closure assemble installs on the kindJoin node, mirroring the closure
// Builder.Join2/Join3 build. A merge block is a THIRD Registry namespace,
// separate from blocks and predicates: a name may appear once in each without
// conflict, because a merge lowers to a node's merge closure rather than to a
// block's run or a predicate's arm test.
type regMerge struct {
	// safety is the merge block's retry-on-resume classification, set in Go by the
	// NodeOptions given to RegisterJoin2/RegisterJoin3 (none: a side effect, which halts on
	// an ambiguous crash). A config join's "safety" may only lower it.
	safety  agent.Safety
	arity   int
	inTypes []reflect.Type
	outType reflect.Type
	merge   func(ctx context.Context, inputs []any) (any, error)
}

// NewRegistry returns a fresh, empty Registry. Every Load takes an explicit
// Registry; there is no shared global, so two loaders never contend over one
// mutable namespace.
func NewRegistry() *Registry {
	return &Registry{
		blocks: make(map[string]*regBlock),
		preds:  make(map[string]*regPred),
		merges: make(map[string]*regMerge),
	}
}

// registerBlock installs a block under name, recording a duplicate as an error on
// the Registry (surfaced at Load) and returning it for inline checking. A
// duplicate never silently overwrites the existing entry.
func (r *Registry) registerBlock(name string, b *regBlock) error {
	if _, dup := r.blocks[name]; dup {
		err := fmt.Errorf("plan: duplicate block registration %q", name)
		r.errs = append(r.errs, err)
		return err
	}
	r.blocks[name] = b
	return nil
}

// registerPred installs a predicate under name, recording a duplicate as an error
// on the Registry (surfaced at Load) and returning it for inline checking. A
// duplicate never silently overwrites the existing entry.
func (r *Registry) registerPred(name string, p *regPred) error {
	if _, dup := r.preds[name]; dup {
		err := fmt.Errorf("plan: duplicate predicate registration %q", name)
		r.errs = append(r.errs, err)
		return err
	}
	r.preds[name] = p
	return nil
}

// registerMerge installs a merge block under name, recording a duplicate as an
// error on the Registry (surfaced at Load) and returning it for inline checking. A
// duplicate never silently overwrites the existing entry.
func (r *Registry) registerMerge(name string, m *regMerge) error {
	if _, dup := r.merges[name]; dup {
		err := fmt.Errorf("plan: duplicate merge registration %q", name)
		r.errs = append(r.errs, err)
		return err
	}
	r.merges[name] = m
	return nil
}

// RegisterJoin2 registers a fixed-arity fan-in merge func(ctx, A, B) (O, error) as a
// merge block named name, inferring A, B, and O from fn (the config never restates
// types; they flow from the registered merge). It is the config counterpart of
// Builder.Join2: a "join" wiring element references the merge block by name, names
// its two ordered inputs, and assemble builds a kindJoin node whose ordered input
// types are A, B and whose type-erased merge closure asserts each boxed input to
// its concrete type before calling fn, exactly like Builder.Join2. A duplicate name
// is an error, surfaced at Load and returned here for inline checking.
//
// opts set the merge block's Safety, as for RegisterStep: with none, a loaded join is a side
// effect and halts on an ambiguous crash, like a hand-built Join. Only Go code can mark a merge
// retry-safe (ReadOnly, Idempotent); a config join's "safety" may only lower what opts declare.
func RegisterJoin2[A, B, O any](r *Registry, name string, fn func(context.Context, A, B) (O, error), opts ...NodeOption) error {
	return r.registerMerge(name, &regMerge{
		safety:  safetyFromOptions(agent.Safety{}, opts),
		arity:   2,
		inTypes: []reflect.Type{reflect.TypeFor[A](), reflect.TypeFor[B]()},
		outType: reflect.TypeFor[O](),
		merge: func(ctx context.Context, inputs []any) (any, error) {
			if len(inputs) != 2 {
				return nil, fmt.Errorf("plan: join %q expected 2 inputs, got %d", name, len(inputs))
			}
			av, ok := inputs[0].(A)
			if !ok {
				return nil, fmt.Errorf("plan: join %q input 0 got type %T, want %s", name, inputs[0], reflect.TypeFor[A]())
			}
			bv, ok := inputs[1].(B)
			if !ok {
				return nil, fmt.Errorf("plan: join %q input 1 got type %T, want %s", name, inputs[1], reflect.TypeFor[B]())
			}
			out, err := fn(ctx, av, bv)
			if err != nil {
				return nil, err
			}
			return out, nil
		},
	})
}

// RegisterJoin3 is RegisterJoin2 for arity three: it registers a fan-in merge
// func(ctx, A, B, C) (O, error) as a merge block named name, inferring A, B, C, and O
// from fn. It is the config counterpart of Builder.Join3: a "join" element names
// three ordered inputs and assemble builds a kindJoin node whose ordered input
// types are A, B, C and whose erased merge closure asserts each boxed input before
// calling fn. A duplicate name is an error, surfaced at Load and returned here for
// inline checking. opts set the merge block's Safety, as for RegisterJoin2.
func RegisterJoin3[A, B, C, O any](r *Registry, name string, fn func(context.Context, A, B, C) (O, error), opts ...NodeOption) error {
	return r.registerMerge(name, &regMerge{
		safety:  safetyFromOptions(agent.Safety{}, opts),
		arity:   3,
		inTypes: []reflect.Type{reflect.TypeFor[A](), reflect.TypeFor[B](), reflect.TypeFor[C]()},
		outType: reflect.TypeFor[O](),
		merge: func(ctx context.Context, inputs []any) (any, error) {
			if len(inputs) != 3 {
				return nil, fmt.Errorf("plan: join %q expected 3 inputs, got %d", name, len(inputs))
			}
			av, ok := inputs[0].(A)
			if !ok {
				return nil, fmt.Errorf("plan: join %q input 0 got type %T, want %s", name, inputs[0], reflect.TypeFor[A]())
			}
			bv, ok := inputs[1].(B)
			if !ok {
				return nil, fmt.Errorf("plan: join %q input 1 got type %T, want %s", name, inputs[1], reflect.TypeFor[B]())
			}
			cv, ok := inputs[2].(C)
			if !ok {
				return nil, fmt.Errorf("plan: join %q input 2 got type %T, want %s", name, inputs[2], reflect.TypeFor[C]())
			}
			out, err := fn(ctx, av, bv, cv)
			if err != nil {
				return nil, err
			}
			return out, nil
		},
	})
}

// RegisterStep registers an arbitrary func(ctx, I) (O, error) as a Step block named name,
// inferring I and O from fn (the config never restates types; they flow from the
// registered block). The installed run closure mirrors Builder.Step: it asserts
// the erased input is I, calls fn, and boxes the O result back as any. A duplicate
// name is an error, surfaced at Load and returned here for inline checking.
//
// Pass plan.ReadOnly()/plan.Idempotent() to record the block's retry-on-resume
// Safety in Go (the config JSON carries no Safety); a loaded node then resumes
// identically to one built with Builder.Step and the same option.
func RegisterStep[I, O any](r *Registry, name string, fn func(context.Context, I) (O, error), opts ...NodeOption) error {
	return r.registerBlock(name, &regBlock{
		kind:    kindStep,
		inType:  reflect.TypeFor[I](),
		outType: reflect.TypeFor[O](),
		safety:  safetyFromOptions(agent.Safety{}, opts),
		run: func(ctx context.Context, in any) (any, error) {
			typed, ok := in.(I)
			if !ok {
				return nil, fmt.Errorf("plan: step %q got input of type %T, want %s", name, in, reflect.TypeFor[I]())
			}
			out, err := fn(ctx, typed)
			if err != nil {
				return nil, err
			}
			return out, nil
		},
	})
}

// RegisterTool registers an agent.Tool as a Tool block named name. I and O are
// explicit because an agent.Tool is untyped (json.RawMessage in and out): the I
// input is JSON-encoded into the tool's args and the tool's JSON result is decoded
// into O, exactly like Builder.Tool. A duplicate name, or a tool the agent's
// wrapper check refuses (as agent.New would), is an error, surfaced at Load and
// returned here for inline checking.
//
// Safety AUTO-DERIVES from the tool's spec (agent.SpecOf), mirroring Builder.Tool; an explicit
// plan.ReadOnly()/plan.Idempotent() option overrides the derived Safety. Safety is
// recorded in Go here, not in the config JSON.
func RegisterTool[I, O any](r *Registry, name string, t agent.Tool, opts ...NodeOption) error {
	spec := agent.SpecOf(t) // read once, as the agent reads it
	if err := checkTool(t); err != nil {
		// Recorded on the Registry, as a duplicate is, so Load reports it to a caller that did
		// not check this return.
		err = fmt.Errorf("plan: RegisterTool %q: %w", name, err)
		r.errs = append(r.errs, err)
		return err
	}
	return r.registerBlock(name, &regBlock{
		kind:     kindTool,
		inType:   reflect.TypeFor[I](),
		outType:  reflect.TypeFor[O](),
		safety:   safetyFromOptions(spec.Safety, opts),
		approval: spec.Approval,
		run: func(ctx context.Context, in any) (any, error) {
			typed, ok := in.(I)
			if !ok {
				return nil, fmt.Errorf("plan: tool %q got input of type %T, want %s", name, in, reflect.TypeFor[I]())
			}
			args, err := json.Marshal(typed)
			if err != nil {
				return nil, fmt.Errorf("plan: tool %q encode input: %w", name, err)
			}
			raw, err := callTool(ctx, t, spec.Timeout, args)
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
}

// RegisterModel registers a model call built from prompt as a Model block named
// name. I and O are explicit because a prompt string infers nothing: I is the input
// rendered into the prompt and O is the structured result. It mirrors Builder.Model:
// at run time the loaded node renders prompt as a Go text/template with the decoded
// input I as data, calls the model bound to the loaded flow, and decodes the model's
// text response as JSON into O. A duplicate name is an error, surfaced at Load and
// returned here for inline checking.
//
// The model is bound to the loaded flow with WithLoadedModel at Load time (a Registry
// carries no model); a config that declares a Model block loaded without a bound
// model fails Build, naming the node. O must be a JSON-shaped type.
//
// Pass plan.ReadOnly()/plan.Idempotent() to record the block's retry-on-resume Safety
// in Go (the config JSON carries no Safety), mirroring Builder.Model; the default
// keeps the conservative halt-on-ambiguous-crash for a non-idempotent model call.
func RegisterModel[I, O any](r *Registry, name, prompt string, opts ...NodeOption) error {
	return r.registerBlock(name, &regBlock{
		kind:    kindModel,
		inType:  reflect.TypeFor[I](),
		outType: reflect.TypeFor[O](),
		safety:  safetyFromOptions(agent.Safety{}, opts),
		prompt:  prompt,
		// run is nil for a Model block: runNode dispatches a kindModel node to runModel,
		// which reads the flow's bound model at run time (see Builder.Model / WithModel).
	})
}

// RegisterPredicate registers a Switch predicate func(M)bool named name, capturing
// M via reflect.TypeFor[M] so Load can verify M equals the switched node's output
// type. The stored predicate is erased to func(any)bool with a v.(M) assertion
// exactly like wiring.go's Switch: a wrong dynamic type means the arm did not
// match. A duplicate name is an error, surfaced at Load and returned here for
// inline checking.
func RegisterPredicate[M any](r *Registry, name string, pred func(M) bool) error {
	return r.registerPred(name, &regPred{
		mType: reflect.TypeFor[M](),
		pred: func(v any) bool {
			typed, ok := v.(M)
			if !ok {
				return false
			}
			return pred(typed)
		},
	})
}
