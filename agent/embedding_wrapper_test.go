package agent_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// sealing is a port-level wrapper that embeds *MemStore to inherit Lister and Leaser, and
// overrides Insert (to encrypt, tenant-scope, or audit what is stored). It also inherits MemStore's
// transitional Do and History, which write through a Journal over the MemStore, past its Insert.
type sealing struct {
	*agent.MemStore
	inserts atomic.Int64
}

func (s *sealing) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	s.inserts.Add(1)
	return s.MemStore.Insert(ctx, runID, name, data)
}

// Such a wrapper, passed where a Durable goes, is refused with ErrConfig before anything is
// written, rather than have every write bypass its Insert. Wrapped in a Journal, it works.
func TestEmbeddingWrapper_WithAnInheritedShimIsRefused(t *testing.T) {
	ctx := context.Background()
	w := &sealing{MemStore: agent.NewMemStore()}
	_, err := agent.New(agent.NewScriptedModel(agent.TextTurn("done")), w).Run(ctx, "r", "hi")
	if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "NewJournal") {
		t.Fatalf("Run through the wrapper = %v; want ErrConfig pointing at NewJournal", err)
	}
	if _, err := agent.Step(ctx, w, "r", "s", func(context.Context) (int, error) { return 1, nil }); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Step through the wrapper = %v; want ErrConfig", err)
	}
	if recs, _ := w.MemStore.History(ctx, "r"); len(recs) != 0 {
		t.Fatalf("the refused wrapper's store holds %d records", len(recs))
	}

	j, err := agent.NewJournal(w)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.New(agent.NewScriptedModel(agent.TextTurn("done")), j).Run(ctx, "r", "hi"); err != nil {
		t.Fatal(err)
	}
	if w.inserts.Load() == 0 {
		t.Fatal("a Journal over the wrapper wrote past its Insert")
	}
}

// A wrapper that embeds a store and intercepts Do itself (the crash-injecting wrappers of the
// engine's tests) is driven through its Do, as before, even if it declares a Store method too.
type interceptsDo struct {
	*agent.MemStore
	dos atomic.Int64
}

func (s *interceptsDo) Do(ctx context.Context, runID, name string, fn func(context.Context) (agent.Record, error)) (agent.Record, error) {
	s.dos.Add(1)
	return s.MemStore.Do(ctx, runID, name, fn)
}

func (s *interceptsDo) Get(ctx context.Context, runID, name string) (agent.Entry, bool, error) {
	return s.MemStore.Get(ctx, runID, name)
}

func TestEmbeddingWrapper_ThatInterceptsDoIsDrivenThroughIt(t *testing.T) {
	w := &interceptsDo{MemStore: agent.NewMemStore()}
	if _, err := agent.New(agent.NewScriptedModel(agent.TextTurn("done")), w).Run(context.Background(), "r", "hi"); err != nil {
		t.Fatal(err)
	}
	if w.dos.Load() == 0 {
		t.Fatal("the run bypassed the wrapper's Do")
	}
}
