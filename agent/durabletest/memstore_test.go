package durabletest_test

import (
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/durabletest"
)

// MemStore hands back the record a replay reads, on the live path too.
func TestMemStore_Fidelity(t *testing.T) {
	durabletest.Run(t, func(*testing.T) agent.Durable { return agent.NewMemStore() })
}
