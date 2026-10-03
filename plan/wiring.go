package plan

import "fmt"

// named is the minimal shape wiring needs from an endpoint: its durable journal
// key. Handle[I,O] satisfies it through its exported Name method, and Handle is
// the only type that satisfies Producer[M]/Consumer[M] (their markers are
// unexported), so a Producer/Consumer is always a Handle and always named. This
// unexported interface lets wiring recover the endpoint name without touching the
// frozen handle.go scaffold.
type named interface{ Name() string }

// endpointName extracts the journal key of a wiring endpoint. It panics only on a
// programmer error that the type system already forbids: a Producer/Consumer that
// is not a Handle cannot be constructed, because the marker methods are
// unexported to this package.
func endpointName(v any) string {
	n, ok := v.(named)
	if !ok {
		panic(fmt.Sprintf("plan: wiring endpoint %T does not name a step (only Handle is a valid endpoint)", v))
	}
	return n.Name()
}

// Edge connects a producer to a consumer, unifying the connecting type M at the
// call site: the producer's output type and the consumer's input type must both be
// M, so a mismatch does not compile and the compiler error names the handles, not
// an erased spec entry. Edge records the endpoint names only; the values flow
// through the journal at run time.
func (b *Builder[In, Out]) Edge[M any](from Producer[M], to Consumer[M]) {
	b.core.edges = append(b.core.edges, edge{
		from: endpointName(from),
		to:   endpointName(to),
	})
}

// Arm is one branch of a Switch, produced by When, Else, or LoopBack. It carries
// the typed predicate over the switched node's output (nil for an Else arm) and
// the target step's name. Switch type-erases the predicate to func(any)bool at the
// wiring boundary.
//
// A LoopBack arm sets loopBack and loopMax: its target is an EARLIER loop head, so
// the arm is a bounded back-edge (a cycle) rather than a forward route. See LoopBack.
type Arm[M any] struct {
	isElse   bool
	pred     func(M) bool
	predName string
	target   string
	loopBack bool
	loopMax  int
}

// Named names the arm's predicate, as RegisterPredicate names one for a config, and returns
// the arm. Digest commits to the name, so a flow built in Go that names its predicates the
// way a config does has the config's digest, and swapping two named predicates changes it.
// A Go predicate is a func with no stable name of its own, so an unnamed arm commits to
// none: the Go code, not the digest, is then what says which predicate runs.
func (a Arm[M]) Named(name string) Arm[M] {
	a.predName = name
	return a
}

// When routes to `to` when pred(over.Out) is true. pred must be pure over the
// value: Build lowers the branch choice to its own journaled step, so a resumed
// run replays the recorded arm and the predicate is not re-evaluated against
// changed external state.
func When[M any](pred func(M) bool, to Consumer[M]) Arm[M] {
	return Arm[M]{pred: pred, target: endpointName(to)}
}

// Else routes to `to` when no When arm matched. At most one Else per Switch, which
// Build validates. An Else arm carries no predicate.
func Else[M any](to Consumer[M]) Arm[M] {
	return Arm[M]{isElse: true, target: endpointName(to)}
}

// LoopBack declares a BOUNDED LOOP as a Switch arm: when pred(over.Out) is true,
// route the switched value BACK to `head` (an earlier node, the loop head) and run
// the loop body again, up to max iterations. It is the rung-1 way to express a
// bounded loop with NO new executor: a loop is a back-edge (a Switch arm whose
// target is an ancestor of the Switch) plus an iteration bound. The other arm(s) of
// the same Switch are the exit; Build requires at least one non-loop-back arm so the
// loop can terminate.
//
// The type frontier is enforced by the compiler, not at run time: `head` is a
// Consumer[M] and the Switch unifies M with over.Out, so the value routed on the
// back-edge (of type M) must equal the head's input type. A single-type loop is the
// only shape LoopBack expresses, by construction.
//
// max must be > 0. Run journals each iteration's node executions under
// iteration-scoped keys (node:iter:<n>:<node>), so at-most-once, halt-on-ambiguity, and
// resume all hold PER ITERATION exactly as for a linear flow; if the loop would
// re-enter the head more than max times without taking the exit arm, Run returns a
// runaway-loop error rather than looping forever. Like When, pred must be pure over
// the switched value: each iteration's choice is journaled and replayed on resume.
//
// LoopBack co-locates the back-edge, its predicate, its target head, and the bound
// in one typed call, which reads more cleanly than a separate Loop(head, max)
// marker and keeps the type-frontier check at the call site where the compiler can
// enforce it.
func LoopBack[M any](max int, pred func(M) bool, head Consumer[M]) Arm[M] {
	return Arm[M]{pred: pred, target: endpointName(head), loopBack: true, loopMax: max}
}

// Switch routes on over.Out to exactly one arm. M unifies the switched producer's
// output with every arm's predicate/target input, so an arm typed to the wrong
// value does not compile. The branch choice is journaled as its own agent.Journal.Step at
// Build/Run time (Agent D), so a resumed run replays the recorded arm and only the
// taken arm executes. Arms do not reconverge in rung-1 (open design point a): each
// arm terminates a path that Build checks produces Out.
func (b *Builder[In, Out]) Switch[M any](over Producer[M], arms ...Arm[M]) {
	erased := make([]arm, len(arms))
	for i, a := range arms {
		erased[i] = arm{isElse: a.isElse, predName: a.predName, target: a.target, loopBack: a.loopBack, loopMax: a.loopMax}
		if a.pred != nil {
			pred := a.pred
			// Type-erase the typed predicate. The switched value arrives as any at
			// the journal boundary; a wrong dynamic type means the arm did not match.
			erased[i].pred = func(v any) bool {
				typed, ok := v.(M)
				if !ok {
					return false
				}
				return pred(typed)
			}
		}
	}
	b.core.branches = append(b.core.branches, branch{
		over: endpointName(over),
		arms: erased,
	})
}
