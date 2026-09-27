package govern

import (
	"context"
	"encoding/json"
	"testing"

	agent "github.com/blackwell-systems/bide"
	gsm "github.com/blackwell-systems/gsm"
)

// TestAttestedEventTool_StateDigestsReplay is the payoff for the single-leaf (action, policy,
// state) binding: a verifier holding the policy and the run's governed events can rebuild the
// reference machine, replay the events from the initial state, and confirm each committed
// state_digest matches the reference at that transition. This checks the runtime's state against
// the verified reference per action, for this run (it does not prove refinement for all inputs).
func TestAttestedEventTool_StateDigestsReplay(t *testing.T) {
	ctx := context.Background()

	build := func() (*gsm.Machine, gsm.Var, gsm.Var) {
		r := gsm.NewRegistry("cap")
		a := r.Int("a", 0, 5)
		b := r.Int("b", 0, 5)
		r.DeclInvariant("a_cap", gsm.Le(gsm.V(a), gsm.Lit(3)), gsm.Do(gsm.Set(a, gsm.Lit(3))))
		r.DeclEvent("inc_a", gsm.Do(gsm.Set(a, gsm.Add(gsm.V(a), gsm.Lit(1)))))
		r.DeclEvent("inc_b", gsm.Do(gsm.Set(b, gsm.Add(gsm.V(b), gsm.Lit(1)))))
		m, rep, err := r.Build()
		if err != nil {
			t.Fatalf("build: %v\n%s", err, rep)
		}
		return m, a, b
	}

	// Production side: a governor drives real transitions; each tool call records a state_digest.
	m, _, _ := build()
	digest := "policy-x" // opaque; the digest value is not what this test checks
	gov := New(m, m.NewState())

	events := []string{"inc_a", "inc_b", "inc_a", "inc_a", "inc_a"} // a is capped at 3
	committed := make([]string, 0, len(events))
	for i, ev := range events {
		tool := AttestedEventTool(gov, ev, ev, ev, digest, agent.Safety{})
		res, err := tool.Call(ctx, json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		var out map[string]any
		if err := json.Unmarshal(res, &out); err != nil {
			t.Fatalf("unmarshal %d: %v", i, err)
		}
		committed = append(committed, out["state_digest"].(string))
	}

	// Verifier side: an independent reference build of the same policy replays the same events
	// and must reproduce every committed state_digest.
	ref, _, _ := build()
	st := ref.NewState()
	for i, ev := range events {
		st = ref.Apply(st, ev)
		if st.Digest() != committed[i] {
			t.Fatalf("replay diverged at step %d (%s): committed %s, reference %s", i, ev, committed[i], st.Digest())
		}
	}
}
