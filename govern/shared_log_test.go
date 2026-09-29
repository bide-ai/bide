package govern

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
	gsm "github.com/blackwell-systems/gsm"
)

// Two PersistentGovernors sharing one EventLog stand in for two processes sharing a durable log
// (Postgres, SQLite, Redis), the high-availability setup the log adapters exist for. Each governed
// action commits a state_digest, and an auditor checks those digests by replaying the shared log
// in order (the verifier side of TestAttestedEventTool_StateDigestsReplay). Every committed digest
// must match that replay at the position the action recorded, and after Sync every governor's state
// must be the shared state.
func TestPersistentGovernor_SharedLogStaysConsistent(t *testing.T) {
	ctx := context.Background()
	build := func() *gsm.Machine {
		r := gsm.NewRegistry("cap")
		a := r.Int("a", 0, 5)
		r.DeclInvariant("a_cap", gsm.Le(gsm.V(a), gsm.Lit(3)), gsm.Do(gsm.Set(a, gsm.Lit(3))))
		r.DeclEvent("inc_a", gsm.Do(gsm.Set(a, gsm.Add(gsm.V(a), gsm.Lit(1)))))
		m, rep, err := r.Build()
		if err != nil {
			t.Fatalf("build: %v\n%s", err, rep)
		}
		return m
	}

	shared := NewMemEventLog()
	mA, mB := build(), build()
	govA, err := NewPersistent(ctx, mA, shared, "acct", mA.NewState())
	if err != nil {
		t.Fatal(err)
	}
	govB, err := NewPersistent(ctx, mB, shared, "acct", mB.NewState())
	if err != nil {
		t.Fatal(err)
	}
	type commit struct {
		digest   string
		position int64
	}
	act := func(gov Applier) commit {
		t.Helper()
		res, err := AttestedEventTool(gov, "inc_a", "increment", "inc_a", "policy", agent.Safety{}).Call(ctx, json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(res, &out); err != nil {
			t.Fatal(err)
		}
		return commit{out["state_digest"].(string), int64(out["position"].(float64))}
	}

	// The two processes interleave their actions on the shared log.
	committed := []commit{act(govA), act(govB), act(govA)}

	// The auditor replays the shared log with a reference build of the policy.
	events, err := shared.Events(ctx, "acct", 0)
	if err != nil {
		t.Fatal(err)
	}
	ref := build()
	st := ref.NewState()
	replayed := make([]string, len(events)) // the state digest after each log position
	for i, ev := range events {
		st = ref.Apply(st, ev)
		replayed[i] = st.Digest()
	}
	for i, c := range committed {
		if c.position < 0 || c.position >= int64(len(replayed)) {
			t.Fatalf("action %d recorded position %d, outside the shared log (%d events)", i, c.position, len(replayed))
		}
		if c.digest != replayed[c.position] {
			t.Errorf("action %d committed state %s, but replaying the shared log through position %d gives %s", i, c.digest, c.position, replayed[c.position])
		}
	}
	for name, gov := range map[string]*PersistentGovernor{"A": govA, "B": govB} {
		got, err := gov.Sync(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.Digest() != st.Digest() {
			t.Errorf("governor %s syncs to state %s, but the shared log replays to %s", name, got.Digest(), st.Digest())
		}
	}
}
