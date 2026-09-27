package plan

import (
	"errors"
	"fmt"
)

// Build validates the accumulated spec for whole-graph coherence and freezes it
// into a *Flow[In, Out]. It performs no I/O and runs no step: it is a pure
// static check over the reified topology.
//
// The validations, in order, and each error names the offending step:
//
//   - construction errors recorded during authoring (duplicate names) surface first;
//   - the entry step consumes In (entry.inType == the pinned In);
//   - every node is reachable from the entry via edges and Switch arms;
//   - every terminal path produces Out (a node with no outgoing edge and that is
//     not switched over must have outType == the pinned Out);
//   - each Switch has at most one Else and a predicate typed to the switched output.
//
// On success it returns a *Flow whose spec is sealed: the returned value holds a
// distinct *builderCore, so mutating the original Builder after Build cannot
// change the frozen flow.
func (b *Builder[In, Out]) Build() (*Flow[In, Out], error) {
	c := b.core

	// 1. Drain construction errors (duplicate names, recorded by register).
	if len(c.errs) > 0 {
		return nil, fmt.Errorf("plan: build %q: %w", c.flowName, errors.Join(c.errs...))
	}

	// A flow needs an entry step: the first constructed node. An empty flow cannot
	// consume In or produce Out.
	if c.entry == "" {
		return nil, fmt.Errorf("plan: build %q: flow has no steps (call Step, Tool, or Model before Build)", c.flowName)
	}
	entry := c.byName[c.entry]

	// 2. Re-assert name uniqueness (already enforced during construction; assert the
	// invariant holds so a future spec change cannot silently violate it).
	if len(c.byName) != len(c.nodes) {
		return nil, fmt.Errorf("plan: build %q: node count %d disagrees with unique names %d", c.flowName, len(c.nodes), len(c.byName))
	}

	// 3. Entry consumes In. inType is captured at construction from the step's I.
	if c.inType != nil && entry.inType != nil && entry.inType != c.inType {
		return nil, fmt.Errorf("plan: build %q: entry step %q consumes %s but the flow input is %s",
			c.flowName, entry.name, typeName(entry.inType), typeName(c.inType))
	}

	// 4. Switch coherence: at most one Else per Switch, and the switched-over node
	// exists. The predicate typing is enforced at the call site by Switch[M]; here
	// we assert the switched node is declared so a branch cannot dangle.
	switchedOver := make(map[string]bool, len(c.branches))
	for _, br := range c.branches {
		if c.byName[br.over] == nil {
			return nil, fmt.Errorf("plan: build %q: Switch over unknown step %q", c.flowName, br.over)
		}
		switchedOver[br.over] = true
		elses := 0
		for _, a := range br.arms {
			if c.byName[a.target] == nil {
				return nil, fmt.Errorf("plan: build %q: Switch over %q routes to unknown step %q", c.flowName, br.over, a.target)
			}
			if a.isElse {
				elses++
			}
		}
		if elses > 1 {
			return nil, fmt.Errorf("plan: build %q: Switch over %q has %d Else arms, want at most one", c.flowName, br.over, elses)
		}
	}

	// 4b. Model binding: a Model node calls the flow's bound model at run time, so a
	// flow that declares one must have a model bound via WithModel. Reject it here,
	// naming the node, rather than surfacing an opaque nil-model error deep in Run.
	if c.model == nil {
		for _, n := range c.nodes {
			if n.kind == kindModel {
				return nil, fmt.Errorf("plan: build %q: Model step %q has no bound model; bind one with Builder.WithModel(m) before Build", c.flowName, n.name)
			}
		}
	}

	// 4c. Acyclicity: rung-1 flows have no back-edges. A cycle makes topological
	// execution impossible and is a spec error. topoOrder returns it (and Run reuses
	// the same order), so a graph that builds always runs in a fixed sequential order.
	if _, err := c.topoOrder(); err != nil {
		return nil, fmt.Errorf("plan: build %q: %w", c.flowName, err)
	}

	// 5. Reachability: every node must be reachable from the entry via edges and
	// Switch arms. An orphan step is a wiring mistake; the error names it.
	reachable := c.reachableFrom(c.entry)
	for _, n := range c.nodes {
		if !reachable[n.name] {
			return nil, fmt.Errorf("plan: build %q: step %q is unreachable from entry %q", c.flowName, n.name, c.entry)
		}
	}

	// 5b. Join coherence. A Join fans in several producers that BOTH (all) execute; it
	// is not reconvergence of mutually-exclusive Switch arms (that stays deferred). Two
	// checks, each naming the offending join and input:
	//
	//   - port types: each input producer's declared output type must equal the
	//     corresponding merge-function parameter type, by reflect identity (mirroring how
	//     Edge unifies M). A mismatch is a wiring bug caught here rather than a runtime
	//     type-assertion failure inside the merge.
	//   - both inputs on the executed path: a Join input a Switch can skip (reachable
	//     only via a Switch arm, or via a DIFFERENT arm than another input of the same
	//     Join) is rejected, because the Join would then run with a missing input. See
	//     joinInputGate.
	if err := c.checkJoins(); err != nil {
		return nil, err
	}

	// 6. Terminal coherence: every terminal path produces Out. A terminal node has
	// no outgoing edge and is not switched over (a switched node routes onward
	// through its arms). Each such node must produce Out. Every Switch arm target
	// must lead to a terminal producing Out, which reachability + this check
	// together guarantee: each arm target is reachable and any path from it ends at
	// a terminal that this check constrains to Out.
	hasOutEdge := make(map[string]bool, len(c.edges))
	for _, e := range c.edges {
		hasOutEdge[e.from] = true
	}
	for _, n := range c.nodes {
		if hasOutEdge[n.name] || switchedOver[n.name] {
			continue // routes onward: not a terminal
		}
		if c.outType != nil && n.outType != nil && n.outType != c.outType {
			return nil, fmt.Errorf("plan: build %q: terminal step %q produces %s but the flow output is %s",
				c.flowName, n.name, typeName(n.outType), typeName(c.outType))
		}
	}

	// Freeze: seal a distinct spec into the Flow so post-Build mutation of the
	// original Builder cannot change the frozen flow.
	return &Flow[In, Out]{core: c.seal()}, nil
}

