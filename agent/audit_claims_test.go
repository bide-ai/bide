package agent_test

// Audit's reading of the claim bookkeeping kinds (review of the claim protocol): a journal holding
// not-started and claim-held records proves and verifies like any other; neither kind is taken for
// a completed call or step; a call voided by a not-started record is provably absent from the
// tool-use key set, and a call that fired under a held claim is provably present.

import (
	"context"
	"crypto/ed25519"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

func TestAuditReadsClaimBookkeeping(t *testing.T) {
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(nil)
	m := agent.NewMemStore()
	fired := 0
	charge := agent.Func("charge", "", agent.Safety{}, func(context.Context, struct{}) (string, error) { fired++; return "ok", nil })
	model := func() agent.Model {
		return agent.NewScriptedModel(agent.ToolTurn("c1", "charge", `{}`), agent.ToolTurn("c2", "charge", `{}`), agent.TextTurn("done"))
	}
	// c1: marker commits and errors, then not-started is recorded (voided); re-attempted under
	// attempt:retry:1. c2: claim taken back and held.
	s := &r3Store{m: m, faults: []r3Fault{{"attempt:tool:c1", "c"}}}
	j, _ := agent.NewJournal(s)
	_, err := agent.New(model(), j, charge).SetMaxConcurrency(1).Run(ctx, "r", "hi")
	t.Logf("drive 1: %v", err)
	s.faults = []r3Fault{{"attempt:tool:c2", "nc"}, {"attempt:not-started:", "nc"}}
	_, err = agent.New(model(), j, charge).SetMaxConcurrency(1).Run(ctx, "r", "hi")
	t.Logf("drive 2: %v", err)
	_, err = agent.New(model(), j, charge).SetMaxConcurrency(1).Run(ctx, "r", "hi")
	t.Logf("drive 3: %v; fired %d", err, fired)
	r3Dump(t, m, "r")
	if fired != 2 {
		t.Fatalf("fired %d, want 2", fired)
	}

	pkg, err := audit.Evidence(ctx, m, "r", priv, 1, audit.WithAllToolCalls())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range pkg.Actions {
		t.Logf("action: %s %s (record %q kind %s)", a.Kind, a.Label, a.Bundle.Record.Name, a.Bundle.Record.Kind)
		if k := a.Bundle.Record.Kind; k == agent.StepNotStarted {
			t.Fatalf("bookkeeping record %q offered as evidence of an action", a.Bundle.Record.Name)
		}
	}
	if len(pkg.Actions) != 2 {
		t.Fatalf("%d actions, want the two completed calls", len(pkg.Actions))
	}
	rep, err := pkg.Verify(pub)
	if err != nil || !rep.OK {
		t.Fatalf("verify: %v %+v", err, rep)
	}
	// Every leaf, bookkeeping included, proves against the root.
	hist, _ := m.History(ctx, "r")
	root, err := audit.Root(ctx, m, "r")
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range hist {
		p, err := audit.Prove(ctx, m, "r", i)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := audit.VerifyInclusion(root, r, p); !ok || err != nil {
			t.Fatalf("leaf %d (%s) does not verify: %v", i, r.Name, err)
		}
	}
	// Tool-use key set: completed calls only.
	for _, key := range []string{"tooluse:c1", "tooluse:c2"} {
		if _, err := audit.ProveAbsent(hist, audit.ToolUseKeys, key); err == nil {
			t.Fatalf("%s proven absent, but the call completed", key)
		}
	}
	if _, err := audit.ProveAbsent(hist, audit.ToolUseKeys, "tooluse:c9"); err != nil {
		t.Fatalf("absence of a call never made: %v", err)
	}
}
