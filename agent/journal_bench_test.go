package agent_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// The journal benchmarks the redesign's performance gates compare base against head with (see
// docs: bench workflow). They use only API present on both sides of the change, so the same file
// runs against either.

// BenchmarkRunTurns drives fresh runs of five model turns, each calling a retry-safe tool, on a
// MemStore: the journal's cost per live turn.
func BenchmarkRunTurns(b *testing.B) {
	ctx := context.Background()
	turns := make([]agent.ScriptedTurn, 0, 6)
	for i := range 5 {
		turns = append(turns, agent.ToolTurn(fmt.Sprintf("c%d", i), "lookup", `{}`))
	}
	turns = append(turns, agent.TextTurn("done"))
	tool := agent.MustFunc("lookup", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}))
	store := agenttest.MemJournal()
	a := agenttest.MustNew(agent.NewScriptedModel(turns...), store, agent.WithTools(tool))
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		if _, err := a.Run(ctx, fmt.Sprintf("run-%d", i), agent.UserText("hi")); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkToolCallSideEffect drives fresh runs whose one tool call is a side effect: its claim,
// its result, and the turns around it.
func BenchmarkToolCallSideEffect(b *testing.B) {
	ctx := context.Background()
	tool := agent.MustFunc("charge", "", func(context.Context, struct{}) (string, error) { return "ok", nil })
	store := agenttest.MemJournal()
	a := agenttest.MustNew(
		agent.NewScriptedModel(agent.ToolTurn("c1", "charge", `{}`), agent.TextTurn("done")),
		store,
		agent.WithTools(tool),
	)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		if _, err := a.Run(ctx, fmt.Sprintf("run-%d", i), agent.UserText("hi")); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStep runs one side-effect Step and one retry-safe Step per iteration in one run, each
// new, and one recorded Step read back.
func BenchmarkStep(b *testing.B) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	fn := func(context.Context) (int, error) { return 1, nil }
	safe := agent.WithSafety(agent.Safety{ReadOnly: true})
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		if _, err := store.Step(ctx, "run", fmt.Sprintf("w%d", i), fn); err != nil {
			b.Fatal(err)
		}
		if _, err := store.Step(ctx, "run", fmt.Sprintf("r%d", i), fn, safe); err != nil {
			b.Fatal(err)
		}
		if _, err := store.Step(ctx, "run", fmt.Sprintf("w%d", i), fn); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRecoverPass10k is one recovery pass over 10,000 runs, of which 1,000 are unfinished.
func BenchmarkRecoverPass10k(b *testing.B) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	value := func(context.Context) (agent.Record, error) { return agent.Record{Kind: agent.StepValue}, nil }
	start := func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: []byte(`{"input":"go"}`)}, nil // recovery reads it (P14)
	}
	for i := range 10_000 {
		id := fmt.Sprintf("run-%05d", i)
		if _, err := journaltest.Do(ctx, store, id, "run:start", start); err != nil {
			b.Fatal(err)
		}
		if i%10 != 0 {
			if _, err := journaltest.Do(ctx, store, id, "run:complete", value); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		n, err := agent.Recover(ctx, store, func(context.Context, string, agent.RunStart) error { return nil }, agent.WithLeaseHolder("w"))
		if err != nil || n != 1000 {
			b.Fatalf("Recover = %d, %v", n, err)
		}
	}
}
