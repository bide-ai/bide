// Package plan is rung-1 of the expression-surfaces stack: a handle-based flow
// builder that lowers to the journal-backed runtime and adds no new executor.
//
// A caller describes a flow by naming durable steps and wiring them with typed
// handles. Each Builder.Step, Builder.Tool, and Builder.Model returns a
// Handle[I, O] that satisfies Producer[O] and Consumer[I] through unexported
// marker methods, so Builder.Edge and Builder.Switch unify the connecting type
// at compile time and only handles can be endpoints. The wiring is reified into
// an internal spec (nodes, edges, branches) rather than executed eagerly.
//
// Builder.Build lowers that spec onto the existing durable runtime: every node
// becomes an agent.Step keyed by its journal name, and each Switch choice
// becomes its own journaled step recording the taken arm. Flow.Run drives the
// lowered flow sequentially with no goroutines or errgroup, so it inherits the
// substrate's at-most-once, halt, and resume guarantees unchanged. The builder
// is a surface over agent.Step control flow; it introduces no separate
// scheduler or executor.
//
// Builder.Join2 and Builder.Join3 add fixed-arity fan-in: a node that consumes two
// (or three) upstream producers that BOTH execute and merges them, so a fan-out
// (a producer with several outgoing edges) can reconverge. Flow.Run executes the
// reachable DAG in a deterministic topological order, strictly sequentially: a Join
// runs only after all its inputs are journaled, so the fan-in barrier is realized by
// ordering, not concurrency (still no goroutines or errgroup). A Switch still prunes
// to its taken arm; Build rejects a Join whose inputs a Switch can skip (Join is
// fan-in of both-execute branches, not reconvergence of mutually-exclusive arms).
// Join is currently expressible only through the Go builder; the config loader (Load)
// does not yet carry a join wiring element, which is a deferred follow-up.
//
// Because the topology is declared as a value, plan can render the authored
// graph (Flow.RenderMermaid) and compare it against the journal-derived path of
// a run (Flow.Conform), distinguishing the declared diagram from the diagram
// agent.RenderMermaid recovers from the journal.
//
// See docs/design/expression-surfaces.md for the design and the open points
// carried by this rung (arm reconvergence and the no-new-executor invariant).
package plan
