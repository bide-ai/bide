package chaos

import (
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/durabletest"
)

// The crash-injecting wrapper, when it does not crash, hands back its inner store's record
// unchanged, so live and replay agree through it as they do on the store itself.
func TestCrashStore_Fidelity(t *testing.T) {
	durabletest.Run(t, func(*testing.T) agent.Durable { return &crashStore{inner: agent.NewMemStore()} })
}
