package sqlitelog_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bide-ai/bide/govern"
	"github.com/bide-ai/bide/govern/sqlitelog"
	gsm "github.com/blackwell-systems/gsm"
)

// The SQLite adapter satisfies the govern.EventLog port (asserted in the test so the
// adapter package itself needn't import govern — keeping the coupling one-directional).
var _ govern.EventLog = (*sqlitelog.Log)(nil)

// buildOrderMachine mirrors the govern package's canonical order-fulfillment machine.
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

// Real on-disk crash recovery: apply events, CLOSE the DB (process exit), reopen the file,
// and a fresh PersistentGovernor reconstructs the exact governed state by replay.
func TestSQLiteLog_DurableReconstructAcrossReopen(t *testing.T) {
	ctx := context.Background()
	m, status, paid, inv := buildOrderMachine(t)
	path := filepath.Join(t.TempDir(), "events.db")
	initial := m.NewState().SetInt(inv, 1)

	// Process 1: apply governed events, then "crash" (close the file).
	log1, err := sqlitelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	pg1, err := govern.NewPersistent(ctx, m, log1, "order-1", initial)
	if err != nil {
		t.Fatal(err)
	}
	pg1.Apply(ctx, "process_payment")
	pg1.Apply(ctx, "restock")
	pg1.Apply(ctx, "ship_item")
	s1 := pg1.State()
	log1.Close()

	// Process 2: reopen the SAME file, reconstruct by replay.
	log2, err := sqlitelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer log2.Close()
	pg2, err := govern.NewPersistent(ctx, m, log2, "order-1", initial)
	if err != nil {
		t.Fatal(err)
	}
	s2 := pg2.State()

	if s1.Get(status) != s2.Get(status) || s1.GetBool(paid) != s2.GetBool(paid) || s1.GetInt(inv) != s2.GetInt(inv) {
		t.Fatalf("on-disk reconstruction diverged: before {%s %v %d} after {%s %v %d}",
			s1.Get(status), s1.GetBool(paid), s1.GetInt(inv), s2.Get(status), s2.GetBool(paid), s2.GetInt(inv))
	}
	if s2.Get(status) != "shipped" || !s2.GetBool(paid) || s2.GetInt(inv) != 1 {
		t.Fatalf("reconstructed = {%s %v %d}, want {shipped true 1}", s2.Get(status), s2.GetBool(paid), s2.GetInt(inv))
	}
}
