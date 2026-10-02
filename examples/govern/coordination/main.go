// Command coordination shows the gsm "Governed State Machine" convergence layer surfaced
// through the govern package: multiple agents mutating SHARED state converge to the same
// valid normal form regardless of interleaving, certified at build time (not asserted at
// runtime). Two scenes, both with NO API key (a small inline scripted Model drives the
// agent tool calls):
//
//   - Scene 1 (single registry, verify-or-repair): declare an invariant with no explicit
//     compensation and let gsm SYNTHESIZE a convergent repair (Registry.BuildOrSynthesize),
//     wrap the verified machine with govern.New, and drive it through an agent via
//     govern.EventTool. Applying past the cap clamps back to a valid normal form.
//
//   - Scene 2 (cyclic non-monotone federation): a two-registry federation with a cycle that
//     plain Build rejects (it cannot converge). CoordinationPlan() names the one morphism
//     edge to place under external coordination; BuildCoordinated(plan) then builds a
//     convergent FedMachine, which govern.NewFederated wraps as a crash-recoverable governor.
//
//     cd examples/govern && go run ./coordination
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/govern"
	gsm "github.com/blackwell-systems/gsm"
)

// callN calls the named tool n times across n turns, then answers in text. It drives the
// governed EventTool from a real agent loop without a live LLM.
type callN struct {
	tool string
	n    int
	turn int
}

func (m *callN) Stream(_ context.Context, _ agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 4)
	if m.turn < m.n {
		id := fmt.Sprintf("c%d", m.turn)
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: id, Name: m.tool, ArgsFragment: []byte(`{}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	} else {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "done applying governed events"}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	}
	m.turn++
	close(ch)
	return agent.NewStream(ch), nil
}

func main() {
	synthesizeScene()
	fmt.Println()
	federatedScene()
}

// synthesizeScene: BuildOrSynthesize verifies convergence or, when the invariant has no
// explicit repair, synthesizes one. The verified machine is wrapped with govern.New and
// exposed to an agent as a governed EventTool. Each tool call applies "inc_a"; once past the
// cap the synthesized compensation clamps the state back to a valid normal form.
func synthesizeScene() {
	fmt.Println("== Scene 1: single registry, verify-or-repair (BuildOrSynthesize) ==")

	r := gsm.NewRegistry("counters")
	a := r.Int("a", 0, 5)
	b := r.Int("b", 0, 5)
	// Cap on `a` with a nil repair: gsm SYNTHESIZES a convergent compensation rather than
	// making us write one. The events only increment (monotone), so a convergent repair
	// exists (clamp `a` to its cap).
	r.DeclInvariant("a_cap", gsm.Le(gsm.V(a), gsm.Lit(3)), nil)
	r.DeclEvent("inc_a", gsm.Do(gsm.Set(a, gsm.Add(gsm.V(a), gsm.Lit(1)))))
	r.DeclEvent("inc_b", gsm.Do(gsm.Set(b, gsm.Add(gsm.V(b), gsm.Lit(1)))))

	m, syn, err := r.BuildOrSynthesize()
	if err != nil {
		log.Fatalf("BuildOrSynthesize: %v", err)
	}
	fmt.Printf("  convergent=%v (synthesized a repair, cost %d)\n", syn.Convergent, syn.Cost)

	gov := govern.New(m, m.NewState())
	incA := govern.EventTool(gov, govern.EventToolConfig{Name: "increment_a", Description: "Apply the governed inc_a event", Event: "inc_a", Safety: agent.Safety{Idempotent: true}})

	// The agent calls increment_a five times; the cap is 3, so the synthesized compensation
	// clamps the shared state to a valid normal form regardless of how the calls interleave.
	if _, err := agent.New(&callN{tool: "increment_a", n: 5}, agent.NewMemStore(), incA).
		Run(context.Background(), "gov-1", "Increment a five times."); err != nil {
		log.Fatalf("run: %v", err)
	}
	st := gov.State()
	fmt.Printf("  after 5 governed inc_a: a=%d (clamped to cap), valid=%v\n", st.GetInt(a), m.IsValid(st))
}

// federatedScene: a cyclic, non-monotone federation. Plain Build rejects the cycle;
// CoordinationPlan names the edge to coordinate externally, and BuildCoordinated builds a
// convergent FedMachine. govern.NewFederated wraps it as a crash-recoverable governor whose
// state is reconstructed by replaying a durable event log.
func federatedScene() {
	fmt.Println("== Scene 2: cyclic federation, break the cycle with a coordination plan ==")
	ctx := context.Background()

	regA := gsm.NewRegistry("a")
	as := regA.Enum("as", "lo", "hi")
	regA.On("araise").Does(gsm.SetLabel(as, "hi")).Add()

	regB := gsm.NewRegistry("b")
	bs := regB.Enum("bs", "lo", "hi")
	regB.On("braise").Does(gsm.SetLabel(bs, "hi")).Add()

	// A non-monotone flip in BOTH directions makes the two morphisms a cycle: a drives b and
	// b drives a, so no acyclic topological normal form exists.
	flip := map[string]string{"lo": "hi", "hi": "lo"}
	fed := gsm.NewFederation("cyc").
		Morphism(regA, regB).Shared(bs).Map(func(src, dst gsm.State) gsm.State { return dst.Set(bs, flip[src.Get(as)]) }).Add().
		Morphism(regB, regA).Shared(as).Map(func(src, dst gsm.State) gsm.State { return dst.Set(as, flip[src.Get(bs)]) }).Add()

	if _, _, err := fed.Build(); err != nil {
		fmt.Printf("  plain Build rejects the cycle: %v\n", firstLine(err.Error()))
	}

	// CoordinationPlan names the morphism edge whose target shared component must be placed
	// under external coordination to break the obstruction.
	plan := fed.CoordinationPlan()
	fmt.Printf("  coordination plan: %d point(s): %v\n", len(plan), plan)

	m, _, err := fed.BuildCoordinated(plan)
	if err != nil {
		log.Fatalf("BuildCoordinated: %v", err)
	}

	// Wrap the convergent FedMachine as a crash-recoverable governor over a durable log.
	gov, err := govern.NewFederated(ctx, m, govern.NewMemEventLog(), "order-1", m.NewState())
	if err != nil {
		log.Fatalf("NewFederated: %v", err)
	}
	if _, err := gov.Apply(ctx, "a", "araise"); err != nil {
		log.Fatalf("apply: %v", err)
	}
	if _, err := gov.Apply(ctx, "b", "braise"); err != nil {
		log.Fatalf("apply: %v", err)
	}
	fs := gov.State()
	fmt.Printf("  federated state after coordinated build: a.as=%q b.bs=%q\n",
		m.Of(fs, regA).Get(as), m.Of(fs, regB).Get(bs))
}

// firstLine trims a multi-line gsm diagnostic to its headline for compact output.
func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}
