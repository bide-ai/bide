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

	// 5. Reachability: every node must be reachable from the entry via edges and
	// Switch arms. An orphan step is a wiring mistake; the error names it.
	reachable := c.reachableFrom(c.entry)
	for _, n := range c.nodes {
		if !reachable[n.name] {
			return nil, fmt.Errorf("plan: build %q: step %q is unreachable from entry %q", c.flowName, n.name, c.entry)
		}
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
