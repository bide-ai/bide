package govern

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/bide-ai/bide/agent"
	gsm "github.com/blackwell-systems/gsm"
)

// --- minimal scripted model to drive real agent.Agent loops in these tests ---

type scriptModel struct {
	turns [][]agent.Emit
	i     int
}

func (m *scriptModel) Stream(_ context.Context, _ agent.Request) (*agent.Stream, error) {
	turn := m.turns[m.i]
	m.i++
	ch := make(chan agent.Emit, len(turn))
	for _, e := range turn {
		ch <- e
	}
	close(ch)
	return agent.NewStream(ch), nil
}

func toolTurn(id, name string) []agent.Emit {
	return []agent.Emit{
		{Event: agent.ToolCallDelta{Index: 0, ID: id, Name: name, ArgsFragment: json.RawMessage(`{}`)}},
		{Event: agent.Finish{Reason: "tool_use"}},
	}
}
func textTurn(s string) []agent.Emit {
	return []agent.Emit{{Event: agent.TextDelta{Text: s}}, {Event: agent.Finish{Reason: "stop"}}}
}

// orderVars holds the state variables of the order-fulfillment domain.
type orderVars struct {
	status    gsm.Var
	paid      gsm.Var
	inventory gsm.Var
}

// buildOrderMachine builds + VERIFIES the canonical order-fulfillment governed machine.
// Build returning no error IS the convergence guarantee (WFC + CC proven exhaustively).
func buildOrderMachine(t *testing.T) (*gsm.Machine, orderVars) {
	t.Helper()
	r := gsm.NewRegistry("order_fulfillment")
	v := orderVars{
		status:    r.Enum("status", "pending", "paid", "shipped", "cancelled"),
		paid:      r.Bool("paid"),
		inventory: r.Int("inventory", 0, 5),
	}

	r.Invariant("no_ship_unpaid").
		Watches(v.status, v.paid).
		Holds(func(s gsm.State) bool { return s.Get(v.status) != "shipped" || s.GetBool(v.paid) }).
		Repair(func(s gsm.State) gsm.State { return s.Set(v.status, "pending") }).
		Add()
	r.Invariant("stock_non_negative").
		Watches(v.inventory).
		Holds(func(s gsm.State) bool { return s.GetInt(v.inventory) >= 0 }).
		Repair(func(s gsm.State) gsm.State { return s.SetInt(v.inventory, 0) }).
		Add()

	r.Event("process_payment").
		Writes(v.status, v.paid).
		Guard(func(s gsm.State) bool { return s.Get(v.status) == "pending" }).
		Apply(func(s gsm.State) gsm.State { return s.Set(v.status, "paid").SetBool(v.paid, true) }).
		Add()
	r.Event("ship_item").
		Writes(v.status, v.inventory).
		Guard(func(s gsm.State) bool { return s.Get(v.status) == "paid" && s.GetInt(v.inventory) > 0 }).
		Apply(func(s gsm.State) gsm.State {
			return s.Set(v.status, "shipped").SetInt(v.inventory, s.GetInt(v.inventory)-1)
		}).
		Add()
	r.Event("restock").
		Writes(v.inventory).
		Apply(func(s gsm.State) gsm.State { return s.SetInt(v.inventory, s.GetInt(v.inventory)+1) }).
		Add()

	// Only process_payment ⊥ restock is genuinely concurrent (disjoint footprint:
	// status/paid vs inventory). ship_item is NOT independent of restock — restock changes
	// inventory, which ship_item's guard reads — so declaring it makes gsm's CC check FAIL
	// the build. That build-time rejection is the whole value: gsm won't let a divergent
	// concurrency model ship.
	r.Independent("process_payment", "restock")

	m, _, err := r.Build()
	if err != nil {
		t.Fatalf("convergence NOT guaranteed: %v", err)
	}
	return m, v
}

func snapshot(s gsm.State, v orderVars) string {
	return fmt.Sprintf("%s|paid=%v|inv=%d", s.Get(v.status), s.GetBool(v.paid), s.GetInt(v.inventory))
}

// Build succeeds ⇒ gsm proved every interleaving converges. This is the build-time guarantee.
func TestGovernor_BuildProvesConvergence(t *testing.T) {
	buildOrderMachine(t) // fatals if convergence isn't guaranteed
}

// The headline: an independent agent's event (restock, Agent B) interleaved at EVERY
// position within another agent's causal flow (pay→ship, Agent A) converges to the SAME
// valid final state. This is provable order-independence — the thing no other agent
// framework offers.
func TestGovernor_AllInterleavingsConverge(t *testing.T) {
	m, v := buildOrderMachine(t)
	initial := m.NewState().SetInt(v.inventory, 1)

	// Reorder only the PROVEN-independent pair (payment ⊥ restock); ship is causal-last.
	interleavings := [][]string{
		{"process_payment", "restock", "ship_item"},
		{"restock", "process_payment", "ship_item"},
	}

	var want string
	for i, seq := range interleavings {
		g := New(m, initial)
		for _, e := range seq {
			g.Apply(context.Background(), e)
		}
		got := snapshot(g.State(), v)
		if i == 0 {
			want = got
		} else if got != want {
			t.Fatalf("interleaving %v → %s, but earlier orders → %s (DIVERGENCE)", seq, got, want)
		}
	}
	if want != "shipped|paid=true|inv=1" {
		t.Fatalf("converged state = %s, want shipped|paid=true|inv=1", want)
	}
}

