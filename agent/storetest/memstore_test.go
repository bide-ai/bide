package storetest_test

import (
	"context"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/agent/storetest"
)

// MemStore meets every store requirement; its handles are one store.
func TestMemStore(t *testing.T) {
	m := agent.NewMemStore()
	storetest.Run(t, func(*testing.T) agent.Store { return m })
}

// The counting wrapper passes keys through unchanged, whatever the context, and may Unwrap.
func TestCountingStore_IsAWellBehavedWrapper(t *testing.T) {
	type tenant struct{}
	a := context.WithValue(context.Background(), tenant{}, "A")
	b := context.WithValue(context.Background(), tenant{}, "B")
	storetest.CheckWrapper(t, func(s agent.Store) agent.Store { return agenttest.NewCountingStore(s) }, a, b)
}
