package audit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

func mustFailRun(t *testing.T, what string, cert audit.RunCertificate, approved []string, pub ed25519.PublicKey) {
	t.Helper()
	res, err := audit.VerifyRun(cert, approved, pub)
	if err == nil && res.OK {
		t.Fatalf("%s: the certificate verifies", what)
	}
}

// plainRun journals n value steps (no tool calls, so no policies) in runID and returns the records
// and the journal head.
func plainRun(t *testing.T, runID string, vals ...string) ([]agent.Record, audit.TreeHead) {
	t.Helper()
	ctx := context.Background()
	s := agent.NewMemStore()
	for i, v := range vals {
		v := v
		if _, err := s.Do(ctx, runID, "s"+string(rune('a'+i)), func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`"` + v + `"`)}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	recs, _ := s.History(ctx, runID)
	th, err := audit.NewTreeHead(ctx, s, runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	return recs, th
}

// TestVerifyRun_UsedPolicyHeadIsBoundToRunAndJournal: a used-policy head signed by the right key
// still fails unless it is the used-policy set of this run's certified journal tree.
func TestVerifyRun_UsedPolicyHeadIsBoundToRunAndJournal(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)

	// Runs X and Y have byte-identical journals, so identical roots; only the run ID tells them apart.
	recsX, thX := plainRun(t, "X", "a")
	recsY, thY := plainRun(t, "Y", "a")
	absX, err := audit.SignAbsenceRoot(recsX, audit.PolicyUsedKeys, thX, priv, 2)
	if err != nil {
		t.Fatal(err)
	}
	absY, err := audit.SignAbsenceRoot(recsY, audit.PolicyUsedKeys, thY, priv, 2)
	if err != nil {
		t.Fatal(err)
	}
	props := []string{"only-approved-policies", "policies-convergence-certified"}
	genuine := audit.RunCertificate{Format: audit.RunCertificateFormat, RunID: "X", Properties: props, UsedPolicies: []string{}, UsedPolicyAbsence: absX, STH: audit.SignTreeHead(thX, priv)}
	if res, _ := audit.VerifyRun(genuine, nil, pub); !res.OK {
		t.Fatalf("the genuine certificate of a run that used no policy does not verify: %+v", res)
	}

	c := genuine
	c.UsedPolicyAbsence = absY
	mustFailRun(t, "run Y's used-policy head for run X", c, nil, pub)

	c = genuine
	c.STH = audit.SignTreeHead(thY, priv)
	mustFailRun(t, "run Y's journal head for run X", c, nil, pub)
	// The used-policy set is bound to the journal head, so it is not established either.
	if res, _ := audit.VerifyRun(c, nil, pub); res.OnlyApprovedPolicies {
		t.Fatal("only-approved-policies held against another run's journal head")
	}

	// The tool-use set of a run with no tool calls is empty too, but it is not the used-policy set.
	toolX, err := audit.SignAbsenceRoot(recsX, audit.ToolUseKeys, thX, priv, 2)
	if err != nil {
		t.Fatal(err)
	}
	c = genuine
	c.UsedPolicyAbsence = toolX
	mustFailRun(t, "run X's tool-use head as its used-policy head", c, nil, pub)

	// A different history of run X of the same length: the used-policy head names its journal root.
	recsX2, thX2 := plainRun(t, "X", "b")
	absX2, err := audit.SignAbsenceRoot(recsX2, audit.PolicyUsedKeys, thX2, priv, 2)
	if err != nil {
		t.Fatal(err)
	}
	c = genuine
	c.UsedPolicyAbsence = absX2
	mustFailRun(t, "a used-policy head of another history of run X", c, nil, pub)
}
