package plan

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
)

// topoOrderReference is topoOrder as it was written before it stopped scanning every node for each
// node it freed: the reference the property test below holds the current one to. Node order feeds the
// digest, so the order must not change by so much as a swap.
func (c *builderCore) topoOrderReference() ([]string, error) {
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
			// A LoopBack arm is a bounded BACK-EDGE to an earlier loop head: it forms a
			// cycle deliberately, so it is EXCLUDED from the forward graph. Excluding it
			// keeps the forward graph acyclic, so Kahn still linearizes it and Run walks a
			// fixed forward order; Run handles the back-edge separately as loop re-entry.
			if a.loopBack {
				continue
			}
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
		emitted := make(map[string]bool, len(order))
		for _, name := range order {
			emitted[name] = true
		}
		return nil, fmt.Errorf("topology has a cycle %s (rung-1 flows must be acyclic: a way back is an arm with loop_max)", cyclePath(c.nodes, adj, emitted))
	}
	return order, nil
}

// randomCore builds a spec of n nodes with random edges and switch arms (some of them loop-back arms,
// which topoOrder leaves out), mostly acyclic, with some back edges so that cycles are covered too.
func randomCore(r *rand.Rand, n int, cycles bool) *builderCore {
	c := &builderCore{flowName: "f", byName: map[string]*node{}}
	for i := range n {
		nd := &node{name: fmt.Sprintf("n%d", i)}
		c.nodes = append(c.nodes, nd)
		c.byName[nd.name] = nd
	}
	rank := r.Perm(n) // the topological rank of each node, unrelated to its declared position
	pick := func() (int, int) {
		a, b := r.IntN(n), r.IntN(n)
		if rank[a] > rank[b] && !(cycles && r.IntN(12) == 0) {
			a, b = b, a
		}
		return a, b
	}
	for range r.IntN(3*n + 1) {
		a, b := pick()
		if a != b {
			c.edges = append(c.edges, edge{from: c.nodes[a].name, to: c.nodes[b].name})
		}
	}
	for range r.IntN(n/3 + 1) {
		a, _ := pick()
		br := branch{over: c.nodes[a].name}
		for range 1 + r.IntN(3) {
			x, y := pick()
			_ = x
			if y != a {
				br.arms = append(br.arms, arm{target: c.nodes[y].name, loopBack: r.IntN(5) == 0})
			}
		}
		c.branches = append(c.branches, br)
	}
	return c
}

// The order topoOrder returns, and the error for a cycle, are exactly those of the reference over
// thousands of random graphs: fan-in and fan-out, duplicate edges, loop-back arms, declared positions
// unrelated to the topological order.
func TestTopoOrderMatchesTheReference(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	var cyclic, acyclic int
	for i := range 6000 {
		n := 1 + r.IntN(40)
		if i%500 == 0 {
			n = 200 + r.IntN(300) // a few large ones
		}
		c := randomCore(r, n, i%3 == 0)
		got, gerr := c.topoOrder()
		want, werr := c.topoOrderReference()
		if (gerr == nil) != (werr == nil) || (gerr != nil && gerr.Error() != werr.Error()) {
			t.Fatalf("graph %d (%d nodes): error %v, reference %v", i, n, gerr, werr)
		}
		if gerr != nil {
			cyclic++
			continue
		}
		acyclic++
		if !slices.Equal(got, want) {
			t.Fatalf("graph %d (%d nodes): order %v, reference %v", i, n, got, want)
		}
	}
	if cyclic < 100 || acyclic < 1000 {
		t.Errorf("the generator covers %d cyclic and %d acyclic graphs; want both well represented", cyclic, acyclic)
	}
}
