package plan

import (
	"context"
	"reflect"

	"github.com/bide-ai/bide/agent"
)

// nodeKind classifies a lowered step for rendering and validation.
type nodeKind int

const (
	kindStep   nodeKind = iota // arbitrary func(context.Context, I) (O, error)
	kindTool                   // an agent.Tool
	kindModel                  // a model call built from a prompt
	kindSwitch                 // a journaled branch choice
	kindJoin                   // a fixed-arity fan-in that merges several producers
)

// node is one declared step in the topology. inType/outType are reflect.Type
// captured at construction so Build can validate whole-graph coherence and
// RenderMermaid can label each node.
type node struct {
	name string
	// block names the behaviour the node runs: the registered block (or merge block) a
	// config node or join references, or, for a node built in Go, the name given with
	// BlockName, defaulting to the node's own name (register fills it in). Digest commits
	// to it, so a config that points a node at another block with the same types
	// describes a different flow.
	block   string
	kind    nodeKind
	inType  reflect.Type
	outType reflect.Type
	// run is the type-erased lowering closure the constructor installs; Build
	// wraps it in an agent.Step call keyed by name. It takes the decoded input
	// (as any) and returns the output (as any) or an error. nil for kindSwitch.
	run func(ctx context.Context, in any) (any, error)
	// safety is the node's retry-on-resume classification, mirroring agent.Safety
	// for a core tool. The zero value (no ReadOnly/Idempotent)
	// is the conservative default: on an ambiguous mid-node crash the node HALTS
	// rather than re-run, preserving the surface's at-most-once-by-default. A node
	// marked retry-safe (agent.Safety.RetrySafe) instead re-runs its body from the
	// top on resume (see runNode). It is a
	// runtime resume property, not part of the wired topology, so it deliberately
	// does NOT participate in Digest (a flow's identity is its shape).
	safety agent.Safety
	// approval is the approval gate a wrapped agent tool (its ToolSpec.Approval) or a config
	// "approval" block declares. Build refuses a flow with one, since flows do not enforce it.
	approval *agent.ApprovalPolicy
	// prompt is the raw prompt template of a kindModel node, recorded at
	// construction. It is a Go text/template rendered with the decoded input I as
	// data at run time, so {{.Field}} references the input's fields. It is empty
	// for every other kind. runModel reads it; the bound model (builderCore.model)
	// supplies the call. It is not part of Digest (the digest commits to topology,
	// not to a node's prompt text, mirroring how a Step's body is not hashed).
	prompt string
	// joinInputs are the ORDERED source node names a kindJoin node fans in, one per
	// merge-function parameter (Join2 records two, Join3 three). They are the join's
	// input ports in declared order, so the merge closure receives its arguments in
	// the same order the author wrote them. Empty for every other kind. The join also
	// records one edge per input (source -> join name) so the executor and reachability
	// walk treat the fan-in as ordinary edges; joinInputs additionally pins the ORDER,
	// which a plain edge set does not. It participates in Digest so a diamond's shape is
	// stable and distinct from a differently-ordered or differently-shaped merge.
	joinInputs []string
	// joinInTypes are the reflect.Type of each join input port, positionally aligned
	// with joinInputs, captured from the merge function's parameter types at
	// construction (A, B for Join2). Build checks each equals the corresponding input
	// producer's output type by reflect identity, mirroring how Edge unifies M. Empty
	// for every other kind. It participates in Digest.
	joinInTypes []reflect.Type
	// merge is the type-erased fan-in closure a kindJoin node installs: it receives the
	// decoded inputs boxed as any, positionally aligned with joinInputs/joinInTypes, and
	// returns the merged output boxed as any (or an error). runNode dispatches a kindJoin
	// node to it instead of node.run (which is nil for a join). It is nil for every other
	// kind. Like run it is not hashed: the digest commits to the join's shape (its ordered
	// inputs and their types plus its own name and output type), not to the merge body.
	merge func(ctx context.Context, inputs []any) (any, error)
}

