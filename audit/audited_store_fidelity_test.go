package audit_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/storetest"
	"github.com/bide-ai/bide/audit"
)

// The anchoring wrapper meets the store requirements and hands back its inner store's record
// unchanged, so live and replay agree through a Journal on it as they do on the store itself.
func TestAuditedStore_Fidelity(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	inner := agent.NewMemStore()
	anchor := audit.NewMemAnchorLog()
	storetest.Run(t, func(t *testing.T) agent.Store {
		s, err := audit.NewAuditedStore(inner, edS(priv), anchor)
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}