// topoOrder returns the node names in a deterministic topological order over the
// declared directed graph: an ordinary edge (producer -> consumer, which includes a
// Join's input edges) and a Switch arm (switched node -> each arm target) are the
// directed edges. It is Kahn's algorithm with insertion-order tie-breaking (a node
// with no remaining predecessors is emitted in declared node order), so a fixed
// topology always yields the same order and Run's sequential execution and the
// digest stay stable. It returns an error if the graph has a cycle, naming that a
// cycle exists; rung-1 has no back-edges, so a cycle is a build/spec error.
//
// A Join appears after all of its input sources because each input is a directed
// edge into the Join; that ordering is exactly the fan-in barrier Run relies on.
func (c *builderCore) topoOrder() ([]string, error) {
	adj := make(map[string][]string, len(c.nodes))
	indeg := make(map[string]int, len(c.nodes))
	for _, n := range c.nodes {
		indeg[n.name] = 0
	}
	addEdge := func(from, to string) {
		adj[from] = append(adj[from], to)
		indeg[to]++
	}
	for _, e := range c.edges {
		addEdge(e.from, e.to)
	}
	for _, br := range c.branches {
		for _, a := range br.arms {
			addEdge(br.over, a.target)
		}
	}

	// Seed the queue in declared node order so ties break deterministically.
	var queue []string
	for _, n := range c.nodes {
		if indeg[n.name] == 0 {
			queue = append(queue, n.name)
		}
	}
	order := make([]string, 0, len(c.nodes))
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		order = append(order, cur)
		// Collect newly-freed successors, then append them in declared node order so
		// the tie-break stays stable regardless of adjacency insertion order.
		var freed []string
		for _, next := range adj[cur] {
			indeg[next]--
			if indeg[next] == 0 {
				freed = append(freed, next)
			}
		}
		if len(freed) > 0 {
			freedSet := make(map[string]bool, len(freed))
			for _, f := range freed {
				freedSet[f] = true
			}
			for _, n := range c.nodes {
				if freedSet[n.name] {
					queue = append(queue, n.name)
				}
			}
		}
	}
	if len(order) != len(c.nodes) {
		return nil, fmt.Errorf("topology has a cycle (rung-1 flows must be acyclic)")
	}
	return order, nil
}

