package agent_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// sealing is a port-level wrapper that embeds *MemStore to inherit Lister and Leaser, and
// overrides Insert (to encrypt, tenant-scope, or audit what is stored).
type sealing struct {
	*agent.MemStore
	inserts atomic.Int64
}

func (s *sealing) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	s.inserts.Add(1)
	return s.MemStore.Insert(ctx, runID, name, data)
}

// Wrapped in a Journal, such a wrapper has every write go through its Insert.
func TestEmbeddingWrapper_JournalWritesThroughItsInsert(t *testing.T) {
	ctx := context.Background()
	w := &sealing{MemStore: agent.NewMemStore()}
	j, err := agent.NewJournal(w)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("done")), j).Run(ctx, "r", agent.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	if w.inserts.Load() == 0 {
		t.Fatal("a Journal over the wrapper wrote past its Insert")
	}
}
