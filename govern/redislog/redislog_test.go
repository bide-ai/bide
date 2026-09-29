package redislog_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/bide-ai/bide/govern"
	"github.com/bide-ai/bide/govern/redislog"
	gsm "github.com/blackwell-systems/gsm"
)

// The Redis adapter satisfies the govern.EventLog port (asserted here so the adapter
// package needn't import govern — one-directional coupling).
var _ govern.EventLog = (*redislog.Log)(nil)

func buildOrderMachine(t *testing.T) (*gsm.Machine, gsm.Var, gsm.Var, gsm.Var) {
	t.Helper()
	r := gsm.NewRegistry("order")
	status := r.Enum("status", "pending", "paid", "shipped", "cancelled")
	paid := r.Bool("paid")
	inventory := r.Int("inventory", 0, 5)
	r.Invariant("no_ship_unpaid").Watches(status, paid).
		Holds(func(s gsm.State) bool { return s.Get(status) != "shipped" || s.GetBool(paid) }).
		Repair(func(s gsm.State) gsm.State { return s.Set(status, "pending") }).Add()
	r.Invariant("stock_non_negative").Watches(inventory).
		Holds(func(s gsm.State) bool { return s.GetInt(inventory) >= 0 }).
		Repair(func(s gsm.State) gsm.State { return s.SetInt(inventory, 0) }).Add()
	r.Event("process_payment").Writes(status, paid).
		Guard(func(s gsm.State) bool { return s.Get(status) == "pending" }).
		Apply(func(s gsm.State) gsm.State { return s.Set(status, "paid").SetBool(paid, true) }).Add()
	r.Event("ship_item").Writes(status, inventory).
		Guard(func(s gsm.State) bool { return s.Get(status) == "paid" && s.GetInt(inventory) > 0 }).
		Apply(func(s gsm.State) gsm.State { return s.Set(status, "shipped").SetInt(inventory, s.GetInt(inventory)-1) }).Add()
	r.Event("restock").Writes(inventory).
		Apply(func(s gsm.State) gsm.State { return s.SetInt(inventory, s.GetInt(inventory)+1) }).Add()
	r.Independent("process_payment", "restock")
	m, _, err := r.Build()
	if err != nil {
		t.Fatalf("convergence not guaranteed: %v", err)
	}
	return m, status, paid, inventory
}

// Set REDIS_ADDR (e.g. localhost:6379) to run; skips otherwise so the suite stays green.
// Proves networked event-sourced reconstruction: apply events, drop the in-memory
// governor, reconstruct a fresh one from the SAME Redis stream by replay.
func TestRedisLog_ReconstructFromStream(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("set REDIS_ADDR to run the Redis Streams integration test")
	}
	ctx := context.Background()
	m, status, paid, inv := buildOrderMachine(t)

	log, err := redislog.Open(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	entity := fmt.Sprintf("order-redis-%s-%d", t.Name(), time.Now().UnixNano()) // unique per run: the Redis instance persists
	initial := m.NewState().SetInt(inv, 1)

	pg1, err := govern.NewPersistent(ctx, m, log, entity, initial)
	if err != nil {
		t.Fatal(err)
	}
	pg1.Apply(ctx, "process_payment")
	pg1.Apply(ctx, "restock")
	pg1.Apply(ctx, "ship_item")
	s1 := pg1.State()

	// Fresh governor over the SAME Redis stream reconstructs by replay.
	pg2, err := govern.NewPersistent(ctx, m, log, entity, initial)
	if err != nil {
		t.Fatal(err)
	}
	s2 := pg2.State()

	if s1.Get(status) != s2.Get(status) || s1.GetBool(paid) != s2.GetBool(paid) || s1.GetInt(inv) != s2.GetInt(inv) {
		t.Fatalf("reconstruction diverged: before {%s %v %d} after {%s %v %d}",
			s1.Get(status), s1.GetBool(paid), s1.GetInt(inv), s2.Get(status), s2.GetBool(paid), s2.GetInt(inv))
	}
	if s2.Get(status) != "shipped" || !s2.GetBool(paid) || s2.GetInt(inv) != 1 {
		t.Fatalf("reconstructed = {%s %v %d}, want {shipped true 1}", s2.Get(status), s2.GetBool(paid), s2.GetInt(inv))
	}
}