// checkJoins validates every kindJoin node: its input ports type-match the merge
// function's parameters (by reflect identity), and both/all its inputs lie on a
// single executed path so the Join never runs with a missing input. It names the
// offending join and input in every error.
func (c *builderCore) checkJoins() error {
	for _, n := range c.nodes {
		if n.kind != kindJoin {
			continue
		}
		// Port types: each input producer's output type must equal the corresponding
		// merge parameter type. The Join2/Join3 call site already unifies A/B/(C) with
		// the producer handles' O at compile time; this re-checks by reflect identity so
		// a hand-assembled or loaded spec cannot smuggle a mismatch past the compiler.
		for i, src := range n.joinInputs {
			in := c.byName[src]
			if in == nil {
				return fmt.Errorf("plan: build %q: join %q reads unknown input %q", c.flowName, n.name, src)
			}
			if i < len(n.joinInTypes) && n.joinInTypes[i] != nil && in.outType != nil && in.outType != n.joinInTypes[i] {
				return fmt.Errorf("plan: build %q: join %q input %q produces %s but the merge expects %s",
					c.flowName, n.name, src, typeName(in.outType), typeName(n.joinInTypes[i]))
			}
		}

		// Mutually-exclusive inputs: a Join requires ALL its inputs to run whenever the
		// Join runs. If a Switch can take an arm that skips one input while another input
		// (or the Join itself) still runs, the fan-in is not a barrier over
		// both-execute branches but a reconvergence of mutually-exclusive arms, which is
		// deferred. Reject it, naming the join and the input a Switch can skip.
		gates := make([]map[string]int, len(n.joinInputs)) // per input: switch-over -> required arm index (-2 = requires-but-varies)
		for i, src := range n.joinInputs {
			gates[i] = c.joinInputGate(src)
		}
		for i := range n.joinInputs {
			for _, br := range c.branches {
				armI, gatedI := gates[i][br.over]
				for j := range n.joinInputs {
					if j == i {
						continue
					}
					armJ, gatedJ := gates[j][br.over]
					switch {
					case gatedI && gatedJ && armI != armJ:
						// Two inputs require different arms of the same Switch: mutually exclusive.
						return fmt.Errorf("plan: build %q: join %q inputs %q and %q are on mutually-exclusive arms of the Switch over %q; a Join requires all inputs on one executed path",
							c.flowName, n.name, n.joinInputs[i], n.joinInputs[j], br.over)
					case gatedI && !gatedJ:
						// One input is gated behind a Switch arm that can skip it while the other
						// input runs regardless: the Join could fire with input i missing.
						return fmt.Errorf("plan: build %q: join %q input %q is reachable only via one arm of the Switch over %q and can be skipped while input %q runs; a Join requires all inputs on one executed path",
							c.flowName, n.name, n.joinInputs[i], br.over, n.joinInputs[j])
					}
				}
			}
		}
	}
	return nil
}

// joinInputGate reports, for the given Join-input source node, which Switches gate
// reaching it and via which single arm. The result maps a Switch's switched-over
// node name to the arm index that must be taken to reach src. A Switch appears in
// the map only when src is reachable EXCLUSIVELY through exactly one of its arms
// (the mutual-exclusion hazard); a Switch whose several arms all lead to src, or
// which src reaches without traversing at all, is ungated and omitted. It walks the
// declared edge+arm graph with per-arm reachability, so it sees a Join input a Switch
// can skip regardless of how many plain edges sit between the arm and the input.
func (c *builderCore) joinInputGate(src string) map[string]int {
	adj := make(map[string][]string, len(c.nodes))
	for _, e := range c.edges {
		adj[e.from] = append(adj[e.from], e.to)
	}
	armEdges := make(map[string][]struct {
		arm    int
		target string
	}, len(c.branches))
	for _, br := range c.branches {
		for ai, a := range br.arms {
			armEdges[br.over] = append(armEdges[br.over], struct {
				arm    int
				target string
			}{ai, a.target})
		}
	}

	gate := make(map[string]int)
	for _, br := range c.branches {
		// For this Switch, which arm(s) can reach src? Walk from each arm target over
		// all edges and arms; if exactly one arm reaches src and the switched node is
		// itself required to reach src (src is not reachable from entry while bypassing
		// this Switch), then src is gated by that single arm.
		reachedByArm := make(map[int]bool)
		for ai, a := range br.arms {
			if c.canReach(a.target, src, adj, armEdges) {
				reachedByArm[ai] = true
			}
		}
		if len(reachedByArm) != 1 {
			continue // unreachable through this Switch, or reachable through several arms
		}
		// src is reachable through exactly one arm of this Switch. It is gated only if
		// the Switch is actually on the path: src is NOT reachable from entry without
		// passing through this switched node. Check by reachability from entry with this
		// Switch's arms removed.
		if !c.reachableBypassingSwitch(src, br.over, adj, armEdges) {
			for ai := range reachedByArm {
				gate[br.over] = ai
			}
		}
	}
	return gate
}

