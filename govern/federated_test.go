package govern_test

import (
	"context"
	"path/filepath"
	"testing"

	gsm "github.com/blackwell-systems/gsm"
	"github.com/dayna/go-agents/govern"
	"github.com/dayna/go-agents/govern/sqlitelog"
)

// buildMfrSupFederation reproduces the paper's §8.5 manufacturer–supplier federation: a
// manufacturer (authoritative source) and a supplier (target) linked by a total morphism.
func buildMfrSupFederation(t *testing.T) (*gsm.FedMachine, *gsm.Registry, *gsm.Registry, gsm.Var, gsm.Var) {
	t.Helper()

	mfr := gsm.NewRegistry("manufacturer")
	mstate := mfr.Enum("mstate", "draft", "active", "suspended")
	mfr.Event("epub").Writes(mstate).
		Guard(func(s gsm.State) bool { return s.Get(mstate) == "draft" }).
		Apply(func(s gsm.State) gsm.State { return s.Set(mstate, "active") }).Add()

	sup := gsm.NewRegistry("supplier")
	sstate := sup.Enum("sstate", "idle", "listed", "stale", "err")
	sup.Invariant("no_err").Watches(sstate).
		Holds(func(s gsm.State) bool { return s.Get(sstate) != "err" }).
		Repair(func(s gsm.State) gsm.State { return s.Set(sstate, "idle") }).Add()
	sup.Event("eexp").Writes(sstate).
		Apply(func(s gsm.State) gsm.State { return s.Set(sstate, "stale") }).Add()

	image := map[string]string{"draft": "idle", "active": "listed", "suspended": "stale"}
	fed := gsm.NewFederation("mfr-sup").
		Morphism(mfr, sup).Shared(sstate).
		Map(func(srcNF, dst gsm.State) gsm.State { return dst.Set(sstate, image[srcNF.Get(mstate)]) }).
		Add()

	m, rep, err := fed.Build()
	if err != nil {
		t.Fatalf("federation does not converge: %v\n%s", err, rep)
	}
	return m, mfr, sup, mstate, sstate
}

// TestFederatedGovernor_ReconstructFromLog proves crash recovery: apply events through one
// governor, drop it, then reconstruct a fresh governor from the SAME log and confirm the
// federated state matches. Runs over both the in-memory and on-disk SQLite event logs.
func TestFederatedGovernor_ReconstructFromLog(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "fed.db")
	sqLog, err := sqlitelog.Open(sqlitePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqLog.Close() })

	logs := map[string]govern.EventLog{
		"mem":    govern.NewMemEventLog(),
		"sqlite": sqLog,
	}

	for name, log := range logs {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			m, mfr, sup, mstate, sstate := buildMfrSupFederation(t)
			entity := "order-" + name

			g1, err := govern.NewFederated(ctx, m, log, entity, m.NewState())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := g1.Apply(ctx, "manufacturer", "epub"); err != nil {
				t.Fatal(err)
			}
			if _, err := g1.Apply(ctx, "supplier", "eexp"); err != nil {
				t.Fatal(err)
			}
			s1 := g1.State()

			// Fresh governor over the SAME log reconstructs by replay.
			g2, err := govern.NewFederated(ctx, m, log, entity, m.NewState())
			if err != nil {
				t.Fatal(err)
			}
			s2 := g2.State()

			if m.Of(s1, mfr).ID() != m.Of(s2, mfr).ID() || m.Of(s1, sup).ID() != m.Of(s2, sup).ID() {
				t.Fatalf("reconstruction diverged: before (%s,%s) after (%s,%s)",
					m.Of(s1, mfr).Get(mstate), m.Of(s1, sup).Get(sstate),
					m.Of(s2, mfr).Get(mstate), m.Of(s2, sup).Get(sstate))
			}
			// Authority: the supplier event was overwritten; state is (active, listed).
			if ms, ss := m.Of(s2, mfr).Get(mstate), m.Of(s2, sup).Get(sstate); ms != "active" || ss != "listed" {
				t.Fatalf("reconstructed = (%s,%s), want (active, listed)", ms, ss)
			}
			if !m.IsValid(s2) {
				t.Fatal("reconstructed state is not federally valid")
			}
		})
	}
}

// TestFederatedGovernor_OrderIndependent confirms that applying the same federated events in
// different orders (across different entities on a shared log) converges to the same state —
// the federated convergence guarantee, exercised through the durable governor.
func TestFederatedGovernor_OrderIndependent(t *testing.T) {
	ctx := context.Background()
	m, mfr, sup, mstate, sstate := buildMfrSupFederation(t)
	log := govern.NewMemEventLog()

	ga, err := govern.NewFederated(ctx, m, log, "order-A", m.NewState())
	if err != nil {
		t.Fatal(err)
	}
	ga.Apply(ctx, "manufacturer", "epub")
	ga.Apply(ctx, "supplier", "eexp")

	gb, err := govern.NewFederated(ctx, m, log, "order-B", m.NewState())
	if err != nil {
		t.Fatal(err)
	}
	gb.Apply(ctx, "supplier", "eexp")
	gb.Apply(ctx, "manufacturer", "epub")

	sa, sb := ga.State(), gb.State()
	if m.Of(sa, mfr).ID() != m.Of(sb, mfr).ID() || m.Of(sa, sup).ID() != m.Of(sb, sup).ID() {
		t.Fatalf("orderings diverged: A=(%s,%s) B=(%s,%s)",
			m.Of(sa, mfr).Get(mstate), m.Of(sa, sup).Get(sstate),
			m.Of(sb, mfr).Get(mstate), m.Of(sb, sup).Get(sstate))
	}
}

// TestFederatedGovernor_RejectsUnknown confirms an unapplicable event is neither applied nor
// written to the log (validate-before-append).
func TestFederatedGovernor_RejectsUnknown(t *testing.T) {
	ctx := context.Background()
	m, _, _, _, _ := buildMfrSupFederation(t)
	log := govern.NewMemEventLog()
	g, err := govern.NewFederated(ctx, m, log, "order-X", m.NewState())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := g.Apply(ctx, "nonesuch", "epub"); err == nil {
		t.Fatal("expected error applying event to unknown registry")
	}
	// Nothing should have been logged, so reconstruction is a clean initial state.
	entries, err := log.Events(ctx, "order-X")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("rejected event was written to the log: %v", entries)
	}
}
