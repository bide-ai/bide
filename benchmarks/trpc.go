// Package benchmarks holds chaos-benchmark adapters for OTHER agent SDKs. It lives in its
// own module so those SDKs' dependency trees never touch the Bide core.
package benchmarks

import (
	"context"
	"sync"

	"github.com/bide-ai/bide/chaos"
	"trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/graph"
	"trpc.group/trpc-go/trpc-agent-go/graph/checkpoint/inmemory"
)

// dropSaver wraps a persistent checkpoint saver but LOSES every persisting write from
// crashAt onward — the faithful model of a process crash at that point: trpc treats a
// checkpoint save error as non-fatal and keeps running in-memory, so a real crash means
// "everything written after the crash point never persisted." Reads pass through to the
// surviving (pre-crash) checkpoints.
type dropSaver struct {
	inner   graph.CheckpointSaver
	mu      sync.Mutex
	writes  int
	crashAt int
	dropped bool
}

func (s *dropSaver) drop() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if s.crashAt > 0 && s.writes >= s.crashAt {
		s.dropped = true
		return true
	}
	return false
}

func (s *dropSaver) Get(ctx context.Context, cfg map[string]any) (*graph.Checkpoint, error) {
	return s.inner.Get(ctx, cfg)
}
func (s *dropSaver) GetTuple(ctx context.Context, cfg map[string]any) (*graph.CheckpointTuple, error) {
	return s.inner.GetTuple(ctx, cfg)
}
func (s *dropSaver) List(ctx context.Context, cfg map[string]any, f *graph.CheckpointFilter) ([]*graph.CheckpointTuple, error) {
	return s.inner.List(ctx, cfg, f)
}
func (s *dropSaver) Put(ctx context.Context, req graph.PutRequest) (map[string]any, error) {
	if s.drop() {
		return req.Config, nil // pretend success; do NOT persist (lost to the crash)
	}
	return s.inner.Put(ctx, req)
}
func (s *dropSaver) PutFull(ctx context.Context, req graph.PutFullRequest) (map[string]any, error) {
	if s.drop() {
		return req.Config, nil
	}
	return s.inner.PutFull(ctx, req)
}
func (s *dropSaver) PutWrites(ctx context.Context, req graph.PutWritesRequest) error {
	if s.drop() {
		return nil
	}
	return s.inner.PutWrites(ctx, req)
}
func (s *dropSaver) DeleteLineage(ctx context.Context, lineageID string) error {
	return s.inner.DeleteLineage(ctx, lineageID)
}
func (s *dropSaver) Close() error { return nil }

// buildChargeGraph: start → charge (non-idempotent: increments *fired) → finish.
func buildChargeGraph(fired *int) (*graph.Graph, error) {
	return graph.NewStateGraph(graph.NewStateSchema()).
		AddNode("start", func(ctx context.Context, s graph.State) (any, error) { return nil, nil }).
		AddNode("charge", func(ctx context.Context, s graph.State) (any, error) {
			*fired++ // the non-idempotent side effect
			return nil, nil
		}).
		AddEdge("start", "charge").
		SetEntryPoint("start").
		SetFinishPoint("charge").
		Compile()
}

// TRPC returns a chaos.System for trpc-agent-go's graph executor with checkpoint/resume.
func TRPC() chaos.System { return trpcSys{} }

type trpcSys struct{}

func (trpcSys) Writes() int { return 6 }

func (trpcSys) NewRun() chaos.Run {
	return &trpcRun{store: inmemory.NewSaver(), fired: new(int)}
}

type trpcRun struct {
	store graph.CheckpointSaver // the persistent store, shared across steps (survives a crash)
	fired *int
}

func (r *trpcRun) Fired() int { return *r.fired }

func (r *trpcRun) Step(crashAt int) bool {
	g, err := buildChargeGraph(r.fired)
	if err != nil {
		return false
	}
	ds := &dropSaver{inner: r.store, crashAt: crashAt}
	exec, err := graph.NewExecutor(g, graph.WithCheckpointSaver(ds))
	if err != nil {
		return false
	}
	// Same lineage each Step → the executor resumes from the latest surviving checkpoint.
	init := graph.State{graph.CfgKeyLineageID: "chaos", graph.CfgKeyCheckpointNS: "ns"}
	ch, err := exec.Execute(context.Background(), init, &agent.Invocation{InvocationID: "chaos"})
	if err != nil {
		return false
	}
	for range ch { // drain to completion
	}
	return ds.dropped // "crashed" = a checkpoint was lost; call Step again to resume
}