// edge is a declared connection producer.Out -> consumer.In (names, not values).
type edge struct{ from, to string }

// arm is a resolved Switch arm: a predicate over the switched node's output
// (nil for Else) and the target node name. Stored type-erased; the typed
// When/Else/LoopBack builders populate these.
//
// A LoopBack arm additionally sets loopBack and loopMax: its target is the loop
// HEAD (an earlier node), so the arm is a bounded back-edge rather than a forward
// route. loopMax is the maximum number of times the loop body may re-enter the
// head before Run declares a runaway loop and errors; it is > 0 for a loop-back
// arm and 0 otherwise. The back-edge is deliberately excluded from the forward
// (acyclic) graph topoOrder and reachability walk, so Kahn still works; Run
// re-enters the head per iteration under iteration-scoped journal keys. Build
// validates the target is an ancestor (a real cycle) and that the Switch also has
// a non-loop-back exit arm.
type arm struct {
	isElse bool
	pred   func(v any) bool
	// predName names the predicate: the registered predicate a config arm references, or
	// the name given with Arm.Named in Go ("" when none was given, and for Else). Digest
	// commits to it, so a config that swaps one predicate for another describes a
	// different flow.
	predName string
	target   string
	loopBack bool // true iff this arm is a bounded back-edge to an earlier loop head
	loopMax  int  // for a loop-back arm, the max iterations before Run errors (> 0); 0 otherwise
}

// branch is a declared Switch: the switched-over producer node and its ordered arms.
type branch struct {
	over string
	arms []arm
}

// builderCore is the mutable spec a Builder accumulates and Build freezes. It
// is the reified value RenderMermaid walks. Handle holds a *builderCore
// back-reference.
type builderCore struct {
	flowName        string
	entry           string       // set to the first constructed node's name (entry step)
	inType, outType reflect.Type // pinned by New[In,Out]
	nodes           []*node
	byName          map[string]*node
	edges           []edge
	branches        []branch
	errs            []error // deferred construction errors surfaced at Build
	// model is the agent.Model bound to this flow via WithModel. A kindModel node
	// reads it at RUN time (it is not known at node construction); nil means no
	// model was bound. Build rejects a flow that declares a Model node with no
	// bound model, naming the node. It is a runtime binding, not part of the wired
	// topology, so it is NOT part of Digest.
	model agent.Model
	// loops are the bounded loops Build derived from the LoopBack arms (see
	// loopSpec). They are populated by Build (deriveLoops) and carried onto the
	// sealed flow so Run can drive each loop region, and digest/render/conform can
	// commit to and recognise the loop structure. Empty for an acyclic flow. The
	// loop STRUCTURE (head, switch, body region, and the max bound) participates in
	// Digest; the runtime iteration count does not (it is runtime, like Safety).
	loops []loopSpec
}

// loopSpec is one bounded loop Build derived from a LoopBack arm: the loop head
// (the back-edge target, an earlier node), the loop switch (the node the Switch is
// over, which carries the back-edge arm), the exit arm target (the first
// non-loop-back arm), the ordered body region (head..switch inclusive, a
// contiguous interval of the forward topoOrder), and the iteration bound. Run
// drives the region iteratively under iteration-scoped journal keys; Build
// validates the back-edge is a real cycle (the switch is reachable from the head in
// the forward graph), that an exit arm exists, and that the region is a contiguous
// interval so the executor stays a simple sequential sweep.
type loopSpec struct {
	head    string   // the back-edge target; the loop head re-entered each iteration
	over    string   // the switched node carrying the back-edge arm (the loop switch)
	body    []string // head..switch inclusive, in forward topoOrder (a contiguous interval)
	max     int      // maximum iterations before Run errors (runaway guard); > 0
	headIdx int      // index of head in the forward topoOrder
	overIdx int      // index of the loop switch in the forward topoOrder
}

// typeOf captures the reflect.Type of T for the constructors to record I/O types.
func typeOf[T any]() reflect.Type { return reflect.TypeFor[T]() }
