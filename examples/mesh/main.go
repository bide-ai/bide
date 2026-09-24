// Command mesh demonstrates a durable, coordination-free *mesh* of agents whose governed
// state constrains each other cyclically — a topology the tree/DAG governance tier cannot
// express. Three regional agents form a ring; the rule is that every region must run at
// least the highest capacity tier requested anywhere in the ring (a mutual constraint).
//
// gsm's monotone-cycle convergence guarantees all regions agree without coordination,
// regardless of the order the agents act, and the event-sourced FederatedGovernor makes it
// crash-recoverable. Run: go run ./examples/mesh
package main

import (
	"context"
	"fmt"

	gsm "github.com/blackwell-systems/gsm"
	"github.com/dayna/go-agents/govern"
)

// buildRing constructs the cyclic capacity mesh: us → eu → ap → us, where each region's
// effective tier is the max of its predecessor's requested and effective tier.
func buildRing() (*gsm.FedMachine, []*gsm.Registry, []gsm.Var) {
	names := []string{"us", "eu", "ap"}
	regs := make([]*gsm.Registry, 3)
	reqV := make([]gsm.Var, 3)
	tierV := make([]gsm.Var, 3)
	for i, n := range names {
		r := gsm.NewRegistry(n)
		reqV[i] = r.Int("req", 0, 3)
		tierV[i] = r.Int("tier", 0, 3)
		rq := reqV[i]
		for lvl := 1; lvl <= 3; lvl++ {
			l := lvl
			r.Event(fmt.Sprintf("request_t%d", l)).Writes(rq).
				Apply(func(s gsm.State) gsm.State { return s.SetInt(rq, l) }).Add()
		}
		regs[i] = r
	}
	fed := gsm.NewFederation("capacity-ring").AllowMonotoneCycles()
	for i := 0; i < 3; i++ {
		pred := (i + 2) % 3 // us←ap, eu←us, ap←eu  (a ring)
		pReq, pTier, myTier := reqV[pred], tierV[pred], tierV[i]
		fed.Morphism(regs[pred], regs[i]).Shared(myTier).
			Map(func(srcNF, d gsm.State) gsm.State {
				mx := srcNF.GetInt(pReq)
				if v := srcNF.GetInt(pTier); v > mx {
					mx = v
				}
				return d.SetInt(myTier, mx)
			}).Add()
	}
	m, _, err := fed.Build() // verifies monotonicity — a non-monotone cyclic repair is rejected here
	if err != nil {
		panic(err)
	}
	return m, regs, tierV
}

func main() {
	ctx := context.Background()
	m, regs, tierV := buildRing()

	show := func(label string, g *govern.FederatedGovernor) {
		st := g.State()
		fmt.Printf("%-24s us=%d  eu=%d  ap=%d\n", label,
			m.Of(st, regs[0]).GetInt(tierV[0]),
			m.Of(st, regs[1]).GetInt(tierV[1]),
			m.Of(st, regs[2]).GetInt(tierV[2]))
	}

	fmt.Println("== Mutual-constraint mesh: 3 regional agents in a ring ==")
	fmt.Println("Rule: every region runs at least the highest tier requested anywhere in the ring.")
	fmt.Println("Topology: us → eu → ap → us  (cyclic — impossible without monotone-cycle support)")
	fmt.Println()

	log := govern.NewMemEventLog()
	g, _ := govern.NewFederated(ctx, m, log, "ring", m.NewState())
	show("initial:", g)

	fmt.Println("\nAgents act:")
	g.Apply(ctx, "eu", "request_t3")
	show("  eu requests tier 3:", g)
	g.Apply(ctx, "us", "request_t1")
	show("  us requests tier 1:", g)
	fmt.Println("→ eu's tier-3 request propagated around the whole ring; all regions converge to 3.")

	fmt.Println("\nSame requests, reverse order (a separate governor):")
	g2, _ := govern.NewFederated(ctx, m, govern.NewMemEventLog(), "ring", m.NewState())
	g2.Apply(ctx, "us", "request_t1")
	g2.Apply(ctx, "eu", "request_t3")
	show("  converged:", g2)
	fmt.Println("→ identical result — coordination-free convergence regardless of order.")

	fmt.Println("\nCrash recovery (a fresh process replays the durable event log):")
	g3, _ := govern.NewFederated(ctx, m, log, "ring", m.NewState())
	show("  reconstructed:", g3)
	fmt.Println("→ state rebuilt from the log alone.")

	fmt.Println("\n(In production each region is an LLM agent calling govern.FederatedEventTool;")
	fmt.Println(" here deterministic calls stand in. Swap MemEventLog for the SQLite or Redis")
	fmt.Println(" adapter for cross-process durability.)")
}
