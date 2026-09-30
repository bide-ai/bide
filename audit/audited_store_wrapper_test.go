package audit_test

import (
	"context"
	"crypto/ed25519"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/storetest"
	"github.com/bide-ai/bide/audit"
)

type tenantKey struct{}

// AuditedStore implements Unwrap() Durable, so it must pass run IDs and names through unchanged,
// whatever the context (see agent.Durable).
func TestAuditedStore_KeepsTheDurableWrapperContract(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	a := context.WithValue(context.Background(), tenantKey{}, "A")
	b := context.WithValue(context.Background(), tenantKey{}, "B")
	storetest.CheckDurableWrapper(t, func(d agent.Durable) agent.Durable {
		s, err := audit.NewAuditedStore(d, audit.Ed25519Signer{Priv: priv}, audit.NewMemAnchorLog())
		if err != nil {
			t.Fatal(err)
		}
		return s
	}, a, b)
}
