// Command mesh demonstrates a durable, coordination-free safety mesh where agents' governed
// state constrains each other *cyclically* (mutual constraints) — a topology the tree/DAG
// governance tier cannot express.
//
// Three production lines form a ring. Each line can signal a safety level (normal < caution
// < stop). The mesh rule is most-restrictive-wins: every line runs at the HIGHEST level any
// line is signalling — you cannot have one line running normal while a peer signals stop.
// The rule is mutual (cyclic), so it needs monotone-cycle convergence. Actions go through
// govern.FederatedEventTool, the same tool boundary a real LLM agent would call.
//
// Run (from examples/govern, its own module): go run ./mesh
package main

import (
	"context"
	"fmt"

	"github.com/bide-ai/bide/govern"
	gsm "github.com/blackwell-systems/gsm"
)

var levels = []string{"normal", "caution", "stop"}
var rank = map[string]int{"normal": 0, "caution": 1, "stop": 2}

// signalLevels are the levels a line can signal, one event each (signal_<level>).
var signalLevels = levels

func buildSafetyMesh() (*gsm.FedMachine, []*gsm.Registry, []gsm.Var) {
	names := []string{"line1", "line2", "line3"}
	regs := make([]*gsm.Registry, 3)
	signalV := make([]gsm.Var, 3)
	levelV := make([]gsm.Var, 3)
	for i, n := range names {
		r := gsm.NewRegistry(n)
		signalV[i] = r.Enum("signal", levels...) // this line's own request (local)
		levelV[i] = r.Enum("level", levels...)   // effective level (shared, mesh-controlled)
		sig := signalV[i]
		for _, lv := range signalLevels {
			v := lv
			r.Event("signal_" + v).Writes(sig).
				Apply(func(s gsm.State) gsm.State { return s.Set(sig, v) }).Add()
		}
		regs[i] = r
	}
	// Ring: each line's effective level = max(predecessor's signal, predecessor's level).
	fed := gsm.NewFederation("safety-ring").AllowMonotoneCycles()
	for i := 0; i < 3; i++ {
		pred := (i + 2) % 3
		pSig, pLvl, myLvl := signalV[pred], levelV[pred], levelV[i]
		fed.Morphism(regs[pred], regs[i]).Shared(myLvl).
			Map(func(srcNF, d gsm.State) gsm.State {
				hi := rank[srcNF.Get(pSig)]
				if r := rank[srcNF.Get(pLvl)]; r > hi {
					hi = r
				}
				return d.Set(myLvl, levels[hi])
			}).Add()
	}
	m, _, err := fed.Build() // verifies monotonicity — a non-monotone cyclic rule is rejected here
	if err != nil {
		panic(err)
	}
	return m, regs, levelV
}

func main() {
	ctx := context.Background()
	m, regs, levelV := buildSafetyMesh()

	show := func(label string, g *govern.FederatedGovernor) {
		st := g.State()
		fmt.Printf("%-30s line1=%-8s line2=%-8s line3=%-8s\n", label,
			m.Of(st, regs[0]).Get(levelV[0]), m.Of(st, regs[1]).Get(levelV[1]), m.Of(st, regs[2]).Get(levelV[2]))
	}
	// An agent action: a tool call that becomes a governed event (the real LLM-agent path).
	act := func(g *govern.FederatedGovernor, line, level string) {
		tool := govern.FederatedEventTool(g, govern.FederatedEventToolConfig{Name: line + "_signal_" + level, Description: line + " signals " + level, Registry: line, Event: "signal_" + level})
		if _, err := tool.Call(ctx, []byte("{}")); err != nil {
			panic(err)
		}
	}

	fmt.Println("== Safety mesh: 3 production lines in a ring, most-restrictive-wins ==")
	fmt.Println("Rule: every line runs at the highest level ANY line signals. Cyclic mutual constraint.")
	fmt.Println("Actions go through govern.FederatedEventTool — the agent tool boundary.")
	fmt.Println()

	log := govern.NewMemEventLog()
	g, _ := govern.NewFederated(ctx, m, log, "plant", m.NewState())
	show("initial:", g)

	fmt.Println("\nTwo lines signal DIFFERENT levels (a genuine conflict):")
	act(g, "line2", "caution")
	show("  line2 → caution:", g)
	act(g, "line3", "stop")
	show("  line3 → stop:", g)
	fmt.Println("→ conflict (caution vs stop) reconciled: the whole plant runs at STOP.")

	fmt.Println("\nSame two signals, opposite order (a separate governor):")
	g2, _ := govern.NewFederated(ctx, m, govern.NewMemEventLog(), "plant", m.NewState())
	act(g2, "line3", "stop")
	act(g2, "line2", "caution")
	show("  converged:", g2)
	fmt.Println("→ identical — coordination-free, order-independent.")

	fmt.Println("\nline3 stands down; the mesh re-derives from current signals:")
	act(g, "line3", "normal")
	show("  line3 → normal:", g)
	fmt.Println("→ line2's caution still governs the plant — not stuck high.")

	fmt.Println("\nCrash recovery (a fresh process replays the durable event log):")
	g3, _ := govern.NewFederated(ctx, m, log, "plant", m.NewState())
	show("  reconstructed:", g3)
	fmt.Println("→ rebuilt from the event log alone. (Swap MemEventLog for the SQLite/Redis adapter for real durability.)")
}