// canReach reports whether dst is reachable from start over the given plain-edge and
// arm adjacency (both followed). It is a plain BFS used by joinInputGate.
func (c *builderCore) canReach(start, dst string, adj map[string][]string, armEdges map[string][]struct {
	arm    int
	target string
}) bool {
	if start == dst {
		return true
	}
	seen := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		next := append([]string{}, adj[cur]...)
		for _, ae := range armEdges[cur] {
			next = append(next, ae.target)
		}
		for _, nx := range next {
			if nx == dst {
				return true
			}
			if !seen[nx] {
				seen[nx] = true
				queue = append(queue, nx)
			}
		}
	}
	return false
}

// reachableBypassingSwitch reports whether src is reachable from the entry WITHOUT
// traversing the given switched node's arms: it walks plain edges and all OTHER
// Switches' arms, but never the arms of the named Switch. If src is still reachable,
// the Switch does not gate it (some other path reaches src regardless of the arm
// taken), so it is not a mutual-exclusion hazard.
func (c *builderCore) reachableBypassingSwitch(src, skipOver string, adj map[string][]string, armEdges map[string][]struct {
	arm    int
	target string
}) bool {
	seen := map[string]bool{c.entry: true}
	queue := []string{c.entry}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == src {
			return true
		}
		next := append([]string{}, adj[cur]...)
		if cur != skipOver {
			for _, ae := range armEdges[cur] {
				next = append(next, ae.target)
			}
		}
		for _, nx := range next {
			if !seen[nx] {
				seen[nx] = true
				queue = append(queue, nx)
			}
		}
	}
	return seen[src]
}

// reachableFrom returns the set of node names reachable from start by following
// edges (producer -> consumer) and Switch arms (switched node -> each arm
// target). It is a plain breadth-first walk over the declared topology; it runs
// no step.
func (c *builderCore) reachableFrom(start string) map[string]bool {
	// Adjacency: edges plus branch arms, keyed by source node name.
	adj := make(map[string][]string, len(c.nodes))
	for _, e := range c.edges {
		adj[e.from] = append(adj[e.from], e.to)
	}
	for _, br := range c.branches {
		for _, a := range br.arms {
			adj[br.over] = append(adj[br.over], a.target)
		}
	}

	seen := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range adj[cur] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return seen
}

// seal returns a frozen copy of the spec for a built Flow. It copies the slices
// and map so a later mutation of the original Builder (another Step, Edge, or
// Switch) cannot reach into the frozen flow. The type-erased run closures are
// shared by reference: they are immutable once installed by the constructors.
func (c *builderCore) seal() *builderCore {
	sealed := &builderCore{
		flowName: c.flowName,
		entry:    c.entry,
		inType:   c.inType,
		outType:  c.outType,
		nodes:    make([]*node, len(c.nodes)),
		byName:   make(map[string]*node, len(c.nodes)),
		edges:    make([]edge, len(c.edges)),
		branches: make([]branch, len(c.branches)),
		model:    c.model, // carry the bound model onto the frozen flow so Run can call it
	}
	copy(sealed.nodes, c.nodes)
	for _, n := range sealed.nodes {
		sealed.byName[n.name] = n
	}
	copy(sealed.edges, c.edges)
	for i, br := range c.branches {
		arms := make([]arm, len(br.arms))
		copy(arms, br.arms)
		sealed.branches[i] = branch{over: br.over, arms: arms}
	}
	return sealed
}
