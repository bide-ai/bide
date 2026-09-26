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
// Because the topology is declared as a value, plan can render the authored
// graph (Flow.RenderMermaid) and compare it against the journal-derived path of
// a run (Flow.Conform), distinguishing the declared diagram from the diagram
// agent.RenderMermaid recovers from the journal.
//
// See docs/design/expression-surfaces.md for the design and the open points
// carried by this rung (arm reconvergence and the no-new-executor invariant).
package plan
