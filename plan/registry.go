package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	agent "github.com/dayna/go-agents"
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
}

// regPred is a registered Switch predicate. mType is the switched value's type M
// captured via reflect.TypeFor[M], checked at load against the switched node's
// output type. pred is the erased func(any)bool that asserts v.(M) exactly like
// wiring.go's Switch does; run is always nil for a predicate.
type regPred struct {
	mType reflect.Type
	pred  func(v any) bool
}

// NewRegistry returns a fresh, empty Registry. Every Load takes an explicit
// Registry; there is no shared global, so two loaders never contend over one
// mutable namespace.
func NewRegistry() *Registry {
	return &Registry{
		blocks: make(map[string]*regBlock),
		preds:  make(map[string]*regPred),
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

// RegisterStep registers an arbitrary func(I)(O,error) as a Step block named name,
// inferring I and O from fn (the config never restates types; they flow from the
// registered block). The installed run closure mirrors Builder.Step: it asserts
// the erased input is I, calls fn, and boxes the O result back as any. A duplicate
// name is an error, surfaced at Load and returned here for inline checking.
func RegisterStep[I, O any](r *Registry, name string, fn func(I) (O, error)) error {
	return r.registerBlock(name, &regBlock{
		kind:    kindStep,
		inType:  reflect.TypeFor[I](),
		outType: reflect.TypeFor[O](),
		run: func(_ context.Context, in any) (any, error) {
			typed, ok := in.(I)
			if !ok {
				return nil, fmt.Errorf("plan: step %q got input of type %T, want %s", name, in, reflect.TypeFor[I]())
			}
			out, err := fn(typed)
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
// into O, exactly like Builder.Tool. A duplicate name is an error, surfaced at
// Load and returned here for inline checking.
func RegisterTool[I, O any](r *Registry, name string, t agent.Tool) error {
	return r.registerBlock(name, &regBlock{
		kind:    kindTool,
		inType:  reflect.TypeFor[I](),
		outType: reflect.TypeFor[O](),
		run: func(ctx context.Context, in any) (any, error) {
			typed, ok := in.(I)
			if !ok {
				return nil, fmt.Errorf("plan: tool %q got input of type %T, want %s", name, in, reflect.TypeFor[I]())
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
}

// RegisterModel registers a model call built from prompt as a Model block named
// name. I and O are explicit because a prompt string infers nothing: I is the
// input rendered into the prompt and O is the structured result. It carries rung-1
// stub semantics, mirroring Builder.Model: the node participates in topology,
// name-uniqueness, and type unification, but its run body returns the stub error
// (modelStubMessage) because rung-1 binds no model. A duplicate name is an error,
// surfaced at Load and returned here for inline checking.
func RegisterModel[I, O any](r *Registry, name, prompt string) error {
	_ = prompt // recorded intent; rung-1 does not render it (mirrors Builder.Model)
	return r.registerBlock(name, &regBlock{
		kind:    kindModel,
		inType:  reflect.TypeFor[I](),
		outType: reflect.TypeFor[O](),
		run: func(_ context.Context, _ any) (any, error) {
			var zero O
			return zero, fmt.Errorf("%s", modelStubMessage)
		},
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
