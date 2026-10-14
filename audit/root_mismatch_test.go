package audit_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

// A head signed over one journal does not prove a record of another journal with the same run ID, and the error says so in a way callers can test for (audit.ErrRootMismatch).
func TestProveRecordRootMismatchIsATypedError(t *testing.T) {
	ctx := context.Background()
	journal := func(vals ...string) *agent.Journal {
		j := agenttest.MustJournal(agent.NewMemStore())
		for _, v := range vals {
			vv := v
			if _, err := j.Step(ctx, "r", vv, func(context.Context) (string, error) { return vv, nil }, agent.WithSafety(agent.Safety{ReadOnly: true})); err != nil {
				t.Fatal(err)
			}
		}
		return j
	}
	_, priv, _ := ed25519.GenerateKey(nil)
	signed, err := audit.NewTreeHead(ctx, journal("x", "y"), "r", 1000)
	if err != nil {
		t.Fatal(err)
	}
	sth := signTH(t, signed, priv)
	_, err = audit.ProveRecord(ctx, journal("x", "changed"), "r", 0, sth)
	if !errors.Is(err, audit.ErrRootMismatch) {
		t.Fatalf("ProveRecord over a changed history = %v, want audit.ErrRootMismatch", err)
	}
}