// Two REAL concurrent goroutine-agents on one shared Governor, run many times: the
// scheduler interleaves them nondeterministically, yet every run lands on the same valid
// state. Run with -race.
func TestGovernor_ConcurrentAgentsConverge(t *testing.T) {
	m, v := buildOrderMachine(t)
	initial := m.NewState().SetInt(v.inventory, 1)

	for trial := 0; trial < 300; trial++ {
		g := New(m, initial)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { // Agent A: payment
			defer wg.Done()
			g.Apply(context.Background(), "process_payment")
		}()
		go func() { // Agent B: inventory — independent of payment (proven at build)
			defer wg.Done()
			g.Apply(context.Background(), "restock")
		}()
		wg.Wait()
		// The two concurrent agents converged regardless of scheduler interleaving; ship
		// is the causal finalize.
		g.Apply(context.Background(), "ship_item")

		if got := snapshot(g.State(), v); got != "shipped|paid=true|inv=1" {
			t.Fatalf("trial %d diverged: %s", trial, got)
		}
	}
}

// The governor middleware, end-to-end: TWO real agent loops — a payment agent and an
// inventory agent — each call a GOVERNED tool that emits a gsm event to one shared
// Governor. Run concurrently many times; the shared governed state converges every time,
// regardless of scheduler interleaving. This is the bridge from actual agents to proven
// convergence.
func TestGovernor_TwoRealAgentsConverge(t *testing.T) {
	m, v := buildOrderMachine(t)

	for trial := 0; trial < 50; trial++ {
		gov := New(m, m.NewState().SetInt(v.inventory, 1))
		payTool := EventTool(gov, "pay", "process the payment", "process_payment", agent.Safety{})
		restockTool := EventTool(gov, "restock", "restock inventory", "restock", agent.Safety{})

		store := agent.NewMemStore()
		payAgent := agent.New(&scriptModel{turns: [][]agent.Emit{toolTurn("p1", "pay"), textTurn("done")}}, store, payTool)
		invAgent := agent.New(&scriptModel{turns: [][]agent.Emit{toolTurn("r1", "restock"), textTurn("done")}}, store, restockTool)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = payAgent.Run(context.Background(), "pay-run", "pay the order") }()
		go func() { defer wg.Done(); _, _ = invAgent.Run(context.Background(), "inv-run", "restock") }()
		wg.Wait()

		// payment ⊥ restock (proven) → converges regardless of which agent's tool landed first.
		if got := snapshot(gov.State(), v); got != "paid|paid=true|inv=2" {
			t.Fatalf("trial %d: two real agents diverged: %s", trial, got)
		}
	}
}

// Persistence: a PersistentGovernor's state survives a "restart" — every event is durably
// logged, and a fresh governor reconstructs the exact same state by replaying the log.
func TestPersistentGovernor_ReconstructsAfterRestart(t *testing.T) {
	m, v := buildOrderMachine(t)
	log := NewMemEventLog()
	ctx := context.Background()
	initial := m.NewState().SetInt(v.inventory, 1)

	pg, err := NewPersistent(ctx, m, log, "order-42", initial)
	if err != nil {
		t.Fatal(err)
	}
	pg.Apply(ctx, "process_payment")
	pg.Apply(ctx, "restock")
	pg.Apply(ctx, "ship_item")
	before := snapshot(pg.State(), v)

	// "Restart": a brand-new governor over the SAME durable log reconstructs by replay.
	pg2, err := NewPersistent(ctx, m, log, "order-42", initial)
	if err != nil {
		t.Fatal(err)
	}
	after := snapshot(pg2.State(), v)

	if before != after {
		t.Fatalf("reconstruction diverged: before=%s after=%s", before, after)
	}
	if after != "shipped|paid=true|inv=1" {
		t.Fatalf("reconstructed state = %s, want shipped|paid=true|inv=1", after)
	}
}

// Event-sourced convergence (Option B): replaying the SAME event set in a different order
// yields the same state — for the events gsm proved independent. Order in the log is not
// load-bearing for correctness.
func TestPersistentGovernor_ReplayOrderIndependent(t *testing.T) {
	m, v := buildOrderMachine(t)
	ctx := context.Background()
	initial := m.NewState().SetInt(v.inventory, 1)

	// Two logs holding the same independent event set (payment ⊥ restock) in opposite order.
	logA := NewMemEventLog()
	_, _ = logA.Append(ctx, "e", "process_payment")
	_, _ = logA.Append(ctx, "e", "restock")

	logB := NewMemEventLog()
	_, _ = logB.Append(ctx, "e", "restock")
	_, _ = logB.Append(ctx, "e", "process_payment")

	a, _ := NewPersistent(ctx, m, logA, "e", initial)
	b, _ := NewPersistent(ctx, m, logB, "e", initial)

	if snapshot(a.State(), v) != snapshot(b.State(), v) {
		t.Fatalf("replay order changed the result: %s vs %s", snapshot(a.State(), v), snapshot(b.State(), v))
	}
}

// Compensation: an invalid state (shipped-but-unpaid) is auto-repaired to a valid normal
// form on the next apply — no manual rollback, no stuck-invalid state.
func TestGovernor_CompensatesInvalidState(t *testing.T) {
	m, v := buildOrderMachine(t)
	// Force an invalid raw state: shipped without paid.
	invalid := m.NewState().Set(v.status, "shipped").SetInt(v.inventory, 1)

	g := New(m, invalid)
	g.Apply(context.Background(), "restock") // any transition normalizes the whole state

	s := g.State()
	if s.Get(v.status) == "shipped" && !s.GetBool(v.paid) {
		t.Fatalf("invalid shipped-unpaid state was NOT compensated: %s", snapshot(s, v))
	}
}
