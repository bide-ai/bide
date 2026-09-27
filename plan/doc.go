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
// LoopBack adds a BOUNDED loop: a Switch arm whose target is an earlier loop head
// is a back-edge, and the arm carries a max-iteration bound. The other arm(s) of the
// same Switch are the exit; Build requires one so the loop can terminate. The
// back-edge is excluded from the forward (acyclic) graph, so Kahn still linearizes it
// and every other check is unchanged; Flow.Run re-enters the head per iteration under
// ITERATION-SCOPED journal keys (iter:<n>:<node>), so at-most-once, halt-on-ambiguity,
// and resume hold per iteration exactly as for a linear flow, and a run that would
// exceed the bound errors rather than spinning forever. The loop is still driven by a
// plain sequential Go for-loop: no goroutine, channel, or scheduler. The type frontier
// (the value routed on the back-edge equals the head's input type) is enforced at the
// LoopBack call site by the compiler. Digest commits to the loop STRUCTURE (head,
// switch, body region, and max bound) but never the runtime iteration count; conform
// strips the iteration prefix to map each key back to its declared node. Like Join,
// LoopBack is expressible only through the Go builder; the config loader (Load) does
// not yet carry a loop wiring element, which is a deferred follow-up.
//
// Because the topology is declared as a value, plan can render the authored
// graph (Flow.RenderMermaid) and compare it against the journal-derived path of
// a run (Flow.Conform), distinguishing the declared diagram from the diagram
// agent.RenderMermaid recovers from the journal.
//
// See docs/design/expression-surfaces.md for the design and the open points
// carried by this rung (arm reconvergence and the no-new-executor invariant).
package plan
