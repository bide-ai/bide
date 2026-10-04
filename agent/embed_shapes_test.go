package agent_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// sealingV is the sealing wrapper with Insert declared on the VALUE receiver. Passed by pointer
// (the usual way), (*sealingV).Insert is a compiler-generated wrapper.
type sealingV struct {
	*agent.MemStore
	inserts *atomic.Int64
}

func (s sealingV) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	s.inserts.Add(1)
	return s.MemStore.Insert(ctx, runID, name, data)
}

// sealingG is the same wrapper as a generic type, pointer receiver.
type sealingG[T any] struct {
	*agent.MemStore
	inserts atomic.Int64
	_       T
}

func (s *sealingG[T]) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	s.inserts.Add(1)
	return s.MemStore.Insert(ctx, runID, name, data)
}

// nested embeds the sealing wrapper, declaring nothing itself: its Insert comes from sealing.
type nested struct{ *sealing }

// A Journal over a wrapper that embeds a store and overrides Insert writes through that Insert,
// whatever the wrapper's shape: a value receiver, a generic type, or a wrapper nesting one.
func TestEmbeddingWrapper_ShapesWriteThroughTheirInsert(t *testing.T) {
	v := &sealingV{MemStore: agent.NewMemStore(), inserts: new(atomic.Int64)}
	g := &sealingG[int]{MemStore: agent.NewMemStore()}
	n := &nested{&sealing{MemStore: agent.NewMemStore()}}
	for name, tc := range map[string]struct {
		store   agent.Store
		inserts func() int64
	}{
		"value receiver by pointer": {v, v.inserts.Load},
		"generic":                   {g, g.inserts.Load},
		"nested":                    {n, n.inserts.Load},
	} {
		_, err := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("done")), agenttest.MustJournal(tc.store)).Run(context.Background(), "r", agent.UserText("hi"))
		if err != nil || tc.inserts() == 0 {
			t.Errorf("%s: Run = %v with %d wrapper Inserts; want nil and every write through the wrapper's Insert", name, err, tc.inserts())
		}
	}
}

// Stores and the plain wrappers of the engine's tests run under a Journal.
func TestEmbeddingWrapper_PlainShapesAreAccepted(t *testing.T) {
	type countsRuns struct{ *agent.MemStore }
	for name, s := range map[string]agent.Store{
		"MemStore":        agent.NewMemStore(),
		"embeds MemStore": &countsRuns{agent.NewMemStore()},
	} {
		if _, err := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("done")), agenttest.MustJournal(s)).Run(context.Background(), "r", agent.UserText("hi")); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
