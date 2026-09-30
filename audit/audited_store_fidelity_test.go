package audit_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/durabletest"
	"github.com/bide-ai/bide/audit"
)

// The anchoring wrapper hands back its inner store's record unchanged, so live and replay agree
// through it as they do on the store itself.
func TestAuditedStore_Fidelity(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	durabletest.Run(t, func(*testing.T) agent.Durable {
		return mustAuditedStore(t, agent.NewMemStore(), priv, audit.NewMemAnchorLog())
	})
}
