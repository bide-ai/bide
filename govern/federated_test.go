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

// TestFederatedGovernor_PartialSyncMatchesCentral proves the distributed deployment model:
// instead of one governor holding the full federated state, each registry runs on its own
// node (its own component Machine + local state) and nodes exchange only shared projections
// along tree edges. The distributed per-component states must match the durable, centralized
// FederatedGovernor — validating that the Tier-2 governor and the partial-sync protocol agree.
func TestFederatedGovernor_PartialSyncMatchesCentral(t *testing.T) {
	ctx := context.Background()
	m, mfr, sup, mstate, sstate := buildMfrSupFederation(t)

	// Centralized, durable reference.
	central, err := govern.NewFederated(ctx, m, govern.NewMemEventLog(), "order-central", m.NewState())
	if err != nil {
		t.Fatal(err)
	}
	central.Apply(ctx, "supplier", "eexp")
	central.Apply(ctx, "manufacturer", "epub")
	cs := central.State()

	// Distributed: each node has ONLY its component Machine and local state.
	mfrMachine, supMachine := m.Component(mfr), m.Component(sup)
	mfrLocal := mfrMachine.Apply(mfrMachine.NewState(), "epub")
	supLocal := supMachine.Apply(supMachine.NewState(), "eexp")

	// Parent (manufacturer) sends only its shared projection down the edge; supplier merges it.
	proj, err := m.SharedProjection(mfrLocal, mfr, sup)
	if err != nil {
		t.Fatal(err)
	}
	supLocal, err = supMachine.MergeProjection(supLocal, proj)
	if err != nil {
		t.Fatal(err)
	}

	if mfrLocal.ID() != m.Of(cs, mfr).ID() {
		t.Fatalf("manufacturer diverged: distributed %s vs central %s", mfrLocal.Get(mstate), m.Of(cs, mfr).Get(mstate))
	}
	if supLocal.ID() != m.Of(cs, sup).ID() {
		t.Fatalf("supplier diverged: distributed %s vs central %s", supLocal.Get(sstate), m.Of(cs, sup).Get(sstate))
	}
}

// TestFederatedGovernor_MultiSource drives a multi-source (DAG) federation through the durable
// governor: a door is granted only if HR says employed AND Security says cleared (a resolver
// merge). Confirms the governor supports multi-source federations and reconstructs from its log.
func TestFederatedGovernor_MultiSource(t *testing.T) {
	ctx := context.Background()

	hr := gsm.NewRegistry("hr")
	employed := hr.Bool("employed")
	hr.Event("hire").Writes(employed).Apply(func(s gsm.State) gsm.State { return s.SetBool(employed, true) }).Add()
	sec := gsm.NewRegistry("security")
	cleared := sec.Bool("cleared")
	sec.Event("grant").Writes(cleared).Apply(func(s gsm.State) gsm.State { return s.SetBool(cleared, true) }).Add()
	door := gsm.NewRegistry("door")
	access := door.Enum("access", "denied", "granted")
	id := func(srcNF, d gsm.State) gsm.State { return d } // superseded by the resolver
	m, rep, err := gsm.NewFederation("access").
		Morphism(hr, door).Shared(access).Map(id).Add().
		Morphism(sec, door).Shared(access).Map(id).Add().
		Resolve(door, func(dst gsm.State, src map[string]gsm.State) gsm.State {
			if src["hr"].GetBool(employed) && src["security"].GetBool(cleared) {
				return dst.Set(access, "granted")
			}
			return dst.Set(access, "denied")
		}).Build()
	if err != nil {
		t.Fatalf("multi-source federation build: %v\n%s", err, rep)
	}

	log := govern.NewMemEventLog()
	g, err := govern.NewFederated(ctx, m, log, "acct-1", m.NewState())
	if err != nil {
		t.Fatal(err)
	}
	g.Apply(ctx, "hr", "hire")
	if got := m.Of(g.State(), door).Get(access); got != "denied" {
		t.Fatalf("HR-only: access=%s, want denied (needs both)", got)
	}
	g.Apply(ctx, "security", "grant")
	if got := m.Of(g.State(), door).Get(access); got != "granted" {
		t.Fatalf("both sources: access=%s, want granted", got)
	}

	// Reconstruct from the same durable log.
	g2, err := govern.NewFederated(ctx, m, log, "acct-1", m.NewState())
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Of(g2.State(), door).Get(access); got != "granted" {
		t.Fatalf("reconstructed access=%s, want granted", got)
	}
}

// TestFederatedGovernor_MonotoneMesh drives a CYCLIC monotone mesh through the durable
// governor: two peers mutually cap each other's level with max-propagation. Confirms the
// governor supports monotone cyclic federations (mutual constraints) and reconstructs.
func TestFederatedGovernor_MonotoneMesh(t *testing.T) {
	ctx := context.Background()

	a := gsm.NewRegistry("A")
	reqA, sA := a.Int("req", 0, 2), a.Int("s", 0, 2)
	a.Event("reqA2").Writes(reqA).Apply(func(s gsm.State) gsm.State { return s.SetInt(reqA, 2) }).Add()
	b := gsm.NewRegistry("B")
	reqB, sB := b.Int("req", 0, 2), b.Int("s", 0, 2)
	b.Event("reqB1").Writes(reqB).Apply(func(s gsm.State) gsm.State { return s.SetInt(reqB, 1) }).Add()

	// Mutual (cyclic) constraint: each peer's s takes the max of the other's request and s.
	maxFrom := func(oReq, oS, mine gsm.Var) func(gsm.State, gsm.State) gsm.State {
		return func(srcNF, d gsm.State) gsm.State {
			mx := srcNF.GetInt(oReq)
			if v := srcNF.GetInt(oS); v > mx {
				mx = v
			}
			return d.SetInt(mine, mx)
		}
	}
	m, rep, err := gsm.NewFederation("mesh").AllowMonotoneCycles().
		Morphism(b, a).Shared(sA).Map(maxFrom(reqB, sB, sA)).Add().
		Morphism(a, b).Shared(sB).Map(maxFrom(reqA, sA, sB)).Add().
		Build()
	if err != nil {
		t.Fatalf("monotone mesh build: %v\n%s", err, rep)
	}

	log := govern.NewMemEventLog()
	g, err := govern.NewFederated(ctx, m, log, "mesh-1", m.NewState())
	if err != nil {
		t.Fatal(err)
	}
	g.Apply(ctx, "A", "reqA2")
	g.Apply(ctx, "B", "reqB1")
	// The max request (2) propagates around the cycle to both peers.
	if got := m.Of(g.State(), a).GetInt(sA); got != 2 {
		t.Fatalf("A.s=%d, want 2 (max propagates around the mesh)", got)
	}
	if got := m.Of(g.State(), b).GetInt(sB); got != 2 {
		t.Fatalf("B.s=%d, want 2", got)
	}

	g2, err := govern.NewFederated(ctx, m, log, "mesh-1", m.NewState())
	if err != nil {
		t.Fatal(err)
	}
	if m.Of(g2.State(), a).GetInt(sA) != 2 || m.Of(g2.State(), b).GetInt(sB) != 2 {
		t.Fatal("reconstructed mesh state diverged from original")
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
